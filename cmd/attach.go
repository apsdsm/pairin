package cmd

import (
	"fmt"

	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach a TUI to an already-running supervisor in this project",
	RunE:  runAttach,
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	holder := state.LockHolder(cfg.Path)
	if holder == 0 || !state.IsProcessAlive(holder) {
		return fmt.Errorf("no supervisor is running for this project (use `pairin` or `pairin up` to start one)")
	}
	return attachTUI(cfg)
}
