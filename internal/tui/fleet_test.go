package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/hub"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
)

// fleetProject writes a config and starts an in-process supervisor for it,
// standing in for a detached one without needing the binary on disk.
func fleetProject(t *testing.T, root, name string, services ...string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf("[project]\nname = %q\n", name)
	for _, svc := range services {
		body += fmt.Sprintf("\n[[services]]\nname = %q\ncmd = \"while true; do echo %s-%s; sleep 0.2; done\"\ndir = \".\"\n", svc, name, svc)
	}
	path := filepath.Join(dir, ".pairinrc.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
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
		t.Fatalf("start server: %v", err)
	}
	mgr.StartAll()()

	if err := state.Register(state.Instance{
		SupervisorPID: os.Getpid(), ConfigPath: cfg.Path, ProjectName: name,
		SocketPath: state.SocketPath(cfg.Path), StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	t.Cleanup(func() {
		mgr.StopAll()
		srv.Shutdown()
		_ = os.Remove(state.LockPath(cfg.Path))
	})
	return cfg.Path
}

func newFleet(t *testing.T, width, height int, setup func(root string)) (FleetModel, *hub.Hub) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))
	setup(root)

	h := hub.New()
	t.Cleanup(h.Close)
	h.Refresh()

	// Wait for the hub to attach before building the model.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready := len(h.Snapshot()) > 0
		for _, v := range h.Snapshot() {
			if v.State != hub.StateConnected {
				ready = false
			}
		}
		if ready {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	m := NewFleetModel(h)
	model, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return model.(FleetModel), h
}

func sendFleet(m FleetModel, msgs ...tea.Msg) FleetModel {
	for _, msg := range msgs {
		model, _ := m.Update(msg)
		m = model.(FleetModel)
	}
	return m
}

// The point of the whole exercise: several projects, one screen.
func TestFleetShowsEveryProject(t *testing.T) {
	m, _ := newFleet(t, 100, 30, func(root string) {
		fleetProject(t, root, "alpha", "web", "worker")
		fleetProject(t, root, "beta", "api")
	})

	view := m.View()
	for _, want := range []string{"alpha", "beta", "web", "worker", "api"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "2 projects") {
		t.Errorf("header does not count the projects:\n%s", view)
	}
}

// Service names collide across projects all the time — two apps each with a
// "web". Selection has to distinguish them, which is what GridCell.Key is for.
func TestFleetSelectionDistinguishesSameNamedServices(t *testing.T) {
	m, _ := newFleet(t, 100, 30, func(root string) {
		fleetProject(t, root, "alpha", "web")
		fleetProject(t, root, "beta", "web")
	})

	inst, service, ok := m.selection()
	if !ok {
		t.Fatal("nothing selected")
	}
	first := inst.ID
	if service != "web" {
		t.Fatalf("selected service = %q, want web", service)
	}

	// Move to the next cell: same service name, different project.
	m = sendFleet(m, key("down"))
	inst2, service2, ok := m.selection()
	if !ok {
		t.Fatal("nothing selected after moving")
	}
	if service2 != "web" {
		t.Fatalf("second selection = %q, want web", service2)
	}
	if inst2.ID == first {
		t.Error("moving between projects stayed on the same instance")
	}
}

// Zooming narrows that supervisor's log stream to the one service being shown,
// so the rest of the host's output stays off the wire.
func TestFleetZoomAndBack(t *testing.T) {
	m, _ := newFleet(t, 100, 30, func(root string) {
		fleetProject(t, root, "alpha", "web", "worker")
	})

	inst, service, ok := m.selection()
	if !ok {
		t.Fatal("nothing selected")
	}

	m = sendFleet(m, key("z"))
	if !m.zoomed {
		t.Fatalf("'z' did not zoom (status: %q)", m.status)
	}
	if m.zoomID != inst.ID || m.zoomService != service {
		t.Errorf("zoomed into %s/%s, want %s/%s", m.zoomID, m.zoomService, inst.ID, service)
	}

	view := m.View()
	if !strings.Contains(view, service) {
		t.Errorf("zoomed view does not name the service:\n%s", view)
	}

	m = sendFleet(m, key("esc"))
	if m.zoomed {
		t.Error("esc did not leave the zoomed view")
	}
}

// A log line for the zoomed service lands in its pane; one for anything else
// must not.
func TestFleetRoutesLogsToTheZoomedPane(t *testing.T) {
	m, _ := newFleet(t, 100, 30, func(root string) {
		fleetProject(t, root, "alpha", "web", "worker")
	})

	m = sendFleet(m, key("z"))
	if !m.zoomed {
		t.Fatalf("could not zoom (status: %q)", m.status)
	}

	before := len(m.pane.lines)
	m = sendFleet(m, hub.Msg{ID: m.zoomID, Inner: process.LogMsg{Index: m.zoomIndex, Line: "mine"}})
	if len(m.pane.lines) != before+1 {
		t.Errorf("log line for the zoomed service was not appended")
	}

	// A different service in the same project.
	other := m.zoomIndex + 1
	m = sendFleet(m, hub.Msg{ID: m.zoomID, Inner: process.LogMsg{Index: other, Line: "theirs"}})
	if len(m.pane.lines) != before+1 {
		t.Errorf("log line from another service leaked into the pane")
	}

	// A different project entirely.
	m = sendFleet(m, hub.Msg{ID: hub.InstanceID("/somewhere/else"), Inner: process.LogMsg{Index: m.zoomIndex, Line: "elsewhere"}})
	if len(m.pane.lines) != before+1 {
		t.Errorf("log line from another project leaked into the pane")
	}
}

// Acting on a project that isn't running must explain itself rather than fail
// silently or, worse, act on the wrong thing.
func TestFleetActionsOnStoppedProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	dir := filepath.Join(root, "idle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, ".pairinrc.toml")
	if err := os.WriteFile(cfgPath, []byte("[project]\nname = \"Idle\"\n\n[[services]]\nname = \"web\"\ncmd = \"true\"\ndir = \".\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	h := hub.New()
	t.Cleanup(h.Close)
	// Discovered through the registry-free path: nothing running, nothing
	// registered, so seed the catalog directly.
	writeCatalog(t, cfgPath)
	h.Refresh()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v := h.Snapshot()
		if len(v) == 1 && len(v[0].Services) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	m := NewFleetModel(h)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = model.(FleetModel)

	m = sendFleet(m, key("r"))
	if !m.statusErr || !strings.Contains(m.status, "not running") {
		t.Errorf("restart on a stopped project reported %q (err=%v), want an explanation", m.status, m.statusErr)
	}

	m = sendFleet(m, key("z"))
	if m.zoomed {
		t.Error("zoomed into a project that isn't running")
	}
	if !m.statusErr {
		t.Errorf("zoom on a stopped project reported %q, want an explanation", m.status)
	}
}

// An empty host — nothing registered, nothing running — must render and accept
// keys rather than panic on an absent selection.
func TestFleetHandlesEmptyHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "st"))

	h := hub.New()
	t.Cleanup(h.Close)
	h.Refresh()

	m := NewFleetModel(h)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = model.(FleetModel)

	for _, k := range []string{"z", "r", "x", "s", "S", "down", "up", "left", "right", "esc", "/"} {
		m = sendFleet(m, key(k))
	}
	if view := m.View(); view == "" {
		t.Error("empty dashboard rendered nothing")
	}
}

// writeCatalog seeds the catalog with one project path.
func writeCatalog(t *testing.T, configPath string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "pairin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir catalog: %v", err)
	}
	body := fmt.Sprintf("[[project]]\n  name = \"idle\"\n  config = %q\n", configPath)
	if err := os.WriteFile(filepath.Join(dir, "projects.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
}

// Rendering must fit the terminal it was given, at fleet scale too.
func TestFleetViewFitsTerminal(t *testing.T) {
	const width, height = 100, 24
	m, _ := newFleet(t, width, height, func(root string) {
		fleetProject(t, root, "alpha", "one", "two", "three", "four")
		fleetProject(t, root, "beta", "five", "six", "seven")
	})

	lines := strings.Split(m.View(), "\n")
	if len(lines) > height {
		t.Errorf("view is %d lines, want at most %d", len(lines), height)
	}
	for i, line := range lines {
		if w := lipglossWidth(line); w > width {
			t.Errorf("line %d is %d wide, want at most %d: %q", i, w, width, line)
		}
	}
	if testing.Verbose() {
		t.Logf("\n%s", m.View())
	}
}
