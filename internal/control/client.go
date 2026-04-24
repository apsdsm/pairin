package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/process"
)

// Client connects a TUI to a remote supervisor. It exposes the subset of
// process.Manager that the TUI uses, so the rest of the TUI doesn't care
// whether it's talking to a local or remote process manager. Events the
// supervisor broadcasts are applied to a local Services mirror and then
// forwarded to the TUI as the same tea.Msg types the Manager produces, so
// the TUI's Update loop is unchanged.
type Client struct {
	conn net.Conn
	enc  *json.Encoder

	// ProjectName is pulled from the snapshot so the TUI header can render
	// before the TUI has its own config.
	ProjectName string
	StartedAt   time.Time

	// Services mirrors the supervisor's services so TUI panes can render
	// without any round trips. Fields are mutated as events arrive.
	Services  []*process.Service
	nameToIdx map[string]int

	sink process.Sink // tea.Program, once SetProgram is called

	mu     sync.Mutex
	ready  chan struct{}
	closed chan struct{}
	err    error
}

// Dial connects to the supervisor listening at socketPath. It blocks until
// the initial snapshot has been received so that Services is populated by
// the time Dial returns.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", socketPath, err)
	}
	c := &Client{
		conn:      conn,
		enc:       json.NewEncoder(conn),
		nameToIdx: make(map[string]int),
		ready:     make(chan struct{}),
		closed:    make(chan struct{}),
	}
	go c.readLoop()

	// Wait for the initial snapshot or an early failure.
	select {
	case <-c.ready:
		return c, nil
	case <-c.closed:
		if c.err != nil {
			return nil, c.err
		}
		return nil, errors.New("supervisor closed connection before snapshot")
	case <-time.After(5 * time.Second):
		_ = conn.Close()
		return nil, errors.New("timed out waiting for supervisor snapshot")
	}
}

// ServiceList returns the mirrored service list. Satisfies the TUI backend interface.
func (c *Client) ServiceList() []*process.Service { return c.Services }

// Close tears down the connection to the supervisor. Services keep running.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// SetSink installs the TUI's tea.Program (or any Sink) so incoming events
// can be re-emitted as tea.Msg values.
func (c *Client) SetSink(s process.Sink) {
	c.mu.Lock()
	c.sink = s
	c.mu.Unlock()
}

// SetProgram is a compatibility alias matching process.Manager's method.
func (c *Client) SetProgram(p *tea.Program) { c.SetSink(p) }

// StartAll is a no-op in client mode — the supervisor has already started
// (or adopted) services. It exists so the TUI can call it unconditionally.
func (c *Client) StartAll() tea.Cmd {
	return func() tea.Msg { return process.AllStartedMsg{} }
}

// RestartService sends a restart request to the supervisor.
func (c *Client) RestartService(idx int) tea.Cmd {
	return func() tea.Msg {
		if idx < 0 || idx >= len(c.Services) {
			return process.ServiceRestartedMsg{Index: idx}
		}
		_ = c.send(Request{Kind: ReqRestart, Service: c.Services[idx].Config.Name})
		return process.ServiceRestartedMsg{Index: idx}
	}
}

// StopAll in client mode does nothing to the services themselves — 'q' in
// the TUI becomes a detach rather than a shutdown. Use Shutdown for the
// explicit "kill everything" path.
func (c *Client) StopAll() {}

// Shutdown asks the supervisor to stop all services and exit.
func (c *Client) Shutdown() error {
	return c.send(Request{Kind: ReqShutdown})
}

// Error returns the last fatal error seen by the read loop, if any.
func (c *Client) Error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done returns a channel that's closed when the connection is torn down.
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) send(req Request) error {
	c.mu.Lock()
	enc := c.enc
	c.mu.Unlock()
	if enc == nil {
		return errors.New("client closed")
	}
	return enc.Encode(req)
}

func (c *Client) readLoop() {
	dec := json.NewDecoder(c.conn)
	for {
		var evt Event
		if err := dec.Decode(&evt); err != nil {
			c.mu.Lock()
			if err != io.EOF {
				c.err = err
			}
			c.mu.Unlock()
			close(c.closed)
			return
		}
		c.apply(evt)
	}
}

// apply updates the Services mirror based on an incoming event and forwards
// the corresponding tea.Msg to the TUI so it refreshes.
func (c *Client) apply(evt Event) {
	switch evt.Kind {
	case EvtSnapshot:
		if evt.Snapshot == nil {
			return
		}
		c.applySnapshot(*evt.Snapshot)
		select {
		case <-c.ready:
			// already ready; this is a refreshed snapshot
		default:
			close(c.ready)
		}
	case EvtStatus:
		if evt.Status == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Status.Service]
		if !ok {
			return
		}
		svc := c.Services[idx]
		svc.Status = statusFromString(evt.Status.Status)
		svc.PID = evt.Status.PID
		if evt.Status.Branch != "" {
			svc.Branch = evt.Status.Branch
		}
		svc.RestartCount = evt.Status.RestartCount
		c.forward(process.StatusMsg{Index: idx, Status: svc.Status, PID: svc.PID})
	case EvtLog:
		if evt.Log == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Log.Service]
		if !ok {
			return
		}
		svc := c.Services[idx]
		svc.Logs.Add(evt.Log.Line)
		c.forward(process.LogMsg{Index: idx, Line: evt.Log.Line})
	case EvtHealth:
		if evt.Health == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Health.Service]
		if !ok {
			return
		}
		svc := c.Services[idx]
		svc.Healthy = evt.Health.Healthy
		c.forward(process.HealthCheckMsg{Index: idx, Healthy: evt.Health.Healthy})
	case EvtShutdown:
		// Supervisor is going away; the read loop will see EOF next.
	}
}

func (c *Client) applySnapshot(snap Snapshot) {
	c.ProjectName = snap.ProjectName
	c.StartedAt = snap.StartedAt

	if len(c.Services) == 0 {
		// First snapshot: build mirror services from scratch.
		c.Services = make([]*process.Service, 0, len(snap.Services))
		c.nameToIdx = make(map[string]int, len(snap.Services))
		for i, s := range snap.Services {
			svc := process.NewMirrorService(s.Name, s.Short, s.Color, s.Dir, s.Cmd, s.DependsOn, s.HasHealth, s.MaxRestarts)
			svc.Status = statusFromString(s.Status)
			svc.PID = s.PID
			svc.Branch = s.Branch
			svc.Healthy = s.Healthy
			svc.Adopted = s.Adopted
			svc.LogFile = s.LogFile
			svc.RestartCount = s.RestartCount
			c.Services = append(c.Services, svc)
			c.nameToIdx[s.Name] = i
		}
		return
	}
	// Later snapshots: update fields in-place so pointers the TUI holds remain valid.
	for _, s := range snap.Services {
		idx, ok := c.nameToIdx[s.Name]
		if !ok {
			continue
		}
		svc := c.Services[idx]
		svc.Status = statusFromString(s.Status)
		svc.PID = s.PID
		svc.Branch = s.Branch
		svc.Healthy = s.Healthy
		svc.Adopted = s.Adopted
		svc.LogFile = s.LogFile
		svc.RestartCount = s.RestartCount
	}
}

func (c *Client) forward(msg tea.Msg) {
	c.mu.Lock()
	s := c.sink
	c.mu.Unlock()
	if s != nil {
		s.Send(msg)
	}
}

func statusFromString(s string) process.Status {
	switch s {
	case "waiting":
		return process.StatusWaiting
	case "starting":
		return process.StatusStarting
	case "running":
		return process.StatusRunning
	case "crashed":
		return process.StatusCrashed
	case "restarting":
		return process.StatusRestarting
	default:
		return process.StatusStopped
	}
}
