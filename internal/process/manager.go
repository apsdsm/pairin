package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
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
	Branch  string
	Logs    *RingBuffer
	Healthy bool
	RestartCount int // number of auto-restarts since last manual start/restart

	cmd          *exec.Cmd
	generation   int
	healthCancel context.CancelFunc
	mu           sync.Mutex
}

// GetLines returns a copy of the log lines (thread-safe).
func (s *Service) GetLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Logs.Lines()
}

// Manager orchestrates all services.
type Manager struct {
	Services  []*Service
	nameToIdx map[string]int
	program   *tea.Program
	mu        sync.Mutex
	err       error
	quitting  atomic.Bool
}

func NewManager(cfg *config.Config) *Manager {
	services := make([]*Service, len(cfg.Services))
	nameToIdx := make(map[string]int, len(cfg.Services))
	for i, sc := range cfg.Services {
		services[i] = &Service{
			Config: sc,
			Status: StatusStopped,
			Logs:   NewRingBuffer(ringBufferSize),
		}
		nameToIdx[sc.Name] = i
	}
	return &Manager{Services: services, nameToIdx: nameToIdx}
}

func (m *Manager) SetProgram(p *tea.Program) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.program = p
}

func (m *Manager) Error() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *Manager) send(msg tea.Msg) {
	m.mu.Lock()
	p := m.program
	m.mu.Unlock()
	if p != nil {
		p.Send(msg)
	}
}

// StartAll launches all services. Services with unmet dependencies enter
// StatusWaiting and are started automatically once their deps become healthy.
func (m *Manager) StartAll() tea.Cmd {
	return func() tea.Msg {
		for i, svc := range m.Services {
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

func (m *Manager) startService(idx int) {
	svc := m.Services[idx]
	svc.mu.Lock()
	defer svc.mu.Unlock()

	svc.generation++

	// Detect git branch
	svc.Branch = detectBranch(svc.Config.Dir)

	svc.Status = StatusStarting
	m.send(StatusMsg{Index: idx, Status: StatusStarting})

	cmd := exec.Command("sh", "-c", svc.Config.Cmd)
	cmd.Dir = svc.Config.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		svc.Status = StatusCrashed
		svc.Logs.Add(fmt.Sprintf("[pairin] failed to create stdout pipe: %v", err))
		m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		return
	}

	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		svc.Status = StatusCrashed
		svc.Logs.Add(fmt.Sprintf("[pairin] failed to start: %v", err))
		m.send(StatusMsg{Index: idx, Status: StatusCrashed})
		return
	}

	svc.cmd = cmd
	svc.PID = cmd.Process.Pid
	svc.Status = StatusRunning
	m.send(StatusMsg{Index: idx, Status: StatusRunning, PID: svc.PID})

	// Read output in background
	go m.captureOutput(idx, stdout)

	// Wait for process to exit in background
	gen := svc.generation
	go m.waitForExit(idx, cmd, gen)

	// Start healthcheck poller if configured
	if svc.Config.Healthcheck != "" {
		m.startHealthcheckPoller(idx)
	}
}

func (m *Manager) captureOutput(idx int, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		svc := m.Services[idx]
		svc.mu.Lock()
		svc.Logs.Add(line)
		svc.mu.Unlock()
		m.send(LogMsg{Index: idx, Line: line})
	}
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
	svc.cmd = nil
	svc.mu.Unlock()

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

// RestartService stops and restarts a single service.
func (m *Manager) RestartService(idx int) tea.Cmd {
	return func() tea.Msg {
		m.stopService(idx)

		svc := m.Services[idx]
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

	if svc.cmd == nil || svc.cmd.Process == nil {
		svc.mu.Unlock()
		return
	}

	svc.Status = StatusStopped
	svc.Logs.Add("[pairin] stopping...")
	cmd := svc.cmd
	svc.mu.Unlock()

	// Send SIGINT to process group (mutex released so captureOutput can drain the pipe)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		syscall.Kill(-pgid, syscall.SIGINT)
	}

	// Wait up to 5 seconds for graceful shutdown
	done := make(chan struct{})
	go func() {
		cmd.Process.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if pgid != 0 {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		cmd.Process.Kill() // fallback: always try direct kill

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			// Abandon wait — process is truly stuck
		}
	}

	svc.mu.Lock()
	svc.PID = 0
	svc.cmd = nil
	svc.mu.Unlock()
}

// StopAll stops all services in parallel. Called on quit.
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

	m.mu.Lock()
	m.program = nil
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
