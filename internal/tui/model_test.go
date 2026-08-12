package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/process"
)

// fakeBackend stands in for a Manager or control.Client.
type fakeBackend struct {
	services  []*process.Service
	restarted []int
}

func (f *fakeBackend) ServiceList() []*process.Service { return f.services }
func (f *fakeBackend) StartAll() tea.Cmd               { return nil }
func (f *fakeBackend) StopAll()                        {}
func (f *fakeBackend) Shutdown() error                 { return nil }
func (f *fakeBackend) SetProgram(*tea.Program)         {}
func (f *fakeBackend) RestartService(idx int) tea.Cmd {
	f.restarted = append(f.restarted, idx)
	return nil
}

func newTestModel(t *testing.T, n int, width, height int) (DashboardModel, *fakeBackend) {
	t.Helper()

	be := &fakeBackend{}
	cfgServices := make([]config.Service, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("svc-%02d", i)
		cfgServices[i] = config.Service{Name: name}
		svc := process.NewMirrorService(name, "", "blue", "", "true", nil, false, 0)
		svc.ApplyStatus(process.StatusRunning, 1000+i, "main", 0)
		be.services = append(be.services, svc)
	}

	cfg := &config.Config{Project: config.Project{Name: "test"}, Services: cfgServices}
	m := NewDashboardModel(cfg, be)

	model, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return model.(DashboardModel), be
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(m DashboardModel, msgs ...tea.Msg) DashboardModel {
	for _, msg := range msgs {
		model, _ := m.Update(msg)
		m = model.(DashboardModel)
	}
	return m
}

// A handful of services in a normal terminal should still stack as log panes —
// the grid is for when that stops working, not a replacement for it.
func TestFewServicesStaySplit(t *testing.T) {
	m, _ := newTestModel(t, 3, 120, 40)
	if m.view != viewSplit {
		t.Errorf("view = %v, want split", m.view)
	}
}

// Twenty services in an ordinary terminal cannot each have a readable pane, so
// the model degrades to the grid on its own.
func TestManyServicesAutoDegradeToGrid(t *testing.T) {
	m, _ := newTestModel(t, 20, 120, 40)
	if m.view != viewGrid {
		t.Errorf("view = %v, want grid", m.view)
	}
}

// Once the user picks a view, resizing must not override the choice.
func TestExplicitViewChoiceSticks(t *testing.T) {
	m, _ := newTestModel(t, 20, 120, 40)
	m = send(m, key("v")) // grid -> split, explicitly
	if m.view != viewSplit {
		t.Fatalf("after 'v', view = %v, want split", m.view)
	}

	m = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.view != viewSplit {
		t.Errorf("resize overrode the explicit choice: view = %v, want split", m.view)
	}
}

func TestGridZoomFocusesSelection(t *testing.T) {
	m, _ := newTestModel(t, 20, 120, 40)

	m = send(m, key("right"), key("right")) // svc-00 -> svc-02
	if got := m.grid.SelectedName(); got != "svc-02" {
		t.Fatalf("selection = %q, want svc-02", got)
	}
	if m.active != 2 {
		t.Fatalf("active index = %d, want 2", m.active)
	}

	m = send(m, key("z"))
	if m.view != viewFocus {
		t.Fatalf("view = %v, want focus", m.view)
	}
	if m.focused != 2 {
		t.Errorf("focused pane = %d, want 2", m.focused)
	}

	// Leaving focus returns to the grid, not to split.
	m = send(m, key("z"))
	if m.view != viewGrid {
		t.Errorf("view after unzoom = %v, want grid", m.view)
	}
}

func TestGridRestartActsOnSelection(t *testing.T) {
	m, be := newTestModel(t, 20, 120, 40)
	m = send(m, key("right"), key("r"))

	if len(be.restarted) != 1 || be.restarted[0] != 1 {
		t.Errorf("restarted = %v, want [1]", be.restarted)
	}
}

// '/' must capture typing rather than letting it hit the global shortcuts —
// filtering for "quiet" should not detach on the 'q'.
func TestFilterInputSwallowsShortcuts(t *testing.T) {
	m, _ := newTestModel(t, 20, 120, 40)
	m = send(m, key("/"), key("q"))

	if !m.filtering {
		t.Fatal("'/' did not enter filter mode")
	}
	if m.quitting {
		t.Error("typing 'q' while filtering started a detach")
	}
	if m.filterInput != "q" {
		t.Errorf("filter input = %q, want %q", m.filterInput, "q")
	}

	m = send(m, key("esc"))
	if m.filtering || m.grid.Filter() != "" {
		t.Error("esc did not clear the filter")
	}
}

func TestFilterSelectsMatchingService(t *testing.T) {
	m, _ := newTestModel(t, 20, 120, 40)
	m = send(m, key("/"), key("1"), key("7"), key("enter"))

	if m.filtering {
		t.Error("enter did not leave filter entry")
	}
	if got := m.grid.SelectedName(); got != "svc-17" {
		t.Errorf("selection = %q, want svc-17", got)
	}
	if m.active != 17 {
		t.Errorf("active index = %d, want 17", m.active)
	}
}

// The rendered grid must fit the terminal it was given — no wrapping, no
// overflowing the height it was allotted.
func TestGridViewFitsTerminal(t *testing.T) {
	const width, height = 100, 24
	m, _ := newTestModel(t, 20, width, height)

	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Errorf("view is %d lines, want at most %d", len(lines), height)
	}
	for i, line := range lines {
		if w := lipglossWidth(line); w > width {
			t.Errorf("line %d is %d wide, want at most %d: %q", i, w, width, line)
		}
	}

	if testing.Verbose() {
		t.Logf("\n%s", view)
	}
}
