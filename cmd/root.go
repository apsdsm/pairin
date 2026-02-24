package cmd

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/tui"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "pairin",
	Short: "Local development process manager",
	RunE:  runDashboard,
}

func Execute() error {
	return rootCmd.Execute()
}

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	mgr := process.NewManager(cfg)
	model := tui.NewDashboardModel(cfg, mgr)

	p := tea.NewProgram(model, tea.WithAltScreen())
	mgr.SetProgram(p)

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	if finalErr := mgr.Error(); finalErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", finalErr)
	}

	return nil
}
