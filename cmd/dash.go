package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/hub"
	"github.com/apsdsm/pairin/internal/tui"
	"github.com/spf13/cobra"
)

var dashCmd = &cobra.Command{
	Use:     "dash",
	Aliases: []string{"ui"},
	Short:   "Dashboard of every pairin project on this host",
	Long: "Open a dashboard showing every registered and running project at once:\n" +
		"a grid of services grouped by project, with zoom into any one service's logs.\n\n" +
		"Quitting the dashboard leaves every supervisor running.",
	RunE: runDash,
}

func init() {
	rootCmd.AddCommand(dashCmd)
}

func runDash(cmd *cobra.Command, args []string) error {
	h := hub.New()
	defer h.Close()
	h.Refresh()

	model := tui.NewFleetModel(h)
	// WithoutCatchPanics for the same reason as the project TUI: the built-in
	// handler exits zero with no record of what happened.
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithoutCatchPanics())
	h.SetSink(p)

	if err := runProgram(p); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
