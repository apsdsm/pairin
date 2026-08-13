package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/browse"
	"github.com/apsdsm/pairin/internal/catalog"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/hub"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
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

	// Project picker. It takes over the bottom of the screen rather than the
	// whole of it, so the dashboard it's adding to stays in view.
	browsing  bool
	browseDir string
	entries   []browse.Entry
	browseSel int

	// status is a transient line for the result of an action, cleared on the
	// next keystroke.
	status    string
	statusErr bool

	quitting bool
}

func NewFleetModel(h *hub.Hub) FleetModel {
	m := FleetModel{hub: h, grid: NewGrid()}
	m.grid.SetCellStyle(RememberedCellStyle())
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
		switch inner := msg.Inner.(type) {
		case process.LogMsg:
			if m.zoomed && msg.ID == m.zoomID && inner.Index == m.zoomIndex {
				m.pane.AppendLine(inner.Line)
			}
			return m, nil
		case process.LogsClearedMsg:
			if m.zoomed && msg.ID == m.zoomID && inner.Index == m.zoomIndex {
				m.pane.Clear()
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
	if m.browsing {
		return m.handleBrowseKey(msg)
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

	case "a":
		if !m.zoomed {
			return m.openBrowser(), nil
		}
		return m, nil

	case "p":
		return m.togglePin()

	case "c":
		return m.act("clear logs for", func(id hub.InstanceID, svc string) error {
			return m.hub.ClearLogs(id, svc)
		})

	case "C":
		// Whole project, mirroring the s/S and x/S pairing.
		inst, _, ok := m.selection()
		if !ok {
			return m, nil
		}
		if inst.State != hub.StateConnected {
			return m.fail(fmt.Sprintf("%s is not running", inst.Label())), nil
		}
		if err := m.hub.ClearLogs(inst.ID, ""); err != nil {
			return m.fail(fmt.Sprintf("could not clear logs for %s: %v", inst.Label(), err)), nil
		}
		return m.note(fmt.Sprintf("cleared all logs in %s", inst.Label())), nil
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

// ----- project picker -----

// openBrowser opens the picker where it was last left.
func (m FleetModel) openBrowser() FleetModel {
	dir := state.LoadUI().BrowseDir
	if dir == "" {
		dir = browse.Home()
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = browse.Home()
	}

	m.browsing = true
	m.status = ""
	m.statusErr = false
	m = m.readDir(dir)
	m.resize()
	return m
}

func (m FleetModel) closeBrowser() FleetModel {
	m.browsing = false
	m.entries = nil
	m.resize()
	return m
}

// readDir lists a directory into the picker, flagging configs already in the
// catalog so they aren't added twice.
func (m FleetModel) readDir(dir string) FleetModel {
	known := map[string]bool{}
	for _, inst := range m.hub.Snapshot() {
		known[inst.ConfigPath] = true
	}
	if cat, err := catalog.Load(); err == nil {
		for _, p := range cat.Projects {
			known[p.Config] = true
		}
	}

	entries, err := browse.Read(dir, func(path string) bool { return known[path] })
	if err != nil {
		m.status = fmt.Sprintf("cannot open %s: %v", dir, err)
		m.statusErr = true
		return m
	}

	m.browseDir = dir
	m.entries = entries
	m.browseSel = 0

	ui := state.LoadUI()
	ui.BrowseDir = dir
	_ = state.SaveUI(ui)
	return m
}

func (m FleetModel) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "a", "q", "ctrl+c":
		// 'q' closes the picker rather than the dashboard: a mode you opened by
		// accident shouldn't be able to quit out from under you.
		return m.closeBrowser(), nil

	case "up", "k":
		if m.browseSel > 0 {
			m.browseSel--
		}
		return m, nil

	case "down", "j":
		if m.browseSel < len(m.entries)-1 {
			m.browseSel++
		}
		return m, nil

	case "left", "h":
		return m.ascend(), nil

	case "right", "l", "enter":
		return m.choose()
	}
	return m, nil
}

func (m FleetModel) ascend() FleetModel {
	parent := filepath.Dir(m.browseDir)
	if parent == m.browseDir {
		return m
	}
	from := filepath.Base(m.browseDir)
	m = m.readDir(parent)
	// Land on the directory just left, so going up and back down is symmetric.
	for i, e := range m.entries {
		if e.IsDir && !e.IsParent && strings.TrimSuffix(e.Name, string(filepath.Separator)) == from {
			m.browseSel = i
			break
		}
	}
	return m
}

// choose descends into a directory, or adds the selected config.
func (m FleetModel) choose() (tea.Model, tea.Cmd) {
	if m.browseSel < 0 || m.browseSel >= len(m.entries) {
		return m, nil
	}
	e := m.entries[m.browseSel]

	if e.IsParent {
		return m.ascend(), nil
	}
	if e.IsDir {
		return m.readDir(e.Path), nil
	}

	if e.Added {
		return m.fail(fmt.Sprintf("%s is already in the list", displayOr(e.Project, e.Name))), nil
	}
	name, err := m.hub.AddProject(e.Path)
	if err != nil {
		return m.fail(fmt.Sprintf("could not add %s: %v", e.Name, err)), nil
	}

	m = m.closeBrowser()
	m.refresh()
	return m.note(fmt.Sprintf("added %s — start it with s, or `pairin up %s`", displayOr(e.Project, e.Name), name)), nil
}

func displayOr(project, fallback string) string {
	if project != "" {
		return project
	}
	return fallback
}

// browserHeight is how much of the screen the picker takes: enough for its
// entries, never more than half the content area.
func (m FleetModel) browserHeight() int {
	if !m.browsing {
		return 0
	}
	const chrome = 2 // separator rule and the path line

	available := m.height - fleetChromeLines
	max := available / 2
	if max < 6 {
		max = 6
	}

	want := len(m.entries) + chrome
	if want > max {
		want = max
	}
	if want < 4 {
		want = 4
	}
	if want > available {
		want = available
	}
	return want
}

// renderBrowser draws the picker panel, separated from the dashboard above it
// by a titled rule.
func (m FleetModel) renderBrowser(height int) string {
	// Built as separate pieces rather than sliced: the rule is box-drawing
	// characters, three bytes each, so byte offsets cut through them.
	title := " add a project "
	const lead = "──"
	trail := ""
	if pad := m.width - lipgloss.Width(title) - lipgloss.Width(lead); pad > 0 {
		trail = strings.Repeat("─", pad)
	}

	lines := []string{DimStyle.Render(lead) + HeaderStyle.Render(title) + DimStyle.Render(trail)}
	lines = append(lines, DimStyle.Render(shortenPath(m.browseDir)))

	rows := height - len(lines)
	if rows < 1 {
		rows = 1
	}

	// Keep the selection in view, derived from the selection rather than
	// carried between renders — View works on a copy of the model.
	start := 0
	if m.browseSel >= rows {
		start = m.browseSel - rows + 1
	}
	if max := len(m.entries) - rows; start > max {
		start = max
	}
	if start < 0 {
		start = 0
	}

	for i := start; i < len(m.entries) && len(lines) < height; i++ {
		lines = append(lines, m.renderEntry(m.entries[i], i == m.browseSel))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m FleetModel) renderEntry(e browse.Entry, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}

	name := e.Name
	style := lipgloss.NewStyle()
	switch {
	case e.IsConfig && e.Added:
		style = style.Faint(true)
	case e.IsConfig:
		style = style.Foreground(lipgloss.Color("6"))
	case selected:
		style = style.Bold(true)
	}

	// The right-hand note: what a config is, or how many a directory holds.
	note := ""
	switch {
	case e.IsConfig:
		note = e.Project
		if e.Added {
			note = strings.TrimSpace(note + "  already added")
		}
	case e.IsDir && e.Configs > 0:
		note = plural(e.Configs, "config")
	}

	left := marker + style.Render(name)
	if note == "" {
		return left
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(note) - 1
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + DimStyle.Render(note)
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
	if service == "" {
		return m.fail(fmt.Sprintf("%s has no services to %s", inst.Label(), verb)), nil
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

// togglePin decides whether a project keeps its place in the dashboard once it
// stops running. Unpinning is how a project that was only ever started to check
// something gets out of the way again.
func (m FleetModel) togglePin() (tea.Model, tea.Cmd) {
	inst, _, ok := m.selection()
	if !ok {
		return m, nil
	}

	pin := !inst.Pinned
	if err := m.hub.SetPinned(inst.ID, pin); err != nil {
		return m.fail(fmt.Sprintf("could not pin %s: %v", inst.Label(), err)), nil
	}
	m.hub.Refresh()
	m.refresh()

	if pin {
		return m.note(fmt.Sprintf("pinned %s — it will stay listed when stopped", inst.Label())), nil
	}
	if inst.State == hub.StateConnected {
		return m.note(fmt.Sprintf("unpinned %s — it will drop off this list when it stops", inst.Label())), nil
	}
	return m.note(fmt.Sprintf("unpinned %s", inst.Label())), nil
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
			Marker:   PinMarker(inst.Pinned),
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
			// A project with nothing to list still needs to be reachable —
			// otherwise an entry whose config has gone missing can be seen but
			// never selected, and so never pinned, started, or got rid of. The
			// placeholder carries an empty service name, which the actions read
			// as "this is about the project, not a service".
			grp.Cells = append(grp.Cells, GridCell{
				Key:    fleetKey(inst.ID, ""),
				Name:   "(no services)",
				Status: process.StatusStopped,
			})
		}
		groups = append(groups, grp)
	}
	m.grid.SetGroups(groups)
}

// fleetChromeLines is how many lines the frame costs: the glyph key, the
// summary header, the status line and the key hints. Fixed, so that content
// never reflows when a status message appears.
const fleetChromeLines = 4

func (m *FleetModel) resize() {
	if m.width == 0 || m.height == 0 {
		return
	}
	available := m.height - fleetChromeLines - m.browserHeight()
	if available < 1 {
		available = 1
	}
	if m.zoomed {
		m.pane.SetSize(m.width, available)
		return
	}
	m.grid.SetSize(m.width, available)
}

func (m FleetModel) View() string {
	if m.width == 0 {
		return "Starting..."
	}

	// The glyph key sits at the top, where it stays put. Below the grid it
	// moved down the screen every time a project gained a row.
	body := FleetLegend(m.width) + "\n" + m.renderHeader() + "\n"

	content := m.grid.View()
	if m.zoomed {
		content = m.pane.RenderFocus()
	}
	// Pad to a fixed height so the footer doesn't walk up and down.
	panel := m.browserHeight()
	if pad := (m.height - fleetChromeLines - panel) - (strings.Count(content, "\n") + 1); pad > 0 {
		content += strings.Repeat("\n", pad)
	}
	if panel > 0 {
		content += "\n" + m.renderBrowser(panel)
	}

	return body + content + "\n" + m.renderStatus() + "\n" + m.renderKeys()
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

	summary := fmt.Sprintf("%s (%d up) · %s · %d running",
		plural(len(m.instances), "project"), running, plural(services, "service"), up)
	head := HeaderStyle.Render("pairin") + "  " + DimStyle.Render(summary)
	if f := m.grid.Filter(); f != "" {
		head += "  " + StatusStarting.Render("filter: "+f)
	}
	return head
}

// renderStatus is the line above the key hints. It carries the result of the
// last action, or the filter being typed. It is always present, even when
// empty, so that a message appearing doesn't push the rest of the screen up.
func (m FleetModel) renderStatus() string {
	switch {
	case m.quitting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render("Closing…")
	case m.filtering:
		return HeaderStyle.Render("/" + m.filterInput)
	case m.status != "":
		if m.statusErr {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("⚠ " + m.status)
		}
		return DimStyle.Render(m.status)
	default:
		return ""
	}
}

// renderKeys is the bottom line: what you can press. It stays visible while a
// status message is showing, on its own line.
func (m FleetModel) renderKeys() string {
	switch {
	case m.browsing:
		return hints(m.width, "↑↓ move", "enter open/add", "← up", "esc close")
	case m.filtering:
		return hints(m.width, "enter accept", "esc clear")
	case m.zoomed:
		return hints(m.width, "↑↓ scroll", "r restart", "c clear logs", "z back", "q quit")
	default:
		return hints(m.width,
			"↑↓←→", "z logs", "r restart", "x stop", "s start", "S down",
			"a add", "p pin", "q quit", "c clear", "b cells", "/ filter")
	}
}

// instanceSubtitle is the line beside a project's name: where it lives and what
// state it's in.
func instanceSubtitle(inst hub.InstanceView) string {
	// Pin state is shown by the marker on the heading, not repeated here.
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

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
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
