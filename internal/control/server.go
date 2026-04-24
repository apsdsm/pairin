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
		svc := s.mgr.Services[m.Index]
		return Event{Kind: EvtStatus, Status: &StatusEvent{
			Service:      svc.Config.Name,
			Status:       m.Status.String(),
			PID:          m.PID,
			Branch:       svc.Branch,
			RestartCount: svc.RestartCount,
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

func (s *Server) broadcast(evt Event) {
	s.mu.Lock()
	clients := make([]*serverClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		if err := c.write(evt); err != nil {
			c.close()
			s.mu.Lock()
			delete(s.clients, c)
			s.mu.Unlock()
		}
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &serverClient{conn: conn, enc: json.NewEncoder(conn)}
		s.mu.Lock()
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) handle(c *serverClient) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		c.close()
	}()

	// First event to any new client is a fresh snapshot.
	snap := s.snapshot()
	if err := c.write(Event{Kind: EvtSnapshot, Snapshot: &snap}); err != nil {
		return
	}

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
	// We read fields without locking; Service fields are written under
	// svc.mu but the broadcast-from-Sink path already tolerates slightly
	// stale views. For a snapshot, a torn read is acceptable because the
	// next status event will correct it on the client side.
	return ServiceSnapshot{
		Name:         svc.Config.Name,
		Short:        svc.Config.Short,
		Color:        svc.Config.Color,
		Dir:          svc.Config.Dir,
		Cmd:          svc.Config.Cmd,
		Status:       svc.Status.String(),
		PID:          svc.PID,
		Branch:       svc.Branch,
		Healthy:      svc.Healthy,
		HasHealth:    svc.Config.Healthcheck != "",
		Adopted:      svc.Adopted,
		LogFile:      svc.LogFile,
		RestartCount: svc.RestartCount,
		MaxRestarts:  svc.Config.MaxRestarts,
		DependsOn:    svc.Config.DependsOn,
	}
}

// serverClient is one connected TUI. Writes are serialized by the server's
// broadcast loop; the encoder itself is not otherwise shared.
type serverClient struct {
	conn   net.Conn
	enc    *json.Encoder
	closed bool
	mu     sync.Mutex
}

func (c *serverClient) write(evt Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	return c.enc.Encode(evt)
}

func (c *serverClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	_ = c.conn.Close()
}

