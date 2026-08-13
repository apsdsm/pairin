package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/catalog"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
)

// collector captures everything the hub forwards.
type collector struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (c *collector) Send(msg tea.Msg) {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
}

func (c *collector) snapshot() []tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tea.Msg(nil), c.msgs...)
}

// tagged returns the instance IDs that produced a message of the given kind.
func (c *collector) taggedIDs() map[InstanceID]int {
	out := map[InstanceID]int{}
	for _, m := range c.snapshot() {
		if hm, ok := m.(Msg); ok {
			out[hm.ID]++
		}
	}
	return out
}

// project writes a config and returns its path. Kept in a short temp dir
// because unix socket paths cap out around 100 bytes.
func project(t *testing.T, root, name string, services ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := fmt.Sprintf("[project]\nname = %q\n", name)
	for _, svc := range services {
		body += fmt.Sprintf("\n[[services]]\nname = %q\ncmd = \"while true; do echo %s-line; sleep 0.2; done\"\ndir = \".\"\n", svc, svc)
	}
	path := filepath.Join(dir, ".pairinrc.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// startSupervisor runs a manager + control server in-process, standing in for a
// detached supervisor without needing the pairin binary on disk.
func startSupervisor(t *testing.T, configPath string) *control.Server {
	t.Helper()

	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		t.Fatalf("loading %s: %v", configPath, err)
	}
	if err := state.EnsureDirs(cfg.Path); err != nil {
		t.Fatalf("state dirs: %v", err)
	}
	if err := os.WriteFile(state.LockPath(cfg.Path), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatalf("lockfile: %v", err)
	}

	mgr := process.NewManager(cfg)
	srv := control.NewServer(mgr, cfg)
	if err := srv.Start(state.SocketPath(cfg.Path)); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	mgr.StartAll()()

	t.Cleanup(func() {
		mgr.StopAll()
		srv.Shutdown()
		_ = os.Remove(state.LockPath(cfg.Path))
	})
	return srv
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The headline case: one hub, three projects, events from all of them arriving
// tagged with the project they came from.
func TestHubConnectsToSeveralSupervisors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	var paths []string
	for _, name := range []string{"a", "b", "c"} {
		p := project(t, root, name, "svc")
		paths = append(paths, p)
		startSupervisor(t, p)

		if err := state.Register(state.Instance{
			SupervisorPID: os.Getpid(),
			ConfigPath:    p,
			ProjectName:   name,
			SocketPath:    state.SocketPath(p),
			StartedAt:     time.Now(),
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	h := New()
	defer h.Close()

	sink := &collector{}
	h.SetSink(sink)
	// Subscribe to logs so the test can observe tagged log events.
	h.SetDefaultLogMode(control.LogsAll)
	h.Refresh()

	waitFor(t, "all three instances to connect", 10*time.Second, func() bool {
		connected := 0
		for _, v := range h.Snapshot() {
			if v.State == StateConnected {
				connected++
			}
		}
		return connected == 3
	})

	waitFor(t, "tagged events from every instance", 10*time.Second, func() bool {
		return len(sink.taggedIDs()) == 3
	})

	ids := sink.taggedIDs()
	for _, p := range paths {
		if ids[InstanceID(p)] == 0 {
			t.Errorf("no events tagged for %s", p)
		}
	}

	// Every instance should be mirroring its one service.
	for _, v := range h.Snapshot() {
		if len(v.Services) != 1 {
			t.Errorf("%s mirrors %d services, want 1", v.Label(), len(v.Services))
		}
	}
}

// A registered project with no supervisor still has to appear, with its service
// names read from the config — you can't start what you can't see.
func TestStoppedProjectShowsItsShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	p := project(t, root, "idle", "web", "worker")

	cat := &catalog.Catalog{}
	if _, err := cat.Add(catalog.Project{Display: "Idle", Config: p}); err != nil {
		t.Fatalf("catalog add: %v", err)
	}
	if err := cat.Save(); err != nil {
		t.Fatalf("catalog save: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	waitFor(t, "stub services to load", 5*time.Second, func() bool {
		views := h.Snapshot()
		return len(views) == 1 && len(views[0].Services) == 2
	})

	v := h.Snapshot()[0]
	if v.State != StateStopped {
		t.Errorf("state = %v, want stopped", v.State)
	}
	if !v.Registered {
		t.Error("registered project not marked as registered")
	}
	names := []string{v.Services[0].Name, v.Services[1].Name}
	if names[0] != "web" || names[1] != "worker" {
		t.Errorf("stub services = %v, want [web worker]", names)
	}
}

// When a supervisor dies, the hub must notice and fall back to stopped rather
// than keep reporting a connection it no longer has.
func TestHubNoticesSupervisorGoingAway(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	p := project(t, root, "solo", "svc")
	srv := startSupervisor(t, p)
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: p, ProjectName: "solo",
		SocketPath: state.SocketPath(p), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	waitFor(t, "connection", 10*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && v[0].State == StateConnected
	})

	// Supervisor goes away, and so does its lockfile.
	srv.Shutdown()
	_ = os.Remove(state.LockPath(p))

	waitFor(t, "hub to notice the disconnect", 10*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && v[0].State == StateStopped
	})
}

// Snapshot must be safe to call while the hub's goroutines are churning — this
// is the fleet TUI's render path, and it runs on a different goroutine.
func TestSnapshotIsRaceFree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	p := project(t, root, "busy", "svc")
	startSupervisor(t, p)
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: p, ProjectName: "busy",
		SocketPath: state.SocketPath(p), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := New()
	defer h.Close()
	h.SetDefaultLogMode(control.LogsAll)
	h.SetSink(&collector{})
	h.Refresh()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.Refresh()
			time.Sleep(time.Millisecond)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range h.Snapshot() {
			_ = v.Label()
			for _, s := range v.Services {
				_ = s.Status
			}
		}
	}
	<-done
}

// The scenario this was built for: something starts a project temporarily,
// `pairin up` auto-registers it, and once it stops it should get out of the
// way rather than sitting in the dashboard forever.
func TestUnpinnedProjectDropsOffWhenStopped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	temp := project(t, root, "temp", "svc")
	kept := project(t, root, "kept", "svc")

	cat := &catalog.Catalog{}
	if _, err := cat.Add(catalog.Project{Display: "temp", Config: temp, Auto: true}); err != nil {
		t.Fatalf("catalog add: %v", err)
	}
	if _, err := cat.Add(catalog.Project{Display: "kept", Config: kept}); err != nil {
		t.Fatalf("catalog add: %v", err)
	}
	if err := cat.Save(); err != nil {
		t.Fatalf("catalog save: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	// Neither is running, so only the pinned one should be listed.
	waitFor(t, "only the pinned project", 5*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && v[0].ConfigPath == kept
	})

	// While it is running, the unpinned one appears again — you can still see
	// and act on anything that's actually up.
	startSupervisor(t, temp)
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: temp, ProjectName: "temp",
		SocketPath: state.SocketPath(temp), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.Refresh()

	waitFor(t, "the running unpinned project to appear", 5*time.Second, func() bool {
		for _, v := range h.Snapshot() {
			if v.ConfigPath == temp {
				return !v.Pinned
			}
		}
		return false
	})
}

// Pinning is how a project that would otherwise vanish gets kept, and works on
// one that was never in the catalog at all.
func TestSetPinnedKeepsAProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	// Running, but never registered.
	p := project(t, root, "adhoc", "svc")
	startSupervisor(t, p)
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: p, ProjectName: "adhoc",
		SocketPath: state.SocketPath(p), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	waitFor(t, "the running project", 5*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && !v[0].Pinned
	})

	if err := h.SetPinned(InstanceID(p), true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog load: %v", err)
	}
	entry, ok := cat.ByConfig(p)
	if !ok {
		t.Fatal("pinning did not add the project to the catalog")
	}
	if !entry.Pinned() {
		t.Error("catalog entry is not pinned")
	}

	h.Refresh()
	if v := h.Snapshot(); len(v) != 1 || !v[0].Pinned {
		t.Errorf("hub does not report the project as pinned: %+v", v)
	}
}

// A project must not blink empty when its supervisor goes away. Snapshot falls
// back to config-read stubs when there's no client, and those aren't loaded
// while connected — so the disconnect used to leave a window with no services,
// which the dashboard renders as "(no services)".
func TestServicesSurviveTheSupervisorStopping(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	p := project(t, root, "solo", "web", "worker")
	srv := startSupervisor(t, p)
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: p, ProjectName: "solo",
		SocketPath: state.SocketPath(p), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	waitFor(t, "connection", 10*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && v[0].State == StateConnected && len(v[0].Services) == 2
	})

	srv.Shutdown()
	_ = os.Remove(state.LockPath(p))

	// Detach directly rather than racing the supervise goroutine. The window
	// this guards is microseconds wide, so a polling watcher would pass by luck
	// as often as by correctness; this is the exact moment it opens.
	h.detach(InstanceID(p))

	v := h.Snapshot()
	got := 0
	if len(v) == 1 {
		got = len(v[0].Services)
	}
	if got != 2 {
		t.Fatalf("project reported %d services the moment its supervisor went away, want 2", got)
	}

	waitFor(t, "hub to notice the disconnect", 10*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && v[0].State == StateStopped
	})

	// What's left is the last known set, marked stopped.
	for _, s := range h.Snapshot()[0].Services {
		if s.Status != process.StatusStopped {
			t.Errorf("%s status = %v after the supervisor went away, want stopped", s.Name, s.Status)
		}
		if s.PID != 0 {
			t.Errorf("%s still reports pid %d", s.Name, s.PID)
		}
	}
}

// Services are known from the config before any connection is made, so a
// project isn't empty for the seconds a dial can take.
func TestServicesKnownBeforeConnecting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	// Registered and marked running, but nothing is actually listening, so the
	// dial will hang until it times out.
	p := project(t, root, "slow", "web", "worker")
	if err := state.EnsureDirs(p); err != nil {
		t.Fatalf("state dirs: %v", err)
	}
	if err := os.WriteFile(state.LockPath(p), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatalf("lockfile: %v", err)
	}
	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: p, ProjectName: "slow",
		SocketPath: state.SocketPath(p), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := New()
	defer h.Close()
	h.Refresh()

	waitFor(t, "services read from the config", 5*time.Second, func() bool {
		v := h.Snapshot()
		return len(v) == 1 && len(v[0].Services) == 2
	})
}
