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
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/process"
)

// Client connects a TUI to a remote supervisor. It exposes the subset of
// process.Manager that the TUI uses, so the rest of the TUI doesn't care
// whether it's talking to a local or remote process manager. Events the
// supervisor broadcasts are applied to a local Services mirror and then
// forwarded to the TUI as the same tea.Msg types the Manager produces, so
// the TUI's Update loop is unchanged.
type Client struct {
	socketPath string

	// ProjectName is pulled from the snapshot so the TUI header can render
	// before the TUI has its own config.
	ProjectName string
	StartedAt   time.Time

	// Services mirrors the supervisor's services so TUI panes can render
	// without any round trips. The slice itself is built once, from the first
	// snapshot, and never reallocated — a reconnect updates the existing
	// structs in place so pointers the TUI holds stay valid.
	Services  []*process.Service
	nameToIdx map[string]int

	sink process.Sink // tea.Program, once SetProgram is called

	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	done chan struct{}
	err  error

	// Desired log subscription, remembered so it can be re-sent after a
	// reconnect — subscription is per-connection state on the server, and a
	// supervisor restart would otherwise silently resume the full firehose.
	logMode     LogMode
	logServices []string
}

// snapshotTimeout bounds how long we wait for a supervisor to describe itself
// before giving up on a connection attempt.
const snapshotTimeout = 5 * time.Second

// Dial connects to the supervisor listening at socketPath. It blocks until
// the initial snapshot has been received so that Services is populated by
// the time Dial returns.
func Dial(socketPath string) (*Client, error) {
	c := &Client{
		socketPath: socketPath,
		nameToIdx:  make(map[string]int),
		done:       closedChan(),
	}
	if err := c.Reconnect(); err != nil {
		return nil, err
	}
	return c, nil
}

// Reconnect establishes (or re-establishes) the connection to the supervisor
// and blocks until a snapshot has arrived. The mirrored Services are refreshed
// in place, so a TUI holding pointers into them survives the round trip.
//
// Calling this on a live client replaces the connection; callers are expected
// to reach here from a disconnect, not to multiplex.
func (c *Client) Reconnect() error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", c.socketPath, err)
	}

	ready := make(chan struct{})
	done := make(chan struct{})

	c.mu.Lock()
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.done = done
	c.err = nil
	c.mu.Unlock()

	go c.readLoop(conn, done, ready)

	select {
	case <-ready:
		c.resendSubscription()
		return nil
	case <-done:
		if err := c.Error(); err != nil {
			return err
		}
		return errors.New("supervisor closed connection before snapshot")
	case <-time.After(snapshotTimeout):
		_ = conn.Close()
		return errors.New("timed out waiting for supervisor snapshot")
	}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
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

// RequestRestart asks the supervisor to restart a service by name. Unlike
// RestartService it isn't tied to the TUI's pane indices, which is what the
// fleet hub needs — it addresses services across several supervisors at once.
func (c *Client) RequestRestart(service string) error {
	return c.send(Request{Kind: ReqRestart, Service: service})
}

// RequestStop asks the supervisor to stop a service by name.
func (c *Client) RequestStop(service string) error {
	return c.send(Request{Kind: ReqStop, Service: service})
}

// RequestStart asks the supervisor to start a service by name.
func (c *Client) RequestStart(service string) error {
	return c.send(Request{Kind: ReqStart, Service: service})
}

// RequestClearLogs asks the supervisor to discard a service's history. An empty
// name clears every service in the project.
func (c *Client) RequestClearLogs(service string) error {
	return c.send(Request{Kind: ReqClearLogs, Service: service})
}

// StopAll in client mode does nothing to the services themselves — 'q' in
// the TUI becomes a detach rather than a shutdown. Use Shutdown for the
// explicit "kill everything" path.
func (c *Client) StopAll() {}

// Shutdown asks the supervisor to stop all services and exit.
func (c *Client) Shutdown() error {
	return c.send(Request{Kind: ReqShutdown})
}

// SubscribeLogs narrows (or restores) which services stream their log lines to
// this client. The choice is remembered across reconnects.
func (c *Client) SubscribeLogs(mode LogMode, services ...string) error {
	c.mu.Lock()
	c.logMode = mode
	c.logServices = append([]string(nil), services...)
	c.mu.Unlock()
	return c.send(Request{Kind: ReqSubscribe, LogMode: mode, Services: services})
}

func (c *Client) resendSubscription() {
	c.mu.Lock()
	mode, services := c.logMode, append([]string(nil), c.logServices...)
	c.mu.Unlock()
	if mode == LogsAll && len(services) == 0 {
		return
	}
	_ = c.send(Request{Kind: ReqSubscribe, LogMode: mode, Services: services})
}

// Error returns the last fatal error seen by the read loop, if any.
func (c *Client) Error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done returns a channel that's closed when the current connection is torn
// down. A Reconnect installs a fresh channel, so callers should re-read this
// after reconnecting rather than caching the result.
func (c *Client) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

func (c *Client) send(req Request) error {
	c.mu.Lock()
	enc := c.enc
	c.mu.Unlock()
	if enc == nil {
		return errors.New("client closed")
	}
	return enc.Encode(req)
}

// readLoop decodes events until the connection fails. conn, done and ready are
// passed in rather than read from the struct so that a reconnect can't leave
// this loop operating on a channel or socket that has since been replaced.
func (c *Client) readLoop(conn net.Conn, done, ready chan struct{}) {
	defer crash.Guard("control: client read loop")
	defer close(done)

	dec := json.NewDecoder(conn)
	for {
		var evt Event
		if err := dec.Decode(&evt); err != nil {
			c.mu.Lock()
			if err != io.EOF {
				c.err = err
			}
			c.mu.Unlock()
			return
		}
		c.apply(evt)

		if evt.Kind == EvtSnapshot {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
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
	case EvtStatus:
		if evt.Status == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Status.Service]
		if !ok {
			return
		}
		status := statusFromString(evt.Status.Status)
		c.Services[idx].ApplyStatus(status, evt.Status.PID, evt.Status.Branch, evt.Status.RestartCount)
		c.forward(process.StatusMsg{Index: idx, Status: status, PID: evt.Status.PID})
	case EvtLog:
		if evt.Log == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Log.Service]
		if !ok {
			return
		}
		c.Services[idx].AppendLog(evt.Log.Line)
		c.forward(process.LogMsg{Index: idx, Line: evt.Log.Line})
	case EvtHealth:
		if evt.Health == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Health.Service]
		if !ok {
			return
		}
		c.Services[idx].ApplyHealth(evt.Health.Healthy)
		c.forward(process.HealthCheckMsg{Index: idx, Healthy: evt.Health.Healthy})
	case EvtPorts:
		if evt.Ports == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.Ports.Service]
		if !ok {
			return
		}
		c.Services[idx].ApplyPorts(fromWirePorts(evt.Ports.Ports))
		c.forward(process.PortsMsg{Index: idx, Ports: fromWirePorts(evt.Ports.Ports)})
	case EvtLogsCleared:
		if evt.LogsCleared == nil {
			return
		}
		idx, ok := c.nameToIdx[evt.LogsCleared.Service]
		if !ok {
			return
		}
		c.Services[idx].ClearLogBuffer()
		c.forward(process.LogsClearedMsg{Index: idx})
	case EvtShutdown:
		// Supervisor is going away; the read loop will see EOF next.
	}
}

func (c *Client) applySnapshot(snap Snapshot) {
	c.mu.Lock()
	c.ProjectName = snap.ProjectName
	c.StartedAt = snap.StartedAt
	c.mu.Unlock()

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
			svc.Ports = fromWirePorts(s.Ports)
			svc.RestartCount = s.RestartCount
			c.Services = append(c.Services, svc)
			c.nameToIdx[s.Name] = i
		}
		return
	}
	// Later snapshots (a resync, or a reconnect): update fields in-place so
	// pointers the TUI holds remain valid.
	for _, s := range snap.Services {
		idx, ok := c.nameToIdx[s.Name]
		if !ok {
			continue
		}
		c.Services[idx].UpdateMirror(process.ServiceView{
			Status:       statusFromString(s.Status),
			PID:          s.PID,
			Branch:       s.Branch,
			Healthy:      s.Healthy,
			Adopted:      s.Adopted,
			LogFile:      s.LogFile,
			Ports:        fromWirePorts(s.Ports),
			RestartCount: s.RestartCount,
		})
		c.forward(process.StatusMsg{Index: idx, Status: statusFromString(s.Status), PID: s.PID})
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
