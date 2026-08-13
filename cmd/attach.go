package cmd

import (
	"fmt"

	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach [project]",
	Short: "Attach a TUI to an already-running supervisor",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runAttach,
}

func init() {
	attachCmd.Flags().StringVarP(&configFlag, "config", "c", "", "path to a .pairinrc.toml (defaults to searching cwd up to root)")
	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	cfg, err := resolveConfig(cmd, args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	holder := state.LockHolder(cfg.Path)
	if holder == 0 || !state.IsProcessAlive(holder) {
		return fmt.Errorf("no supervisor is running for this project (use `pairin` or `pairin up` to start one)")
	}
	return attachTUI(cfg)
}
