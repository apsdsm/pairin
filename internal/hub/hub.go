// Package hub holds connections to every pairin supervisor on the host at
// once. It is what turns a per-project TUI into a fleet view: the catalog and
// the instance registry between them say which projects exist and which are
// running, and the hub keeps a control client attached to each live one,
// tagging every event with the instance it came from.
//
// Each instance is supervised by its own goroutine that dials, watches for the
// connection dropping, and redials with backoff. Nothing here blocks the
// caller: Refresh only adjusts the *set* of instances, and connecting happens
// in the background, because control.Dial waits up to five seconds for a
// snapshot and a dozen projects would otherwise serialize into a minute.
package hub

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/catalog"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/launcher"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
)

// InstanceID identifies a project. It is the absolute config path, which is
// already the key everything else in pairin is organized around.
type InstanceID string

// ConnState is how the hub currently relates to one project.
type ConnState int

const (
	// StateStopped: no supervisor is running for this project.
	StateStopped ConnState = iota
	// StateConnecting: a supervisor is up and we're dialing it.
	StateConnecting
	// StateConnected: attached, mirroring its services.
	StateConnected
	// StateUnreachable: a supervisor is registered but won't talk to us.
	StateUnreachable
)

func (s ConnState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateUnreachable:
		return "unreachable"
	default:
		return "stopped"
	}
}

// Msg wraps a message from one instance's control client so the fleet model can
// tell which project it came from.
type Msg struct {
	ID    InstanceID
	Inner tea.Msg
}

// StateMsg reports that an instance's connection state changed.
type StateMsg struct {
	ID    InstanceID
	State ConnState
	Err   error
}

// InstanceView is an immutable description of one project, safe to render from.
// As with process.ServiceView, the renderer never touches live structs: the
// per-instance supervise goroutines mutate them continuously.
type InstanceView struct {
	ID            InstanceID
	Name          string
	Display       string
	ConfigPath    string
	Group         string
	State         ConnState
	Err           error
	Registered    bool
	Pinned        bool
	SupervisorPID int
	StartedAt     time.Time
	Services      []process.ServiceView
}

// Label is the best available human name for the project.
func (v InstanceView) Label() string {
	if v.Display != "" {
		return v.Display
	}
	if v.Name != "" {
		return v.Name
	}
	return filepath.Base(filepath.Dir(v.ConfigPath))
}

type instance struct {
	id         InstanceID
	name       string
	display    string
	configPath string
	group      string
	registered bool
	pinned     bool

	state         ConnState
	err           error
	supervisorPID int
	startedAt     time.Time

	client *control.Client

	// stubs describe the services of a project that isn't running, read from
	// its config so a stopped project still shows its shape.
	stubs []process.ServiceView

	cancel context.CancelFunc
}

// Hub owns the set of instances and their connections.
type Hub struct {
	mu        sync.Mutex
	instances map[InstanceID]*instance
	order     []InstanceID
	sink      process.Sink
	closed    bool

	// logMode is the subscription applied to every new connection. The fleet
	// view sets LogsNone; zooming narrows a single instance to LogsOnly.
	logMode control.LogMode

	pollInterval time.Duration
}

// New builds an empty hub. Call Refresh to populate it.
func New() *Hub {
	return &Hub{
		instances:    make(map[InstanceID]*instance),
		logMode:      control.LogsNone,
		pollInterval: 2 * time.Second,
	}
}

// SetSink installs the destination for tagged events (a tea.Program).
func (h *Hub) SetSink(s process.Sink) {
	h.mu.Lock()
	h.sink = s
	h.mu.Unlock()
}

// SetDefaultLogMode changes the subscription used for new connections.
func (h *Hub) SetDefaultLogMode(mode control.LogMode) {
	h.mu.Lock()
	h.logMode = mode
	h.mu.Unlock()
}

// Close tears down every connection. Services keep running.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	insts := make([]*instance, 0, len(h.instances))
	for _, inst := range h.instances {
		insts = append(insts, inst)
	}
	h.instances = make(map[InstanceID]*instance)
	h.order = nil
	h.mu.Unlock()

	for _, inst := range insts {
		if inst.cancel != nil {
			inst.cancel()
		}
	}
}

// discovered is what one pass over the catalog and registry learned about a
// project, before it's reconciled against the hub's existing instances.
type discovered struct {
	name, display, group string
	registered           bool
	pinned               bool
	running              bool
	pid                  int
	startedAt            time.Time
}

func (d *discovered) displayOrName() string {
	if d.display != "" {
		return d.display
	}
	return d.name
}

// Refresh reconciles the instance set against the catalog and the registry:
// new projects get a supervise goroutine, departed ones are torn down. It does
// not block on any network work.
func (h *Hub) Refresh() {
	found := map[InstanceID]*discovered{}

	if cat, err := catalog.Load(); err == nil {
		for _, p := range cat.Projects {
			found[InstanceID(p.Config)] = &discovered{
				name: p.Name, display: p.Display, group: p.Group,
				registered: true, pinned: p.Pinned(),
			}
		}
	}

	// Running supervisors are included whether or not they're registered — a
	// project someone started by path should still show up in the fleet view.
	if insts, err := state.ListInstances(); err == nil {
		for _, inst := range insts {
			id := InstanceID(inst.ConfigPath)
			d, ok := found[id]
			if !ok {
				d = &discovered{}
				found[id] = d
			}
			d.pid = inst.SupervisorPID
			d.startedAt = inst.StartedAt
			d.running = true
			if d.display == "" {
				d.display = inst.ProjectName
			}
		}
	}

	// An unpinned project that isn't running is dropped rather than shown.
	// These are entries `pairin up` added on the user's behalf; keeping them
	// forever means a project started once to check something clutters the
	// dashboard indefinitely, with no services to select and so no way to act
	// on it. Pinning (p in the dashboard, or `pairin register`) opts back in.
	for id, d := range found {
		if !d.running && !d.pinned {
			delete(found, id)
		}
	}

	// Sorted, so the fleet view's project order is stable rather than whatever
	// order the map happened to yield this time.
	ids := make([]InstanceID, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := found[ids[i]], found[ids[j]]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.displayOrName() != b.displayOrName() {
			return a.displayOrName() < b.displayOrName()
		}
		return ids[i] < ids[j]
	})

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}

	for _, id := range ids {
		d := found[id]
		inst, ok := h.instances[id]
		if !ok {
			inst = &instance{
				id:         id,
				configPath: string(id),
				state:      StateStopped,
			}
			h.instances[id] = inst
			h.order = append(h.order, id)

			ctx, cancel := context.WithCancel(context.Background())
			inst.cancel = cancel
			go h.supervise(ctx, id)
		}
		inst.name = d.name
		if d.display != "" {
			inst.display = d.display
		}
		inst.group = d.group
		inst.registered = d.registered
		inst.pinned = d.pinned
		inst.supervisorPID = d.pid
		if !d.startedAt.IsZero() {
			inst.startedAt = d.startedAt
		}
	}

	// Drop instances that are neither registered nor running any more.
	var kept []InstanceID
	var dropped []*instance
	for _, id := range h.order {
		if _, ok := found[id]; ok {
			kept = append(kept, id)
			continue
		}
		if inst, ok := h.instances[id]; ok {
			dropped = append(dropped, inst)
			delete(h.instances, id)
		}
	}
	h.order = kept
	h.mu.Unlock()

	for _, inst := range dropped {
		if inst.cancel != nil {
			inst.cancel()
		}
	}
}

// supervise owns one instance's connection for the hub's lifetime: wait for a
// supervisor to exist, dial it, hold the connection, and redial when it drops.
func (h *Hub) supervise(ctx context.Context, id InstanceID) {
	defer crash.Guard(fmt.Sprintf("hub: supervise %s", id))

	backoff := 250 * time.Millisecond
	const maxBackoff = 5 * time.Second

	// Read the project's shape from its config straight away, whatever state it
	// is in. Dialing takes up to a snapshot timeout, and until it lands there is
	// no client to read services from — a project would otherwise appear empty
	// for those seconds every time the dashboard opened.
	h.loadStubs(id)

	for {
		if ctx.Err() != nil {
			return
		}

		configPath := string(id)
		holder := state.LockHolder(configPath)
		if holder == 0 || !state.IsProcessAlive(holder) {
			h.setState(id, StateStopped, nil)
			h.loadStubs(id)
			if !sleepCtx(ctx, h.pollInterval) {
				return
			}
			backoff = 250 * time.Millisecond
			continue
		}

		h.setState(id, StateConnecting, nil)
		client, err := control.Dial(state.SocketPath(configPath))
		if err != nil {
			h.setState(id, StateUnreachable, err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = 250 * time.Millisecond

		if !h.attach(id, client) {
			// The instance went away while we were dialing.
			_ = client.Close()
			return
		}

		select {
		case <-client.Done():
		case <-ctx.Done():
			_ = client.Close()
			h.detach(id)
			return
		}
		h.detach(id)
	}
}

// attach installs a connected client, returning false if the instance has since
// been removed.
func (h *Hub) attach(id InstanceID, client *control.Client) bool {
	h.mu.Lock()
	inst, ok := h.instances[id]
	if !ok || h.closed {
		h.mu.Unlock()
		return false
	}
	inst.client = client
	inst.state = StateConnected
	inst.err = nil
	if client.ProjectName != "" {
		inst.display = client.ProjectName
	}
	if !client.StartedAt.IsZero() {
		inst.startedAt = client.StartedAt
	}
	mode := h.logMode
	h.mu.Unlock()

	client.SetSink(instanceSink{hub: h, id: id})
	_ = client.SubscribeLogs(mode)

	h.notifyState(id, StateConnected, nil)
	return true
}

// detach drops a connection, keeping what the supervisor was running so the
// project holds its shape on screen.
//
// Without this the project blinks empty: Snapshot falls back to the config-read
// stubs when there's no client, and those were never loaded while connected, so
// there is a window with no services at all — which the dashboard renders as
// "(no services)". Exactly when a supervisor goes away is when you're looking.
func (h *Hub) detach(id InstanceID) {
	h.mu.Lock()
	inst, ok := h.instances[id]
	var client *control.Client
	if ok {
		client = inst.client
	}
	h.mu.Unlock()

	var last []process.ServiceView
	if client != nil {
		for _, svc := range client.ServiceList() {
			v := svc.View()
			// The supervisor is gone, so nothing it was running is running.
			v.Status = process.StatusStopped
			v.PID = 0
			v.Healthy = false
			last = append(last, v)
		}
	}

	h.mu.Lock()
	if inst, ok := h.instances[id]; ok {
		if inst.client != nil {
			_ = inst.client.Close()
			inst.client = nil
		}
		inst.state = StateStopped
		if len(last) > 0 {
			inst.stubs = last
		}
	}
	h.mu.Unlock()
	h.notifyState(id, StateStopped, nil)
}

func (h *Hub) setState(id InstanceID, st ConnState, err error) {
	h.mu.Lock()
	inst, ok := h.instances[id]
	if !ok {
		h.mu.Unlock()
		return
	}
	changed := inst.state != st
	inst.state = st
	inst.err = err
	h.mu.Unlock()

	if changed {
		h.notifyState(id, st, err)
	}
}

// loadStubs reads a stopped project's config so the fleet view can show its
// service names before anything is running.
func (h *Hub) loadStubs(id InstanceID) {
	h.mu.Lock()
	inst, ok := h.instances[id]
	if !ok || len(inst.stubs) > 0 {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	cfg, err := config.LoadFrom(string(id))
	if err != nil {
		return
	}
	stubs := make([]process.ServiceView, 0, len(cfg.Services))
	for _, svc := range cfg.Services {
		stubs = append(stubs, process.ServiceView{
			Name:        svc.Name,
			Short:       svc.Short,
			Color:       svc.Color,
			Dir:         svc.Dir,
			Cmd:         svc.Cmd,
			Status:      process.StatusStopped,
			HasHealth:   svc.Healthcheck != "",
			MaxRestarts: svc.MaxRestarts,
			DependsOn:   svc.DependsOn,
		})
	}

	h.mu.Lock()
	if inst, ok := h.instances[id]; ok {
		inst.stubs = stubs
	}
	h.mu.Unlock()
}

// Snapshot returns a value copy of every instance, in a stable order. This is
// what the fleet TUI renders from.
func (h *Hub) Snapshot() []InstanceView {
	h.mu.Lock()
	insts := make([]*instance, 0, len(h.order))
	for _, id := range h.order {
		if inst, ok := h.instances[id]; ok {
			insts = append(insts, inst)
		}
	}
	// Note what to read each instance's services from while holding the lock,
	// but read them after releasing it: Service.View takes its own lock, and
	// holding two at once is how deadlocks start.
	type source struct {
		client *control.Client
		stubs  []process.ServiceView
	}

	views := make([]InstanceView, len(insts))
	sources := make([]source, len(insts))
	for i, inst := range insts {
		views[i] = InstanceView{
			ID:            inst.id,
			Name:          inst.name,
			Display:       inst.display,
			ConfigPath:    inst.configPath,
			Group:         inst.group,
			State:         inst.state,
			Err:           inst.err,
			Registered:    inst.registered,
			Pinned:        inst.pinned,
			SupervisorPID: inst.supervisorPID,
			StartedAt:     inst.startedAt,
		}
		sources[i] = source{client: inst.client, stubs: inst.stubs}
	}
	h.mu.Unlock()

	for i, src := range sources {
		if src.client != nil {
			for _, svc := range src.client.ServiceList() {
				views[i].Services = append(views[i].Services, svc.View())
			}
			continue
		}
		views[i].Services = append(views[i].Services, src.stubs...)
	}
	return views
}

// Get returns one instance's view.
func (h *Hub) Get(id InstanceID) (InstanceView, bool) {
	for _, v := range h.Snapshot() {
		if v.ID == id {
			return v, true
		}
	}
	return InstanceView{}, false
}

// RestartService asks an instance's supervisor to restart one service.
func (h *Hub) RestartService(id InstanceID, service string) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.RequestRestart(service)
	})
}

// StopService asks an instance's supervisor to stop one service.
func (h *Hub) StopService(id InstanceID, service string) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.RequestStop(service)
	})
}

// StartService asks an instance's supervisor to start one service.
func (h *Hub) StartService(id InstanceID, service string) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.RequestStart(service)
	})
}

// StartProject spawns a supervisor for a stopped project. It returns as soon
// as the supervisor is serving; the instance's supervise goroutine notices and
// connects on its own.
func (h *Hub) StartProject(id InstanceID) error {
	h.mu.Lock()
	inst, ok := h.instances[id]
	st := StateStopped
	if ok {
		st = inst.state
	}
	h.mu.Unlock()

	if !ok {
		return fmt.Errorf("no such project: %s", id)
	}
	if st != StateStopped {
		return fmt.Errorf("project is already %s", st)
	}
	return launcher.Start(string(id), launcher.DefaultTimeout)
}

// AddProject makes a config appear in the dashboard, pinned — going looking for
// it is the same deliberate signal `pairin register` carries. Returns the
// catalog name it was given.
//
// A config that is already catalogued but *unpinned* is pinned rather than
// rejected. Catalogue membership and dashboard visibility are different
// questions: an unpinned, stopped project has a record but isn't shown, and
// refusing to add it would mean telling the user it's already in a list they
// can plainly see it isn't in.
//
// The config is loaded first: adding something that turns out not to be a valid
// pairin config would put an entry in the catalog that can never start.
func (h *Hub) AddProject(configPath string) (string, error) {
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		return "", err
	}

	cat, err := catalog.Load()
	if err != nil {
		return "", fmt.Errorf("loading catalog: %w", err)
	}
	entry, err := cat.SetPinned(cfg.Path, cfg.Project.Name, true)
	if err != nil {
		return "", err
	}
	if err := cat.Save(); err != nil {
		return "", fmt.Errorf("saving catalog: %w", err)
	}

	h.Refresh()
	return entry.Name, nil
}

// SetPinned pins or unpins a project, adding it to the catalog if it isn't
// there yet. An unpinned project disappears from the dashboard as soon as it
// stops running; a pinned one stays, so it can be started again from here.
func (h *Hub) SetPinned(id InstanceID, pinned bool) error {
	h.mu.Lock()
	inst, ok := h.instances[id]
	display := ""
	if ok {
		display = inst.display
	}
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such project: %s", id)
	}

	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}
	if _, err := cat.SetPinned(string(id), display, pinned); err != nil {
		return err
	}
	if err := cat.Save(); err != nil {
		return fmt.Errorf("saving catalog: %w", err)
	}

	h.mu.Lock()
	if inst, ok := h.instances[id]; ok {
		inst.pinned = pinned
		inst.registered = true
	}
	h.mu.Unlock()
	return nil
}

// ClearLogs discards a service's history. An empty service name clears every
// service in the project.
func (h *Hub) ClearLogs(id InstanceID, service string) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.RequestClearLogs(service)
	})
}

// StopProject shuts a whole project down.
func (h *Hub) StopProject(id InstanceID) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.Shutdown()
	})
}

// SubscribeLogs narrows one instance's log stream — used when zooming into a
// service, so only that service's lines cross the socket.
func (h *Hub) SubscribeLogs(id InstanceID, mode control.LogMode, services ...string) error {
	return h.withClient(id, func(c *control.Client) error {
		return c.SubscribeLogs(mode, services...)
	})
}

// Services returns an instance's mirrored services, for a caller that needs the
// live structs (the log pane) rather than a value snapshot.
func (h *Hub) Services(id InstanceID) []*process.Service {
	h.mu.Lock()
	inst, ok := h.instances[id]
	var client *control.Client
	if ok {
		client = inst.client
	}
	h.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.ServiceList()
}

func (h *Hub) withClient(id InstanceID, fn func(*control.Client) error) error {
	h.mu.Lock()
	inst, ok := h.instances[id]
	var client *control.Client
	if ok {
		client = inst.client
	}
	h.mu.Unlock()

	if !ok {
		return fmt.Errorf("no such project: %s", id)
	}
	if client == nil {
		return fmt.Errorf("not connected to %s", filepath.Base(filepath.Dir(string(id))))
	}
	return fn(client)
}

func (h *Hub) forward(msg tea.Msg) {
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		sink.Send(msg)
	}
}

func (h *Hub) notifyState(id InstanceID, st ConnState, err error) {
	h.forward(StateMsg{ID: id, State: st, Err: err})
}

// instanceSink tags an instance's events before handing them on.
type instanceSink struct {
	hub *Hub
	id  InstanceID
}

func (s instanceSink) Send(msg tea.Msg) {
	s.hub.forward(Msg{ID: s.id, Inner: msg})
}

// sleepCtx waits for d, returning false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
