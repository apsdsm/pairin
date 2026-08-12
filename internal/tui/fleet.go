package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/hub"
	"github.com/apsdsm/pairin/internal/process"
)

// fleetRefreshInterval is how often the dashboard re-reads the catalog and the
// registry, so projects started elsewhere appear without a keystroke.
const fleetRefreshInterval = 2 * time.Second

// fleetTickMsg drives that refresh.
type fleetTickMsg struct{}

// FleetModel is the host-wide dashboard: every project's services in one grid,
// with zoom into any one service's logs.
//
// It renders entirely from hub.Snapshot() value copies. The hub's per-instance
// goroutines are mutating the underlying state continuously, and the render
// goroutine must not be reading it directly.
type FleetModel struct {
	hub  *hub.Hub
	grid Grid

	width  int
	height int

	instances []hub.InstanceView

	// Zoom state. When zoomed, exactly one service's logs are streaming.
	zoomed      bool
	zoomID      hub.InstanceID
	zoomService string
	zoomIndex   int // index into that instance's mirrored service list
	pane        Pane

	filtering   bool
	filterInput string

	// status is a transient line for the result of an action, cleared on the
	// next keystroke.
	status    string
	statusErr bool

	quitting bool
}

func NewFleetModel(h *hub.Hub) FleetModel {
	m := FleetModel{hub: h, grid: NewGrid()}
	m.refresh()
	return m
}

func (m FleetModel) Init() tea.Cmd {
	return tea.Batch(
		guarded("fleet-refresh", func() tea.Msg { return fleetTickMsg{} }),
	)
}

func (m FleetModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case fleetTickMsg:
		m.hub.Refresh()
		m.refresh()
		return m, tea.Tick(fleetRefreshInterval, func(time.Time) tea.Msg { return fleetTickMsg{} })

	case hub.StateMsg:
		m.refresh()
		return m, nil

	case fleetErrMsg:
		return m.fail(msg.text), nil

	case hub.Msg:
		// Log lines only flow for the service being zoomed into; everything
		// else is a state change that the next refresh will pick up.
		if inner, ok := msg.Inner.(process.LogMsg); ok {
			if m.zoomed && msg.ID == m.zoomID && inner.Index == m.zoomIndex {
				m.pane.AppendLine(inner.Line)
			}
			return m, nil
		}
		m.refresh()
		return m, nil
	}

	return m, nil
}

func (m FleetModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		return m.handleFilterKey(msg)
	}

	m.status = ""
	m.statusErr = false

	switch msg.String() {
	case "q", "ctrl+c":
		// Quitting the dashboard never touches a supervisor. Stopping things is
		// always explicit: 'S' for a project, 'x' for a service.
		m.quitting = true
		return m, tea.Quit

	case "z", "enter":
		if m.zoomed {
			return m.unzoom(), nil
		}
		return m.zoom()

	case "esc":
		if m.zoomed {
			return m.unzoom(), nil
		}
		if m.grid.Filter() != "" {
			m.grid.SetFilter("")
		}
		return m, nil

	case "up", "k":
		if m.zoomed {
			m.pane.ScrollUp(3)
		} else {
			m.grid.Move(0, -1)
		}
		return m, nil

	case "down", "j":
		if m.zoomed {
			m.pane.ScrollDown(3)
		} else {
			m.grid.Move(0, 1)
		}
		return m, nil

	case "left", "h":
		if !m.zoomed {
			m.grid.Move(-1, 0)
		}
		return m, nil

	case "right", "l":
		if !m.zoomed {
			m.grid.Move(1, 0)
		}
		return m, nil

	case "tab":
		if !m.zoomed {
			m.grid.Move(1, 0)
		}
		return m, nil

	case "shift+tab":
		if !m.zoomed {
			m.grid.Move(-1, 0)
		}
		return m, nil

	case "/":
		if !m.zoomed {
			m.filtering = true
			m.filterInput = m.grid.Filter()
		}
		return m, nil

	case "b":
		if !m.zoomed {
			m.grid.CycleCellStyle()
			m.resize()
			return m.note("cells: " + m.grid.CellStyle().String()), nil
		}
		return m, nil

	case "r":
		return m.act("restart", func(id hub.InstanceID, svc string) error {
			return m.hub.RestartService(id, svc)
		})

	case "x":
		return m.act("stop", func(id hub.InstanceID, svc string) error {
			return m.hub.StopService(id, svc)
		})

	case "s":
		return m.start()

	case "S":
		return m.stopProject()
	}

	return m, nil
}

func (m FleetModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filterInput = ""
		m.grid.SetFilter("")
	case "enter":
		m.filtering = false
	case "backspace":
		if n := len(m.filterInput); n > 0 {
			m.filterInput = m.filterInput[:n-1]
			m.grid.SetFilter(m.filterInput)
		}
	default:
		if len(msg.Runes) > 0 {
			m.filterInput += string(msg.Runes)
			m.grid.SetFilter(m.filterInput)
		}
	}
	return m, nil
}

// selection resolves the highlighted cell back to a project and service.
func (m FleetModel) selection() (hub.InstanceView, string, bool) {
	key := m.grid.SelectedKey()
	if key == "" {
		return hub.InstanceView{}, "", false
	}
	id, service, ok := splitFleetKey(key)
	if !ok {
		return hub.InstanceView{}, "", false
	}
	for _, inst := range m.instances {
		if inst.ID == id {
			return inst, service, true
		}
	}
	return hub.InstanceView{}, "", false
}

// act runs a per-service operation on the selection and reports the outcome.
func (m FleetModel) act(verb string, fn func(hub.InstanceID, string) error) (tea.Model, tea.Cmd) {
	inst, service, ok := m.selection()
	if !ok {
		return m, nil
	}
	if inst.State != hub.StateConnected {
		return m.fail(fmt.Sprintf("%s is not running — press s to start it", inst.Label())), nil
	}
	if err := fn(inst.ID, service); err != nil {
		return m.fail(fmt.Sprintf("could not %s %s: %v", verb, service, err)), nil
	}
	return m.note(fmt.Sprintf("%s %s in %s", verb, service, inst.Label())), nil
}

// start starts either the whole project (if it's down) or the selected service.
func (m FleetModel) start() (tea.Model, tea.Cmd) {
	inst, _, ok := m.selection()
	if !ok {
		return m, nil
	}

	if inst.State != hub.StateConnected {
		label := inst.Label()
		id := inst.ID
		h := m.hub
		// Spawning waits for the supervisor to come up, so it runs off the
		// event loop rather than freezing the dashboard for a few seconds.
		return m.note(fmt.Sprintf("starting %s…", label)), guarded("start-project", func() tea.Msg {
			if err := h.StartProject(id); err != nil {
				return fleetErrMsg{fmt.Sprintf("could not start %s: %v", label, err)}
			}
			h.Refresh()
			return fleetTickMsg{}
		})
	}

	return m.act("start", func(id hub.InstanceID, svc string) error {
		return m.hub.StartService(id, svc)
	})
}

func (m FleetModel) stopProject() (tea.Model, tea.Cmd) {
	inst, _, ok := m.selection()
	if !ok {
		return m, nil
	}
	if inst.State != hub.StateConnected {
		return m.fail(fmt.Sprintf("%s is not running", inst.Label())), nil
	}
	if err := m.hub.StopProject(inst.ID); err != nil {
		return m.fail(fmt.Sprintf("could not stop %s: %v", inst.Label(), err)), nil
	}
	return m.note(fmt.Sprintf("shutting down %s", inst.Label())), nil
}

// fleetErrMsg reports a failure from a background command.
type fleetErrMsg struct{ text string }

func (m FleetModel) note(text string) FleetModel {
	m.status = text
	m.statusErr = false
	return m
}

func (m FleetModel) fail(text string) FleetModel {
	m.status = text
	m.statusErr = true
	return m
}

// zoom opens the selected service's logs, narrowing that supervisor's stream to
// just this one service so the rest of the host's output stays off the wire.
func (m FleetModel) zoom() (tea.Model, tea.Cmd) {
	inst, service, ok := m.selection()
	if !ok {
		return m, nil
	}
	if inst.State != hub.StateConnected {
		return m.fail(fmt.Sprintf("%s is not running — press s to start it", inst.Label())), nil
	}

	services := m.hub.Services(inst.ID)
	idx := -1
	for i, svc := range services {
		if svc.View().Name == service {
			idx = i
			break
		}
	}
	if idx < 0 {
		return m.fail(fmt.Sprintf("%s is no longer present in %s", service, inst.Label())), nil
	}

	if err := m.hub.SubscribeLogs(inst.ID, control.LogsOnly, service); err != nil {
		return m.fail(fmt.Sprintf("could not subscribe to %s: %v", service, err)), nil
	}

	m.zoomed = true
	m.zoomID = inst.ID
	m.zoomService = service
	m.zoomIndex = idx
	m.pane = NewPane(services[idx], idx)
	// The ring buffer only holds what arrived while we were subscribed, which
	// is nothing — fill from the log file on disk instead.
	m.pane.PreloadHistory(preloadHistoryLines)
	m.resize()
	return m, nil
}

func (m FleetModel) unzoom() FleetModel {
	if m.zoomed {
		_ = m.hub.SubscribeLogs(m.zoomID, control.LogsNone)
	}
	m.zoomed = false
	m.zoomID = ""
	m.zoomService = ""
	m.resize()
	return m
}

// refresh rebuilds the rendered state from the hub. Called from Update, never
// from View — View works on a copy of the model, so anything built there is
// discarded before the next keystroke.
func (m *FleetModel) refresh() {
	m.instances = m.hub.Snapshot()

	groups := make([]GridGroup, 0, len(m.instances))
	for _, inst := range m.instances {
		grp := GridGroup{
			Title:    inst.Label(),
			Subtitle: instanceSubtitle(inst),
		}
		for _, svc := range inst.Services {
			grp.Cells = append(grp.Cells, GridCell{
				Key:          fleetKey(inst.ID, svc.Name),
				Name:         svc.Name,
				Color:        svc.Color,
				Status:       svc.Status,
				Healthy:      svc.Healthy,
				HasHealth:    svc.HasHealth,
				RestartCount: svc.RestartCount,
				MaxRestarts:  svc.MaxRestarts,
				PID:          svc.PID,
				DependsOn:    svc.DependsOn,
			})
		}
		if len(grp.Cells) == 0 {
			grp.Note = "no services found in config"
		}
		groups = append(groups, grp)
	}
	m.grid.SetGroups(groups)
}

func (m *FleetModel) resize() {
	if m.width == 0 || m.height == 0 {
		return
	}
	available := m.height - 2 // header + footer
	if m.zoomed {
		m.pane.SetSize(m.width, available)
		return
	}
	m.grid.SetSize(m.width, available-2) // blank line + legend
}

func (m FleetModel) View() string {
	if m.width == 0 {
		return "Starting..."
	}

	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	if m.zoomed {
		b.WriteString(m.pane.RenderFocus())
	} else {
		body := m.grid.View() + "\n\n" + GridLegend()
		if pad := (m.height - 2) - (strings.Count(body, "\n") + 1); pad > 0 {
			body += strings.Repeat("\n", pad)
		}
		b.WriteString(body)
	}

	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

func (m FleetModel) renderHeader() string {
	if m.zoomed {
		label := m.zoomService
		for _, inst := range m.instances {
			if inst.ID == m.zoomID {
				label = inst.Label() + " · " + m.zoomService
				break
			}
		}
		return HeaderStyle.Render("pairin") + "  " + DimStyle.Render(label)
	}

	var services, up, running int
	for _, inst := range m.instances {
		if inst.State == hub.StateConnected {
			running++
		}
		for _, svc := range inst.Services {
			services++
			if svc.Status == process.StatusRunning {
				up++
			}
		}
	}

	summary := fmt.Sprintf("%d projects (%d up) · %d services · %d running",
		len(m.instances), running, services, up)
	head := HeaderStyle.Render("pairin") + "  " + DimStyle.Render(summary)
	if f := m.grid.Filter(); f != "" {
		head += "  " + StatusStarting.Render("filter: "+f)
	}
	return head
}

func (m FleetModel) renderFooter() string {
	if m.quitting {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render("Closing…")
	}
	if m.filtering {
		return HeaderStyle.Render("/"+m.filterInput) + FooterStyle.Render("  enter accept  esc clear")
	}
	if m.status != "" {
		style := DimStyle
		if m.statusErr {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		}
		return style.Render(m.status)
	}
	if m.zoomed {
		return FooterStyle.Render("↑↓ scroll  r restart  z back  q quit")
	}
	return FooterStyle.Render("↑↓←→ move  z logs  r restart  x stop  s start  S down  b cells  / filter  q quit")
}

// instanceSubtitle is the line beside a project's name: where it lives and what
// state it's in.
func instanceSubtitle(inst hub.InstanceView) string {
	parts := []string{shortenPath(filepath.Dir(inst.ConfigPath))}

	switch inst.State {
	case hub.StateConnected:
		if inst.SupervisorPID > 0 {
			parts = append(parts, fmt.Sprintf("sup %d", inst.SupervisorPID))
		}
		if !inst.StartedAt.IsZero() {
			parts = append(parts, FormatUptime(time.Since(inst.StartedAt)))
		}
	case hub.StateConnecting:
		parts = append(parts, "connecting…")
	case hub.StateUnreachable:
		parts = append(parts, "unreachable")
	default:
		parts = append(parts, "stopped — press s to start")
	}

	return strings.Join(parts, "  ")
}

// shortenPath abbreviates the user's home directory, which is otherwise the
// longest and least informative part of every path on screen.
func shortenPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// FormatUptime renders a duration as a short human string ("2h13m", "5d").
func FormatUptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// fleetKey identifies a service uniquely across projects: two projects can
// each have a service called "web".
func fleetKey(id hub.InstanceID, service string) string {
	return string(id) + "\x00" + service
}

func splitFleetKey(key string) (hub.InstanceID, string, bool) {
	i := strings.LastIndex(key, "\x00")
	if i < 0 {
		return "", "", false
	}
	return hub.InstanceID(key[:i]), key[i+1:], true
}
