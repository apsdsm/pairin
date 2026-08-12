package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/process"
)

// Server wraps a process.Manager with a unix-socket control interface.
// It installs itself as the Manager's Sink so every tea.Msg the manager
// produces is fanned out as a protocol Event to all connected clients.
type Server struct {
	mgr       *process.Manager
	cfg       *config.Config
	startedAt time.Time
	ln        net.Listener

	mu      sync.Mutex
	clients map[*serverClient]struct{}
	closed  bool

	shutdown chan struct{}
}

// NewServer prepares a Server. It does not listen yet; call Start for that.
func NewServer(mgr *process.Manager, cfg *config.Config) *Server {
	return &Server{
		mgr:       mgr,
		cfg:       cfg,
		startedAt: time.Now(),
		clients:   make(map[*serverClient]struct{}),
		shutdown:  make(chan struct{}),
	}
}

// Start listens on socketPath and begins accepting clients. It installs the
// server as the Manager's event sink. Returns when the socket is open; the
// accept loop runs in a goroutine.
func (s *Server) Start(socketPath string) error {
	// Remove any stale socket file; a previous supervisor may have left one.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	s.ln = ln
	s.mgr.SetSink(s)

	go s.acceptLoop()
	return nil
}

// Shutdown stops the socket, closes all clients, and signals any waiter.
// Safe to call multiple times.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ln := s.ln
	clients := s.clients
	s.clients = make(map[*serverClient]struct{})
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	// Broadcast shutdown to clients so they can disconnect cleanly, then close.
	for c := range clients {
		_ = c.write(Event{Kind: EvtShutdown})
		c.close()
	}
	select {
	case <-s.shutdown:
	default:
		close(s.shutdown)
	}
}

// Done returns a channel that's closed when Shutdown has been called.
func (s *Server) Done() <-chan struct{} { return s.shutdown }

// Send implements process.Sink. Converts each tea.Msg from the Manager into
// a protocol Event and broadcasts it to every connected client.
func (s *Server) Send(msg tea.Msg) {
	evt, ok := s.eventFor(msg)
	if !ok {
		return
	}
	s.broadcast(evt)
}

// eventFor converts a Manager tea.Msg into a protocol Event, resolving the
// service index to a name so clients don't need to know about indices.
func (s *Server) eventFor(msg tea.Msg) (Event, bool) {
	switch m := msg.(type) {
	case process.StatusMsg:
		if m.Index < 0 || m.Index >= len(s.mgr.Services) {
			return Event{}, false
		}
		v := s.mgr.Services[m.Index].View()
		return Event{Kind: EvtStatus, Status: &StatusEvent{
			Service:      v.Name,
			Status:       m.Status.String(),
			PID:          m.PID,
			Branch:       v.Branch,
			RestartCount: v.RestartCount,
		}}, true
	case process.LogMsg:
		if m.Index < 0 || m.Index >= len(s.mgr.Services) {
			return Event{}, false
		}
		return Event{Kind: EvtLog, Log: &LogEvent{
			Service: s.mgr.Services[m.Index].Config.Name,
			Line:    m.Line,
		}}, true
	case process.HealthCheckMsg:
		if m.Index < 0 || m.Index >= len(s.mgr.Services) {
			return Event{}, false
		}
		return Event{Kind: EvtHealth, Health: &HealthEvent{
			Service: s.mgr.Services[m.Index].Config.Name,
			Healthy: m.Healthy,
		}}, true
	}
	// AllStartedMsg / ServiceRestartedMsg are TUI-internal; drop them.
	return Event{}, false
}

// broadcast hands the event to every client's send queue. Enqueuing never
// blocks, so a client that has stopped reading its socket can no longer stall
// the Manager goroutine that produced the event — nor the other clients, which
// used to sit behind it in this loop.
func (s *Server) broadcast(evt Event) {
	s.mu.Lock()
	clients := make([]*serverClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		c.enqueue(evt)
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := newServerClient(conn, s.snapshot)
		s.mu.Lock()
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		go c.writeLoop()
		go s.handle(c)
	}
}

func (s *Server) handle(c *serverClient) {
	defer crash.Guard("control: client reader")
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		c.close()
	}()

	// First event to any new client is a fresh snapshot.
	snap := s.snapshot()
	c.enqueue(Event{Kind: EvtSnapshot, Snapshot: &snap})

	reader := bufio.NewReader(c.conn)
	dec := json.NewDecoder(reader)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		s.dispatch(req)
	}
}

func (s *Server) dispatch(req Request) {
	switch req.Kind {
	case ReqSnapshot:
		snap := s.snapshot()
		s.broadcast(Event{Kind: EvtSnapshot, Snapshot: &snap})
	case ReqRestart:
		if idx, ok := s.serviceIndex(req.Service); ok {
			// Fire and forget; the Manager runs this on its own goroutine via
			// the returned tea.Cmd. We just invoke it.
			go func(cmd tea.Cmd) {
				if cmd != nil {
					cmd()
				}
			}(s.mgr.RestartService(idx))
		}
	case ReqStop:
		if idx, ok := s.serviceIndex(req.Service); ok {
			go s.mgr.StopService(idx)
		}
	case ReqStart:
		if idx, ok := s.serviceIndex(req.Service); ok {
			go s.mgr.StartService(idx)
		}
	case ReqShutdown:
		go func() {
			s.mgr.StopAll()
			s.Shutdown()
		}()
	}
}

func (s *Server) serviceIndex(name string) (int, bool) {
	for i, svc := range s.mgr.Services {
		if svc.Config.Name == name {
			return i, true
		}
	}
	return 0, false
}

func (s *Server) snapshot() Snapshot {
	out := Snapshot{
		ConfigPath:  s.cfg.Path,
		ProjectName: s.cfg.Project.Name,
		StartedAt:   s.startedAt,
	}
	for _, svc := range s.mgr.Services {
		out.Services = append(out.Services, serviceSnapshot(svc))
	}
	return out
}

func serviceSnapshot(svc *process.Service) ServiceSnapshot {
	v := svc.View()
	return ServiceSnapshot{
		Name:         v.Name,
		Short:        v.Short,
		Color:        v.Color,
		Dir:          v.Dir,
		Cmd:          v.Cmd,
		Status:       v.Status.String(),
		PID:          v.PID,
		Branch:       v.Branch,
		Healthy:      v.Healthy,
		HasHealth:    v.HasHealth,
		Adopted:      v.Adopted,
		LogFile:      v.LogFile,
		RestartCount: v.RestartCount,
		MaxRestarts:  v.MaxRestarts,
		DependsOn:    v.DependsOn,
	}
}

// clientQueueSize bounds how far behind a client may fall before we start
// dropping events. A few thousand events is several seconds of very chatty
// output — past that the client isn't keeping up in any useful sense.
const clientQueueSize = 4096

// clientWriteTimeout bounds a single socket write. Without it, a client that
// has been suspended (ctrl+z) or sits behind a stalled SSH pipe holds the
// write open indefinitely and the writer goroutine never comes back.
const clientWriteTimeout = 10 * time.Second

// serverClient is one connected TUI. Events are queued and written by a
// dedicated goroutine so that a slow or wedged client degrades only itself.
type serverClient struct {
	conn     net.Conn
	enc      *json.Encoder
	ch       chan Event
	done     chan struct{}
	once     sync.Once
	snapshot func() Snapshot

	// wmu serializes encoder access: writeLoop owns the normal path, but
	// Shutdown writes its farewell event directly from the caller's goroutine.
	wmu sync.Mutex

	mu      sync.Mutex
	dropped int
	resync  bool
}

func newServerClient(conn net.Conn, snapshot func() Snapshot) *serverClient {
	return &serverClient{
		conn:     conn,
		enc:      json.NewEncoder(conn),
		ch:       make(chan Event, clientQueueSize),
		done:     make(chan struct{}),
		snapshot: snapshot,
	}
}

// enqueue queues an event for delivery, never blocking. When the queue is full
// the event is dropped and the client is flagged for resync — once it catches
// up, writeLoop pushes a fresh snapshot, which restores correctness no matter
// which events were lost.
func (c *serverClient) enqueue(evt Event) {
	select {
	case <-c.done:
		return
	default:
	}

	select {
	case c.ch <- evt:
	default:
		c.mu.Lock()
		c.dropped++
		c.resync = true
		c.mu.Unlock()
	}
}

func (c *serverClient) writeLoop() {
	defer crash.Guard("control: client writer")
	defer c.close()

	for {
		select {
		case <-c.done:
			return
		case evt := <-c.ch:
			if err := c.write(evt); err != nil {
				return
			}
		}

		// Caught up. If we dropped anything getting here, resend a snapshot so
		// the client's view is authoritative again.
		if len(c.ch) == 0 {
			c.mu.Lock()
			resync, dropped := c.resync, c.dropped
			c.resync, c.dropped = false, 0
			c.mu.Unlock()

			if resync {
				fmt.Fprintf(os.Stderr, "pairin: client fell behind, dropped %d event(s); resyncing\n", dropped)
				snap := c.snapshot()
				if err := c.write(Event{Kind: EvtSnapshot, Snapshot: &snap}); err != nil {
					return
				}
			}
		}
	}
}

func (c *serverClient) write(evt Event) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	select {
	case <-c.done:
		return errors.New("closed")
	default:
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); err != nil {
		return err
	}
	return c.enc.Encode(evt)
}

func (c *serverClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

