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
	viewFocus
)

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
	cfg       *config.Config
	mgr       Backend
	conn      Connection // nil when the backend is local
	panes     []Pane
	width     int
	height    int
	view      viewState
	active    int // active pane index in split view
	focused   int // focused pane index in focus view
	quitting  bool
	shuttingDown bool // true when quitting via 'D' rather than 'q'

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

	model := DashboardModel{
		cfg:        cfg,
		mgr:        mgr,
		panes:      panes,
		view:       viewSplit,
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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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
		if m.view == viewSplit && len(m.panes) > 0 {
			m.active = (m.active + 1) % len(m.panes)
		}
		return m, nil

	case "shift+tab":
		if m.view == viewSplit && len(m.panes) > 0 {
			m.active = (m.active - 1 + len(m.panes)) % len(m.panes)
		}
		return m, nil

	case "z":
		// Zoom toggle: split <-> focus on the active pane.
		if m.view == viewFocus {
			m.view = viewSplit
		} else {
			m.view = viewFocus
			m.focused = m.active
		}
		m.recalcPaneSizes()
		return m, nil

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
		if idx := m.activeIndex(); idx >= 0 && idx < len(m.panes) {
			m.panes[idx].ScrollUp(3)
		}
		return m, nil

	case "down", "j":
		if idx := m.activeIndex(); idx >= 0 && idx < len(m.panes) {
			m.panes[idx].ScrollDown(3)
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

func (m *DashboardModel) activeIndex() int {
	if m.view == viewFocus {
		return m.focused
	}
	return m.active
}

func (m *DashboardModel) recalcPaneSizes() {
	if m.width == 0 || m.height == 0 {
		return
	}

	// Reserve 2 lines for header and 1 for footer
	headerHeight := 1
	footerHeight := 1
	availableHeight := m.height - headerHeight - footerHeight

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

	// Header
	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	// Main content
	if m.view == viewFocus && m.focused >= 0 && m.focused < len(m.panes) {
		b.WriteString(m.panes[m.focused].RenderFocus())
	} else {
		for i := range m.panes {
			b.WriteString(m.panes[i].RenderSplit(i == m.active))
			if i < len(m.panes)-1 {
				b.WriteString("\n")
			}
		}
	}

	// Footer
	b.WriteString("\n")
	b.WriteString(m.renderFooter())

	return b.String()
}

func (m DashboardModel) renderHeader() string {
	// Each pane's title bar carries its own status/name/health/PID, so the
	// header just shows the project name.
	return HeaderStyle.Render(m.cfg.Project.Name)
}

func (m DashboardModel) renderFooter() string {
	if m.quitting {
		msg := "Detaching..."
		if m.shuttingDown {
			msg = "Shutting down..."
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render(msg)
	}
	if m.disconnected {
		msg := fmt.Sprintf("supervisor unreachable — reconnecting in %s", m.retryDelay.Round(100*time.Millisecond))
		if m.retryErr != nil {
			msg += fmt.Sprintf(" (%v)", m.retryErr)
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("⚠ " + msg)
	}
	return FooterStyle.Render("tab cycle  r restart  z zoom  q detach  d down")
}
