package process

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/state"
)

type Status int

const (
	StatusStopped Status = iota
	StatusWaiting
	StatusStarting
	StatusRunning
	StatusCrashed
	StatusRestarting
)

func (s Status) String() string {
	switch s {
	case StatusStopped:
		return "stopped"
	case StatusWaiting:
		return "waiting"
	case StatusStarting:
		return "starting"
	case StatusRunning:
		return "running"
	case StatusCrashed:
		return "crashed"
	case StatusRestarting:
		return "restarting"
	default:
		return "unknown"
	}
}

const ringBufferSize = 1000

// RingBuffer is a fixed-size circular buffer for log lines.
type RingBuffer struct {
	lines []string
	head  int
	count int
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		lines: make([]string, size),
	}
}

func (rb *RingBuffer) Add(line string) {
	rb.lines[rb.head] = line
	rb.head = (rb.head + 1) % len(rb.lines)
	if rb.count < len(rb.lines) {
		rb.count++
	}
}

func (rb *RingBuffer) Lines() []string {
	if rb.count == 0 {
		return nil
	}
	result := make([]string, rb.count)
	start := (rb.head - rb.count + len(rb.lines)) % len(rb.lines)
	for i := 0; i < rb.count; i++ {
		result[i] = rb.lines[(start+i)%len(rb.lines)]
	}
	return result
}

// Service represents a single managed subprocess.
type Service struct {
	Config  config.Service
	Status  Status
	PID     int
	PGID    int
	Branch  string
	Logs    *RingBuffer
	Healthy bool
	RestartCount int // number of auto-restarts since last manual start/restart

	// LogFile is the absolute path of the service's stdout/stderr log.
	LogFile string

	// Adopted is true when this Service was attached to a pre-existing
	// orphaned process (e.g. after a pairin crash). Adopted services do not
	// have an *exec.Cmd we can Wait on; liveness is polled instead and they
	// cannot be restarted in phase 1.
	Adopted bool

	cmd          *exec.Cmd
	generation   int
	healthCancel context.CancelFunc
	tailCancel   context.CancelFunc
	mu           sync.Mutex
}

// GetLines returns a copy of the log lines (thread-safe).
func (s *Service) GetLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Logs.Lines()
}

// NewMirrorService creates a Service stub for client-side use. It has no
// exec.Cmd and no Manager-owned goroutines; fields are mutated by the control
// client as events arrive. The TUI treats it exactly like a live Service.
func NewMirrorService(name, short, color, dir, cmd string, dependsOn []string, hasHealth bool, maxRestarts int) *Service {
	svcCfg := config.Service{
		Name:        name,
		Short:       short,
		Color:       color,
		Dir:         dir,
		Cmd:         cmd,
		DependsOn:   dependsOn,
		MaxRestarts: maxRestarts,
	}
	if hasHealth {
		// A non-empty marker; the real probe runs in the supervisor. The TUI
		// only needs to know whether to render a health indicator.
		svcCfg.Healthcheck = "remote"
	}
	return &Service{
		Config: svcCfg,
		Status: StatusStopped,
		Logs:   NewRingBuffer(ringBufferSize),
	}
}

// Sink receives tea.Msg events from the Manager. *tea.Program satisfies this
// for in-process delivery; a socket broadcaster satisfies it when the Manager
// runs in a supervisor with remote TUI clients.
type Sink interface {
	Send(tea.Msg)
}

// Manager orchestrates all services.
type Manager struct {
	Services   []*Service
	configPath string
	nameToIdx  map[string]int
	sink       Sink
	mu         sync.Mutex
	err        error
	quitting   atomic.Bool
}

func NewManager(cfg *config.Config) *Manager {
	services := make([]*Service, len(cfg.Services))
	nameToIdx := make(map[string]int, len(cfg.Services))
	for i, sc := range cfg.Services {
		services[i] = &Service{
			Config:  sc,
			Status:  StatusStopped,
			Logs:    NewRingBuffer(ringBufferSize),
			LogFile: state.LogFilePath(cfg.Path, sc.Name),
		}
		nameToIdx[sc.Name] = i
	}
	return &Manager{
		Services:   services,
		configPath: cfg.Path,
		nameToIdx:  nameToIdx,
	}
}

// ConfigPath returns the path to the .pairinrc.toml the manager was built from.
func (m *Manager) ConfigPath() string { return m.configPath }

// ServiceList returns the managed services. Used by the TUI via a shared
// interface with the control.Client mirror.
func (m *Manager) ServiceList() []*Service { return m.Services }

// StartService starts a single service by index. Exposed for the control server.
func (m *Manager) StartService(idx int) {
	if idx < 0 || idx >= len(m.Services) {
		return
	}
	m.startService(idx)
}

// StopService stops a single service by index. Exposed for the control server.
func (m *Manager) StopService(idx int) {
	if idx < 0 || idx >= len(m.Services) {
		return
	}
	m.stopService(idx)
}

// SetSink installs the event sink (tea.Program or socket broadcaster).
func (m *Manager) SetSink(s Sink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = s
}

// SetProgram is a compatibility alias for SetSink. *tea.Program satisfies Sink.
func (m *Manager) SetProgram(p *tea.Program) {
	m.SetSink(p)
}

func (m *Manager) Error() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *Manager) send(msg tea.Msg) {
	m.mu.Lock()
	s := m.sink
	m.mu.Unlock()
	if s != nil {
		s.Send(msg)
	}
}

// StartAll launches all services. Services with unmet dependencies enter
// StatusWaiting and are started automatically once their deps become healthy.
// Already-adopted services are left running; they just get a status broadcast
// so the TUI renders them.
func (m *Manager) StartAll() tea.Cmd {
	return func() tea.Msg {
		for i, svc := range m.Services {
			svc.mu.Lock()
			adopted := svc.Adopted
			svc.mu.Unlock()
			if adopted {
				m.send(StatusMsg{Index: i, Status: StatusRunning, PID: svc.PID})
				continue
			}
			if len(svc.Config.DependsOn) == 0 || m.allDepsHealthy(i) {
				m.startService(i)
			} else {
				svc.mu.Lock()
				svc.Status = StatusWaiting
				deps := strings.Join(svc.Config.DependsOn, ", ")
				svc.Logs.Add(fmt.Sprintf("[pairin] waiting for dependencies: %s", deps))
				svc.mu.Unlock()
				m.send(StatusMsg{Index: i, Status: StatusWaiting})
			}
		}
		return AllStartedMsg{}
	}
}

// AdoptService registers an orphaned process (from a previous pairin) as a
// live service. No exec.Cmd is created; liveness is polled, stop works via
// PGID signal, restart is not supported in phase 1.
func (m *Manager) AdoptService(idx int, pid, pgid int, logFile string) {
	svc := m.Services[idx]
	svc.mu.Lock()
	svc.generation++
	svc.Adopted = true
	svc.PID = pid
	svc.PGID = pgid
	if logFile != "" {
		svc.LogFile = logFile
	}
	svc.Branch = detectBranch(svc.Config.Dir)
	svc.Status = StatusRunning
	svc.Logs.Add(fmt.Sprintf("[pairin] adopted existing process PID %d (from previous session)", pid))

	m.startTailer(idx)
	if svc.Config.Healthcheck != "" {
		m.startHealthcheckPoller(idx)
	}
	gen := svc.generation
	svc.mu.Unlock()

	m.send(StatusMsg{Index: idx, Status: StatusRunning, PID: pid})

	go m.watchAdopted(idx, pid, gen)
	go m.persistState()
}

// watchAdopted polls an adopted process's liveness. When it disappears, the
// service is marked Stopped (we can't know the exit code from outside).
func (m *Manager) watchAdopted(idx, pid, gen int) {
	svc := m.Services[idx]
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if m.quitting.Load() {
			return
		}
		svc.mu.Lock()
		if svc.generation != gen {
			svc.mu.Unlock()
			return
		}
		svc.mu.Unlock()

		if !state.IsProcessAlive(pid) {
			svc.mu.Lock()
			if svc.generation != gen {
				svc.mu.Unlock()
				return
			}
			svc.Status = StatusStopped
			svc.Logs.Add("[pairin] adopted process exited")
			svc.PID = 0
			svc.PGID = 0
			svc.Adopted = false
			if svc.tailCancel != nil {
				svc.tailCancel()
				svc.tailCancel = nil
			}
			svc.mu.Unlock()
			m.send(StatusMsg{Index: idx, Status: StatusStopped})
			go m.persistState()
			return
		}
	}
}

// persistState writes the current running-services snapshot to state.json.
// Safe to call from any goroutine; serialized by its own write mutex.
func (m *Manager) persistState() {
	if m.configPath == "" {
		return
	}
	snap := &state.State{ConfigPath: m.configPath}
	for _, svc := range m.Services {
		svc.mu.Lock()
		if svc.PID > 0 && (svc.Status == StatusRunning || svc.Status == StatusStarting) {
			snap.Services = append(snap.Services, state.ServiceState{
				Name:      svc.Config.Name,
				PID:       svc.PID,
				PGID:      svc.PGID,
				LogFile:   svc.LogFile,
				Cmd:       svc.Config.Cmd,
				Dir:       svc.Config.Dir,
				StartedAt: time.Now(),
			})
		}
		svc.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = state.Save(m.configPath, snap)
}

func (m *Manager) startService(idx int) {
	svc := m.Services[idx]
	svc.mu.Lock()
	defer svc.mu.Unlock()

	svc.generation++
	svc.Adopted = false

	// Detect git branch
	svc.Branch = detectBranch(svc.Config.Dir)

	svc.Status = StatusStarting
	m.send(StatusMsg{Index: idx, Status: StatusStarting})

	// Rotate the log file if it's grown past the threshold since last session.
	// Rotation at-start only: the child owns the fd, so mid-session rotation
	// would silently lose writes to the renamed file.
	if err := state.RotateIfLarge(svc.LogFile); err != nil {
		svc.Logs.Add(fmt.Sprintf("[pairin] log rotation failed: %v", err))
	}

	if err := os.MkdirAll(state.LogsDir(m.configPath), 0o755); err != nil {
		svc.Status = StatusCrashed
		svc.Logs.Add(fmt.Sprintf("[pairin] failed to create logs dir: %v", err))
		m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		return
	}

	// Open the log file for the child to write to. O_APPEND is belt-and-suspenders:
	// the child has its own fd with its own append flag already, but this keeps
	// behavior consistent if we ever write to it from pairin too.
	logF, err := os.OpenFile(svc.LogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		svc.Status = StatusCrashed
		svc.Logs.Add(fmt.Sprintf("[pairin] failed to open log file: %v", err))
		m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		return
	}

	// Session marker so adopted/tailed logs are readable across pairin restarts.
	fmt.Fprintf(logF, "\n--- pairin session started %s ---\n", time.Now().Format(time.RFC3339))

	cmd := exec.Command("sh", "-c", svc.Config.Cmd)
	cmd.Dir = svc.Config.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = logF
	cmd.Stderr = logF

	if err := cmd.Start(); err != nil {
		logF.Close()
		svc.Status = StatusCrashed
		svc.Logs.Add(fmt.Sprintf("[pairin] failed to start: %v", err))
		m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		return
	}

	// Child has its own dup of the fd; pairin's copy is no longer needed.
	logF.Close()

	svc.cmd = cmd
	svc.PID = cmd.Process.Pid
	if pgid, perr := syscall.Getpgid(svc.PID); perr == nil {
		svc.PGID = pgid
	} else {
		svc.PGID = svc.PID
	}
	svc.Status = StatusRunning
	m.send(StatusMsg{Index: idx, Status: StatusRunning, PID: svc.PID})

	// Tail the log file in background to feed the TUI.
	m.startTailer(idx)

	// Wait for process to exit in background
	gen := svc.generation
	go m.waitForExit(idx, cmd, gen)

	// Start healthcheck poller if configured
	if svc.Config.Healthcheck != "" {
		m.startHealthcheckPoller(idx)
	}

	// Persist the updated state outside the lock.
	go m.persistState()
}

// startTailer launches a goroutine that polls the service's log file for new
// bytes and feeds them to the ring buffer + TUI. Safe for both freshly-started
// and adopted services — the child owns the fd either way, pairin just reads.
//
// Must be called with svc.mu held.
func (m *Manager) startTailer(idx int) {
	svc := m.Services[idx]

	// Stop any previous tailer for this service before starting a new one.
	if svc.tailCancel != nil {
		svc.tailCancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	svc.tailCancel = cancel
	path := svc.LogFile

	go m.tailFile(ctx, idx, path)
}

// tailFile opens the given log path and emits LogMsgs for every new line that
// appears. It tolerates the file not yet existing and detects rotation (inode
// change or truncation) by reopening.
func (m *Manager) tailFile(ctx context.Context, idx int, path string) {
	var (
		f        *os.File
		lastIno  uint64
		pending  []byte
		offset   int64
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	open := func() error {
		if f != nil {
			f.Close()
			f = nil
		}
		nf, err := os.Open(path)
		if err != nil {
			return err
		}
		f = nf
		if info, err := nf.Stat(); err == nil {
			if sys, ok := info.Sys().(*syscall.Stat_t); ok {
				lastIno = sys.Ino
			}
		}
		offset = 0
		pending = pending[:0]
		return nil
	}

	readChunk := func() {
		if f == nil {
			return
		}
		info, err := f.Stat()
		if err != nil {
			return
		}
		// Detect rotation: inode changed, or file shrank below our offset.
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			if lastIno != 0 && sys.Ino != lastIno {
				_ = open()
				return
			}
		}
		if info.Size() < offset {
			// Truncated (rare); start over.
			offset = 0
			pending = pending[:0]
		}
		if info.Size() == offset {
			return
		}
		buf := make([]byte, 64*1024)
		for {
			n, err := f.ReadAt(buf, offset)
			if n > 0 {
				offset += int64(n)
				pending = append(pending, buf[:n]...)
				for {
					nl := bytes.IndexByte(pending, '\n')
					if nl < 0 {
						break
					}
					line := string(pending[:nl])
					pending = pending[nl+1:]
					m.emitLine(idx, line)
				}
			}
			if err == io.EOF || n == 0 {
				return
			}
			if err != nil {
				return
			}
		}
	}

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		if f == nil {
			// File may not exist yet for fresh services — retry quietly.
			_ = open()
		}
		readChunk()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// emitLine feeds a log line into the service's ring buffer and sends it to the TUI.
func (m *Manager) emitLine(idx int, line string) {
	svc := m.Services[idx]
	svc.mu.Lock()
	svc.Logs.Add(line)
	svc.mu.Unlock()
	m.send(LogMsg{Index: idx, Line: line})
}

func (m *Manager) waitForExit(idx int, cmd *exec.Cmd, gen int) {
	svc := m.Services[idx]

	err := cmd.Wait()

	svc.mu.Lock()

	// If a new process has been started since, this goroutine is stale.
	if svc.generation != gen {
		svc.mu.Unlock()
		return
	}

	var exitedWithFailure bool
	if err != nil {
		exitedWithFailure = true
		// Only mark as crashed if it wasn't intentionally stopped
		if svc.Status == StatusRunning {
			svc.Status = StatusCrashed
			svc.Logs.Add(fmt.Sprintf("[pairin] process exited: %v", err))
			m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		}
	} else {
		exitedWithFailure = false
		if svc.Status == StatusRunning {
			svc.Status = StatusStopped
			svc.Logs.Add("[pairin] process exited normally")
			m.send(StatusMsg{Index: idx, Status: StatusStopped})
		}
	}

	// Determine if we should auto-restart
	shouldRestart := m.shouldAutoRestart(svc, exitedWithFailure)
	svc.PID = 0
	svc.PGID = 0
	svc.cmd = nil
	svc.mu.Unlock()

	go m.persistState()

	if shouldRestart && !m.quitting.Load() {
		m.autoRestartService(idx, gen)
	}
}

// shouldAutoRestart checks whether a service should be automatically restarted.
// Must be called with svc.mu held.
func (m *Manager) shouldAutoRestart(svc *Service, exitedWithFailure bool) bool {
	policy := svc.Config.RestartPolicy()
	if policy == "no" {
		return false
	}

	// Don't auto-restart if the service was intentionally stopped
	if svc.Status != StatusCrashed && svc.Status != StatusStopped {
		return false
	}

	// Check max_restarts limit
	if svc.Config.MaxRestarts > 0 && svc.RestartCount >= svc.Config.MaxRestarts {
		svc.Logs.Add(fmt.Sprintf("[pairin] max restarts reached (%d/%d)", svc.RestartCount, svc.Config.MaxRestarts))
		return false
	}

	switch policy {
	case "always":
		return true
	case "on-failure":
		return exitedWithFailure
	case "on-success":
		return !exitedWithFailure
	default:
		return false
	}
}

// autoRestartService handles the auto-restart flow: sets status to restarting,
// waits for the cooldown delay, then restarts the service.
func (m *Manager) autoRestartService(idx int, originalGen int) {
	if m.quitting.Load() {
		return
	}

	svc := m.Services[idx]
	delay := svc.Config.ParsedRestartDelay()

	svc.mu.Lock()
	// Check generation hasn't changed (e.g., manual restart happened)
	if svc.generation != originalGen {
		svc.mu.Unlock()
		return
	}
	svc.Status = StatusRestarting
	svc.RestartCount++

	var msg string
	if svc.Config.MaxRestarts > 0 {
		msg = fmt.Sprintf("[pairin] restarting in %s (%d/%d)...", delay, svc.RestartCount, svc.Config.MaxRestarts)
	} else {
		msg = fmt.Sprintf("[pairin] restarting in %s...", delay)
	}
	svc.Logs.Add(msg)
	svc.mu.Unlock()

	m.send(StatusMsg{Index: idx, Status: StatusRestarting})

	time.Sleep(delay)

	if m.quitting.Load() {
		return
	}

	// Check generation again after sleep
	svc.mu.Lock()
	if svc.generation != originalGen {
		svc.mu.Unlock()
		return
	}
	svc.Healthy = false
	svc.mu.Unlock()

	m.send(HealthCheckMsg{Index: idx, Healthy: false})
	m.startService(idx)
}

// RestartService stops and restarts a single service. Adopted services can't
// be restarted in phase 1 — pairin doesn't own the process tree, so re-exec
// with the original environment isn't safe. Surface a hint in the log instead.
func (m *Manager) RestartService(idx int) tea.Cmd {
	return func() tea.Msg {
		svc := m.Services[idx]
		svc.mu.Lock()
		adopted := svc.Adopted
		svc.mu.Unlock()
		if adopted {
			svc.mu.Lock()
			svc.Logs.Add("[pairin] restart not supported on adopted services; stop first, then let pairin re-launch")
			svc.mu.Unlock()
			m.send(LogMsg{Index: idx, Line: "[pairin] restart not supported on adopted services; stop first, then let pairin re-launch"})
			return ServiceRestartedMsg{Index: idx}
		}

		m.stopService(idx)

		svc.mu.Lock()
		svc.Healthy = false
		svc.RestartCount = 0 // reset auto-restart counter on manual restart
		svc.mu.Unlock()
		m.send(HealthCheckMsg{Index: idx, Healthy: false})

		m.startService(idx)
		return ServiceRestartedMsg{Index: idx}
	}
}

func (m *Manager) stopService(idx int) {
	svc := m.Services[idx]
	svc.mu.Lock()

	// Cancel healthcheck poller
	if svc.healthCancel != nil {
		svc.healthCancel()
		svc.healthCancel = nil
	}
	svc.Healthy = false

	adopted := svc.Adopted
	pid := svc.PID
	pgid := svc.PGID
	cmd := svc.cmd

	if pid == 0 && cmd == nil {
		svc.mu.Unlock()
		return
	}

	svc.Status = StatusStopped
	svc.Logs.Add("[pairin] stopping...")
	svc.mu.Unlock()

	// Signal the process group. Works the same for adopted and owned services.
	if pgid == 0 && pid != 0 {
		if p, err := syscall.Getpgid(pid); err == nil {
			pgid = p
		}
	}
	if pgid != 0 {
		syscall.Kill(-pgid, syscall.SIGINT)
	} else if pid != 0 {
		syscall.Kill(pid, syscall.SIGINT)
	}

	done := make(chan struct{})
	go func() {
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Wait()
		} else if pid != 0 {
			// Adopted: not our child, can't Wait. Poll instead.
			for state.IsProcessAlive(pid) {
				time.Sleep(100 * time.Millisecond)
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if pgid != 0 {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		} else if pid != 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			// Abandon wait — process is truly stuck
		}
	}

	svc.mu.Lock()
	svc.PID = 0
	svc.PGID = 0
	svc.cmd = nil
	if svc.tailCancel != nil {
		svc.tailCancel()
		svc.tailCancel = nil
	}
	if adopted {
		// Adopted services are one-shot: after a stop they exit the pool.
		svc.Adopted = false
	}
	svc.mu.Unlock()
	go m.persistState()
}

// StopAll stops all services in parallel. Called on quit. Clears state.json
// and releases the pairin lock so a clean shutdown leaves no droppings behind.
func (m *Manager) StopAll() {
	m.quitting.Store(true)

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := range m.Services {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				m.stopService(idx)
			}(i)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		// Hard timeout: must exit regardless
	}

	if m.configPath != "" {
		_ = state.Clear(m.configPath)
		_ = state.ReleaseLock(m.configPath)
	}

	m.mu.Lock()
	m.sink = nil
	m.mu.Unlock()
}

func detectBranch(dir string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// Message types sent from Manager to the TUI.

type LogMsg struct {
	Index int
	Line  string
}

type StatusMsg struct {
	Index  int
	Status Status
	PID    int
}

type AllStartedMsg struct{}

type ServiceRestartedMsg struct {
	Index int
}

type HealthCheckMsg struct {
	Index   int
	Healthy bool
}

// checkTCP dials a TCP address with a 1-second timeout.
func checkTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// checkHTTP sends a GET request with a 2-second timeout, expects 2xx.
func checkHTTP(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// runHealthcheck dispatches to the appropriate checker based on URL scheme.
func runHealthcheck(hc string) bool {
	switch {
	case strings.HasPrefix(hc, "tcp://"):
		return checkTCP(strings.TrimPrefix(hc, "tcp://"))
	case strings.HasPrefix(hc, "http://"), strings.HasPrefix(hc, "https://"):
		return checkHTTP(hc)
	default:
		return false
	}
}

// startHealthcheckPoller starts a goroutine that polls the service's
// healthcheck every 2 seconds. It sends HealthCheckMsg on state changes
// and triggers waiting dependents when the service becomes healthy.
func (m *Manager) startHealthcheckPoller(idx int) {
	svc := m.Services[idx]
	// svc.mu must be held by the caller (startService holds it)
	ctx, cancel := context.WithCancel(context.Background())
	svc.healthCancel = cancel
	gen := svc.generation
	hc := svc.Config.Healthcheck

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				healthy := runHealthcheck(hc)

				svc.mu.Lock()
				if svc.generation != gen {
					svc.mu.Unlock()
					return
				}
				changed := healthy != svc.Healthy
				svc.Healthy = healthy
				svc.mu.Unlock()

				if changed {
					m.send(HealthCheckMsg{Index: idx, Healthy: healthy})
					if healthy {
						m.tryStartWaiting()
					}
				}
			}
		}
	}()
}

// allDepsHealthy returns true if all dependencies of the service at idx are healthy.
func (m *Manager) allDepsHealthy(idx int) bool {
	svc := m.Services[idx]
	for _, depName := range svc.Config.DependsOn {
		depIdx := m.nameToIdx[depName]
		dep := m.Services[depIdx]
		dep.mu.Lock()
		healthy := dep.Healthy
		dep.mu.Unlock()
		if !healthy {
			return false
		}
	}
	return true
}

// tryStartWaiting iterates services in StatusWaiting and starts any whose
// dependencies are now all healthy.
func (m *Manager) tryStartWaiting() {
	for i, svc := range m.Services {
		svc.mu.Lock()
		waiting := svc.Status == StatusWaiting
		svc.mu.Unlock()

		if waiting && m.allDepsHealthy(i) {
			m.startService(i)
		}
	}
}
