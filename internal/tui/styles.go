package tui

import "github.com/charmbracelet/lipgloss"

var (
	HeaderStyle = lipgloss.NewStyle().Bold(true)
	DimStyle    = lipgloss.NewStyle().Faint(true)
	ErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	PaneBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238"))

	PaneBorderActiveStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("4"))

	StatusRunning      = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true) // green
	StatusCrashed      = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true) // red
	StatusStopped      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))            // gray
	StatusStarting     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))            // yellow
	StatusWaitingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))            // magenta
	StatusRestarting   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))            // yellow

	StatusHealthy   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	StatusUnhealthy = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow

	// Pin markers sit on project headings, not on services, so they use a
	// colour no status glyph uses.
	PinnedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	UnpinnedStyle = lipgloss.NewStyle().Faint(true)

	FooterStyle = lipgloss.NewStyle().Faint(true)
)

// ColorMap maps color names from config to lipgloss colors.
var ColorMap = map[string]lipgloss.Color{
	"blue":    lipgloss.Color("4"),
	"green":   lipgloss.Color("2"),
	"yellow":  lipgloss.Color("3"),
	"red":     lipgloss.Color("1"),
	"cyan":    lipgloss.Color("6"),
	"magenta": lipgloss.Color("5"),
	"white":   lipgloss.Color("15"),
}

func ServiceColor(name string) lipgloss.Color {
	if c, ok := ColorMap[name]; ok {
		return c
	}
	return lipgloss.Color("15")
}
