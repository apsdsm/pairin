package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/process"
)

type viewState int

const (
	viewSplit viewState = iota
	viewGrid
	viewFocus
)

// minSplitPaneHeight is the shortest a log pane can get before stacking them
// stops being useful. Past that the split view is auto-degraded to the grid —
// twenty two-line viewports show nothing at all.
const minSplitPaneHeight = 6

// Backend is everything the TUI needs from whatever's actually running
// services — either a local process.Manager or a control.Client attached to
// a remote supervisor. Both satisfy this interface.
type Backend interface {
	ServiceList() []*process.Service
	StartAll() tea.Cmd
	RestartService(idx int) tea.Cmd
	StopAll()
	Shutdown() error
	SetProgram(p *tea.Program)
}

// LogClearer is the optional half of a Backend that can discard a service's
// history. A remote control.Client implements it.
type LogClearer interface {
	RequestClearLogs(service string) error
}

// Connection is the optional half of a Backend that can drop and be restored.
// A remote control.Client implements it; a local process.Manager has nothing
// to disconnect from and simply doesn't.
type Connection interface {
	Done() <-chan struct{}
	Reconnect() error
}

// DisconnectedMsg reports that the supervisor connection went away.
type DisconnectedMsg struct{}

// reconnectTickMsg fires when the backoff has elapsed and it's time to retry.
type reconnectTickMsg struct{}

// ReconnectedMsg reports that the supervisor connection is back.
type ReconnectedMsg struct{}

// ReconnectFailedMsg reports a failed attempt; the model backs off and retries.
type ReconnectFailedMsg struct{ Err error }

const (
	reconnectMinDelay = 250 * time.Millisecond
	reconnectMaxDelay = 5 * time.Second
)

type DashboardModel struct {
	cfg          *config.Config
	mgr          Backend
	conn         Connection // nil when the backend is local
	panes        []Pane
	grid         Grid
	width        int
	height       int
	view         viewState
	prevView     viewState // what to return to when leaving focus
	active       int       // selected service; shared by split, grid and focus
	focused      int       // focused pane index in focus view
	quitting     bool
	shuttingDown bool // true when quitting via 'D' rather than 'q'

	// viewChosen records that the user picked a view with 'v'. Until they do,
	// the model picks between split and grid based on how much room there is.
	viewChosen bool

	// filtering is true while '/' input is being typed.
	filtering   bool
	filterInput string

	// Connection state. The TUI stays up and keeps rendering the last known
	// service states while disconnected — the supervisor going away must not
	// take the terminal with it.
	disconnected bool
	retryDelay   time.Duration
	retryErr     error
}

// preloadHistoryLines is how many trailing log lines we pull from disk on
// attach so that reattaching to an already-running supervisor doesn't look
// like a blank screen.
const preloadHistoryLines = 500

func NewDashboardModel(cfg *config.Config, mgr Backend) DashboardModel {
	svcs := mgr.ServiceList()
	panes := make([]Pane, len(svcs))
	for i, svc := range svcs {
		panes[i] = NewPane(svc, i)
		panes[i].PreloadHistory(preloadHistoryLines)
	}

	grid := NewGrid()
	grid.SetCellStyle(RememberedCellStyle())

	model := DashboardModel{
		cfg:        cfg,
		mgr:        mgr,
		panes:      panes,
		grid:       grid,
		view:       viewSplit,
		prevView:   viewSplit,
		retryDelay: reconnectMinDelay,
	}
	if conn, ok := mgr.(Connection); ok {
		model.conn = conn
	}
	return model
}

func (m DashboardModel) Init() tea.Cmd {
	return tea.Batch(guarded("start-all", m.mgr.StartAll()), m.watchConnection())
}

// watchConnection blocks until the supervisor connection drops. Bubble Tea runs
// commands on their own goroutines, so parking one here costs nothing and saves
// the TUI from rendering a frozen world after the supervisor has gone.
func (m DashboardModel) watchConnection() tea.Cmd {
	conn := m.conn
	if conn == nil {
		return nil
	}
	return guarded("watch-connection", func() tea.Msg {
		<-conn.Done()
		return DisconnectedMsg{}
	})
}

func (m DashboardModel) reconnect() tea.Cmd {
	conn := m.conn
	if conn == nil {
		return nil
	}
	return func() tea.Msg {
		if err := conn.Reconnect(); err != nil {
			return ReconnectFailedMsg{Err: err}
		}
		return ReconnectedMsg{}
	}
}

// guarded wraps a command so a panic inside it produces a crash report instead
// of killing the process. Bubble Tea runs each command in its own goroutine,
// and pairin disables the framework's own panic catcher (which swallows the
// error and exits zero) in favour of writing a report we can read afterwards.
func guarded(context string, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() (msg tea.Msg) {
		defer crash.Guard("tui cmd: " + context)
		return cmd()
	}
}

func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Grid cells are rebuilt here rather than at render time: View receives a
	// copy of the model, so anything it computed would be discarded.
	if m.view == viewGrid {
		m.refreshGrid()
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.autoSelectView()
		m.recalcPaneSizes()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case process.LogMsg:
		if msg.Index >= 0 && msg.Index < len(m.panes) {
			m.panes[msg.Index].AppendLine(msg.Line)
		}
		return m, nil

	case process.StatusMsg:
		// Status is already updated in the Service struct by the manager.
		// Just trigger a re-render.
		return m, nil

	case process.AllStartedMsg:
		return m, nil

	case process.ServiceRestartedMsg:
		// Sync logs from buffer after restart
		if msg.Index >= 0 && msg.Index < len(m.panes) {
			m.panes[msg.Index].SyncFromBuffer()
		}
		return m, nil

	case process.HealthCheckMsg:
		// Health state is already updated in the Service struct.
		// Just trigger a re-render.
		return m, nil

	case process.LogsClearedMsg:
		// The pane holds its own copy of the lines, so emptying the service's
		// ring buffer isn't enough.
		if msg.Index >= 0 && msg.Index < len(m.panes) {
			m.panes[msg.Index].Clear()
		}
		return m, nil

	case DisconnectedMsg:
		if m.quitting || m.disconnected {
			return m, nil
		}
		m.disconnected = true
		m.retryDelay = reconnectMinDelay
		return m, tea.Tick(m.retryDelay, func(time.Time) tea.Msg { return reconnectTickMsg{} })

	case reconnectTickMsg:
		if m.quitting || !m.disconnected {
			return m, nil
		}
		return m, guarded("reconnect", m.reconnect())

	case ReconnectFailedMsg:
		if m.quitting || !m.disconnected {
			return m, nil
		}
		m.retryErr = msg.Err
		m.retryDelay *= 2
		if m.retryDelay > reconnectMaxDelay {
			m.retryDelay = reconnectMaxDelay
		}
		return m, tea.Tick(m.retryDelay, func(time.Time) tea.Msg { return reconnectTickMsg{} })

	case ReconnectedMsg:
		m.disconnected = false
		m.retryDelay = reconnectMinDelay
		m.retryErr = nil
		// Lines emitted during the outage went to disk, not to us; the ring
		// buffer we mirror is authoritative for what we can still show.
		for i := range m.panes {
			m.panes[i].SyncFromBuffer()
		}
		return m, m.watchConnection()
	}

	return m, nil
}

func (m DashboardModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Filter entry swallows every key, so a service named "quiet" can be typed
	// without 'q' detaching halfway through.
	if m.filtering {
		return m.handleFilterKey(msg)
	}

	switch key {
	case "q", "ctrl+c":
		// Detach: tear down the TUI, leave the supervisor (and services)
		// running. Use 'D' or `pairin down` to stop everything for real.
		if m.quitting {
			return m, nil
		}
		m.quitting = true
		return m, guarded("detach", func() tea.Msg {
			m.mgr.StopAll()
			return tea.QuitMsg{}
		})

	case "d":
		// Shutdown: stop every service and exit the supervisor.
		if m.quitting {
			return m, nil
		}
		m.quitting = true
		m.shuttingDown = true
		return m, guarded("shutdown", func() tea.Msg {
			_ = m.mgr.Shutdown()
			return tea.QuitMsg{}
		})

	case "tab":
		if m.view != viewFocus && len(m.panes) > 0 {
			m.active = (m.active + 1) % len(m.panes)
			m.syncGridSelection()
		}
		return m, nil

	case "shift+tab":
		if m.view != viewFocus && len(m.panes) > 0 {
			m.active = (m.active - 1 + len(m.panes)) % len(m.panes)
			m.syncGridSelection()
		}
		return m, nil

	case "z", "enter":
		// Zoom toggle: whatever view we were in <-> focus on the active service.
		if m.view == viewFocus {
			m.view = m.prevView
		} else {
			m.prevView = m.view
			m.view = viewFocus
			m.focused = m.active
		}
		m.recalcPaneSizes()
		return m, nil

	case "esc":
		if m.view == viewFocus {
			m.view = m.prevView
			m.recalcPaneSizes()
		} else if m.grid.Filter() != "" {
			m.grid.SetFilter("")
			m.syncActiveFromGrid()
		}
		return m, nil

	case "v":
		// Explicit view choice; stops the model second-guessing it on resize.
		m.viewChosen = true
		switch m.view {
		case viewGrid:
			m.view = viewSplit
		case viewSplit:
			m.view = viewGrid
			m.refreshGrid()
			m.syncGridSelection()
		case viewFocus:
			m.view = m.prevView
		}
		m.recalcPaneSizes()
		return m, nil

	case "/":
		if m.view == viewGrid {
			m.filtering = true
			m.filterInput = m.grid.Filter()
		}
		return m, nil

	case "b":
		if m.view == viewGrid {
			m.grid.CycleCellStyle()
			m.recalcPaneSizes()
		}
		return m, nil

	case "c":
		idx := m.activeIndex()
		svcs := m.mgr.ServiceList()
		if idx < 0 || idx >= len(svcs) {
			return m, nil
		}
		name := svcs[idx].View().Name
		return m, guarded("clear-logs", func() tea.Msg {
			if cl, ok := m.mgr.(LogClearer); ok {
				_ = cl.RequestClearLogs(name)
			}
			return nil
		})

	case "r":
		idx := m.activeIndex()
		svcs := m.mgr.ServiceList()
		if idx < 0 || idx >= len(m.panes) || idx >= len(svcs) {
			return m, nil
		}
		// Clear pane lines for the restarting service
		m.panes[idx] = NewPane(svcs[idx], idx)
		m.recalcPaneSizes()
		return m, guarded("restart", m.mgr.RestartService(idx))

	case "up", "k":
		if m.view == viewGrid {
			m.grid.Move(0, -1)
			m.syncActiveFromGrid()
			return m, nil
		}
		if idx := m.activeIndex(); idx >= 0 && idx < len(m.panes) {
			m.panes[idx].ScrollUp(3)
		}
		return m, nil

	case "down", "j":
		if m.view == viewGrid {
			m.grid.Move(0, 1)
			m.syncActiveFromGrid()
			return m, nil
		}
		if idx := m.activeIndex(); idx >= 0 && idx < len(m.panes) {
			m.panes[idx].ScrollDown(3)
		}
		return m, nil

	case "left", "h":
		if m.view == viewGrid {
			m.grid.Move(-1, 0)
			m.syncActiveFromGrid()
		}
		return m, nil

	case "right", "l":
		if m.view == viewGrid {
			m.grid.Move(1, 0)
			m.syncActiveFromGrid()
		}
		return m, nil

	default:
		// Number keys for focusing
		for i := range m.panes {
			if key == fmt.Sprintf("%d", i+1) {
				m.view = viewFocus
				m.focused = i
				m.active = i
				m.recalcPaneSizes()
				return m, nil
			}
		}
	}

	return m, nil
}

// handleFilterKey runs while '/' input is active.
func (m DashboardModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filterInput = ""
		m.grid.SetFilter("")
		m.syncActiveFromGrid()
	case "enter":
		m.filtering = false
	case "backspace":
		if n := len(m.filterInput); n > 0 {
			m.filterInput = m.filterInput[:n-1]
			m.grid.SetFilter(m.filterInput)
			m.syncActiveFromGrid()
		}
	default:
		if len(msg.Runes) > 0 {
			m.filterInput += string(msg.Runes)
			m.grid.SetFilter(m.filterInput)
			m.syncActiveFromGrid()
		}
	}
	return m, nil
}

func (m *DashboardModel) activeIndex() int {
	if m.view == viewFocus {
		return m.focused
	}
	return m.active
}

// refreshGrid rebuilds the grid's cells from current service state. The project
// TUI is a single group; the fleet dashboard will supply one per project.
func (m *DashboardModel) refreshGrid() {
	m.grid.SetGroups([]GridGroup{{Cells: GridCellsFor(m.mgr.ServiceList())}})
}

// syncGridSelection points the grid at whatever service is currently active.
func (m *DashboardModel) syncGridSelection() {
	svcs := m.mgr.ServiceList()
	if m.active >= 0 && m.active < len(svcs) {
		m.grid.SelectName(svcs[m.active].View().Name)
	}
}

// syncActiveFromGrid is the inverse: the grid moved, so bring the shared
// selection with it. Going through names rather than indices keeps the two in
// step even when a filter is hiding part of the list.
func (m *DashboardModel) syncActiveFromGrid() {
	name := m.grid.SelectedName()
	if name == "" {
		return
	}
	for i, svc := range m.mgr.ServiceList() {
		if svc.View().Name == name {
			m.active = i
			return
		}
	}
}

// autoSelectView picks between split and grid based on available height, unless
// the user has made the choice themselves with 'v'.
func (m *DashboardModel) autoSelectView() {
	if m.viewChosen || m.view == viewFocus || len(m.panes) == 0 || m.height == 0 {
		return
	}
	available := m.height - 2 // header + footer
	if available/len(m.panes) < minSplitPaneHeight {
		if m.view != viewGrid {
			m.view = viewGrid
			m.refreshGrid()
			m.syncGridSelection()
		}
	} else {
		m.view = viewSplit
	}
	m.prevView = m.view
}

func (m *DashboardModel) recalcPaneSizes() {
	if m.width == 0 || m.height == 0 {
		return
	}

	// Chrome is a fixed height so nothing reflows when a status appears: the
	// header, the status line and the key hints, plus the glyph key in grid
	// view (which sits at the top, where it doesn't move).
	availableHeight := m.height - 3
	if m.view == viewGrid {
		availableHeight--
	}
	if availableHeight < 1 {
		availableHeight = 1
	}

	if m.view == viewGrid {
		m.grid.SetSize(m.width, availableHeight)
		return
	}

	if m.view == viewFocus {
		if m.focused >= 0 && m.focused < len(m.panes) {
			m.panes[m.focused].SetSize(m.width, availableHeight)
		}
	} else {
		n := len(m.panes)
		if n == 0 {
			return
		}
		// Each pane gets border (2 lines top+bottom) plus content
		// Distribute height evenly, give remainder to last pane
		paneHeight := availableHeight / n
		for i := range m.panes {
			h := paneHeight
			if i == n-1 {
				h = availableHeight - paneHeight*(n-1)
			}
			// Subtract 2 for border top+bottom
			innerHeight := h - 2
			if innerHeight < 2 {
				innerHeight = 2
			}
			m.panes[i].SetSize(m.width-2, innerHeight)
		}
	}
}

func (m DashboardModel) View() string {
	if m.width == 0 {
		return "Starting..."
	}

	var b strings.Builder

	// In grid view the glyph key goes at the top, where it stays put rather
	// than sliding down the screen as projects gain rows.
	chrome := 3
	if m.view == viewGrid {
		b.WriteString(GridLegend(m.width))
		b.WriteString("\n")
		chrome++
	}

	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	var content string
	switch {
	case m.view == viewGrid:
		content = m.grid.View()
	case m.view == viewFocus && m.focused >= 0 && m.focused < len(m.panes):
		content = m.panes[m.focused].RenderFocus()
	default:
		var panes strings.Builder
		for i := range m.panes {
			panes.WriteString(m.panes[i].RenderSplit(i == m.active))
			if i < len(m.panes)-1 {
				panes.WriteString("\n")
			}
		}
		content = panes.String()
	}
	// Pad to a fixed height so the footer doesn't walk up and down.
	if pad := (m.height - chrome) - (strings.Count(content, "\n") + 1); pad > 0 {
		content += strings.Repeat("\n", pad)
	}
	b.WriteString(content)

	b.WriteString("\n")
	b.WriteString(m.renderStatus())
	b.WriteString("\n")
	b.WriteString(m.renderKeys())

	return b.String()
}

func (m DashboardModel) renderHeader() string {
	// Each pane's title bar carries its own status/name/health/PID, so in the
	// pane views the header just shows the project name. The grid has no title
	// bars, so it gets the tally instead.
	name := HeaderStyle.Render(m.cfg.Project.Name)
	if m.view != viewGrid {
		return name
	}

	cells := GridCellsFor(m.mgr.ServiceList())
	up, total := SummarizeCells(cells)
	summary := DimStyle.Render(fmt.Sprintf("%d services · %d up", total, up))
	if f := m.grid.Filter(); f != "" {
		summary += "  " + StatusStarting.Render(fmt.Sprintf("filter: %s", f))
	}
	return name + "  " + summary
}

// renderStatus is the line above the key hints — transient state, always
// present even when empty so the layout doesn't shift when a message appears.
func (m DashboardModel) renderStatus() string {
	switch {
	case m.quitting:
		msg := "Detaching..."
		if m.shuttingDown {
			msg = "Shutting down..."
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render(msg)
	case m.disconnected:
		msg := fmt.Sprintf("supervisor unreachable — reconnecting in %s", m.retryDelay.Round(100*time.Millisecond))
		if m.retryErr != nil {
			msg += fmt.Sprintf(" (%v)", m.retryErr)
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("⚠ " + msg)
	case m.filtering:
		return HeaderStyle.Render("/" + m.filterInput)
	default:
		return ""
	}
}

// renderKeys is the bottom line, and stays visible while a status is showing.
func (m DashboardModel) renderKeys() string {
	if m.filtering {
		return hints(m.width, "enter accept", "esc clear")
	}
	switch m.view {
	case viewGrid:
		return hints(m.width, "↑↓←→", "z zoom", "r restart", "q detach", "d down", "c clear", "b cells", "/ filter", "v split")
	case viewFocus:
		return hints(m.width, "↑↓ scroll", "r restart", "z back", "q detach", "d down", "c clear logs")
	default:
		return hints(m.width, "tab cycle", "r restart", "z zoom", "q detach", "d down", "c clear", "v grid")
	}
}
