package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/process"
)

type viewState int

const (
	viewSplit viewState = iota
	viewFocus
)

type DashboardModel struct {
	cfg     *config.Config
	mgr     *process.Manager
	panes   []Pane
	width   int
	height  int
	view    viewState
	active  int // active pane index in split view
	focused int // focused pane index in focus view
}

func NewDashboardModel(cfg *config.Config, mgr *process.Manager) DashboardModel {
	panes := make([]Pane, len(mgr.Services))
	for i, svc := range mgr.Services {
		panes[i] = NewPane(svc)
	}

	return DashboardModel{
		cfg:   cfg,
		mgr:   mgr,
		panes: panes,
		view:  viewSplit,
	}
}

func (m DashboardModel) Init() tea.Cmd {
	return m.mgr.StartAll()
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
	}

	return m, nil
}

func (m DashboardModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	switch key {
	case "q", "ctrl+c":
		m.mgr.StopAll()
		return m, tea.Quit

	case "tab":
		if m.view == viewSplit {
			m.active = (m.active + 1) % len(m.panes)
		}
		return m, nil

	case "shift+tab":
		if m.view == viewSplit {
			m.active = (m.active - 1 + len(m.panes)) % len(m.panes)
		}
		return m, nil

	case "a":
		m.view = viewSplit
		m.recalcPaneSizes()
		return m, nil

	case "r":
		idx := m.activeIndex()
		// Clear pane lines for the restarting service
		m.panes[idx] = NewPane(m.mgr.Services[idx])
		m.recalcPaneSizes()
		return m, m.mgr.RestartService(idx)

	case "up", "k":
		if m.view == viewFocus {
			m.panes[m.focused].ScrollUp(3)
		}
		return m, nil

	case "down", "j":
		if m.view == viewFocus {
			m.panes[m.focused].ScrollDown(3)
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
	if m.view == viewFocus {
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
	title := HeaderStyle.Render(m.cfg.Project.Name)

	var indicators []string
	for _, svc := range m.mgr.Services {
		color := ServiceColor(svc.Config.Color)
		var dot string
		switch svc.Status {
		case process.StatusRunning:
			dot = lipgloss.NewStyle().Foreground(color).Render("●")
		case process.StatusCrashed:
			dot = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("●")
		case process.StatusStarting:
			dot = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("○")
		default:
			dot = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("○")
		}
		indicators = append(indicators, dot+" "+DimStyle.Render(svc.Config.Short))
	}

	right := strings.Join(indicators, "  ")
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}

	return title + strings.Repeat(" ", gap) + right
}

func (m DashboardModel) renderFooter() string {
	var parts []string
	for i, svc := range m.mgr.Services {
		parts = append(parts, fmt.Sprintf("%d %s", i+1, svc.Config.Short))
	}

	hints := strings.Join(parts, "  ")
	extra := "  tab cycle  r restart  a split  q quit"

	return FooterStyle.Render(hints + extra)
}
