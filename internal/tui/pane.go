package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/process"
)

type ViewMode int

const (
	ViewSplit ViewMode = iota
	ViewFocus
)

// Pane wraps a viewport for displaying a single service's logs.
type Pane struct {
	service  *process.Service
	viewport viewport.Model
	lines    []string
	width    int
	height   int
}

func NewPane(svc *process.Service) Pane {
	vp := viewport.New(80, 10)
	vp.MouseWheelEnabled = true
	return Pane{
		service:  svc,
		viewport: vp,
	}
}

func (p *Pane) SetSize(width, height int) {
	p.width = width
	p.height = height
	// Reserve 1 line for the title bar
	contentHeight := height - 1
	if contentHeight < 1 {
		contentHeight = 1
	}
	p.viewport.Width = width
	p.viewport.Height = contentHeight
	p.updateContent()
}

func (p *Pane) AppendLine(line string) {
	p.lines = append(p.lines, line)
	p.updateContent()
}

func (p *Pane) SyncFromBuffer() {
	p.lines = p.service.GetLines()
	p.updateContent()
}

func (p *Pane) updateContent() {
	content := strings.Join(p.lines, "\n")
	p.viewport.SetContent(content)
	p.viewport.GotoBottom()
}

func (p *Pane) ScrollUp(n int) {
	p.viewport.LineUp(n)
}

func (p *Pane) ScrollDown(n int) {
	p.viewport.LineDown(n)
}

func (p *Pane) titleLine(active bool) string {
	svc := p.service
	nameColor := ServiceColor(svc.Config.Color)
	nameStyle := lipgloss.NewStyle().Foreground(nameColor).Bold(true)

	var statusStyle lipgloss.Style
	switch svc.Status {
	case process.StatusRunning:
		statusStyle = StatusRunning
	case process.StatusCrashed:
		statusStyle = StatusCrashed
	case process.StatusStarting:
		statusStyle = StatusStarting
	case process.StatusWaiting:
		statusStyle = StatusWaitingStyle
	default:
		statusStyle = StatusStopped
	}

	parts := []string{
		nameStyle.Render(svc.Config.Name),
		DimStyle.Render(svc.Branch),
		statusStyle.Render(svc.Status.String()),
	}

	// Show health indicator for running services with a healthcheck
	if svc.Status == process.StatusRunning && svc.Config.Healthcheck != "" {
		if svc.Healthy {
			parts = append(parts, StatusHealthy.Render("healthy"))
		} else {
			parts = append(parts, StatusUnhealthy.Render("unhealthy"))
		}
	}

	if svc.PID > 0 {
		parts = append(parts, DimStyle.Render(fmt.Sprintf("PID %d", svc.PID)))
	}

	return strings.Join(parts, "  ")
}

// RenderSplit renders the pane for split view with a border.
func (p *Pane) RenderSplit(active bool) string {
	title := p.titleLine(active)
	content := p.viewport.View()

	full := title + "\n" + content

	style := PaneBorderStyle
	if active {
		style = PaneBorderActiveStyle
	}

	return style.Width(p.width).Render(full)
}

// RenderFocus renders the pane for focused (full-screen) view.
func (p *Pane) RenderFocus() string {
	title := p.titleLine(true)
	content := p.viewport.View()
	return title + "\n" + content
}
