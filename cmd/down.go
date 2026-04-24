package cmd

import (
	"fmt"
	"time"

	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop all services and the supervisor for this project",
	RunE:  runDown,
}

func init() {
	rootCmd.AddCommand(downCmd)
}

func runDown(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	holder := state.LockHolder(cfg.Path)
	if holder == 0 || !state.IsProcessAlive(holder) {
		fmt.Println("No supervisor running for this project.")
		return nil
	}

	client, err := control.Dial(state.SocketPath(cfg.Path))
	if err != nil {
		return fmt.Errorf("connecting to supervisor: %w", err)
	}
	if err := client.Shutdown(); err != nil {
		return fmt.Errorf("sending shutdown: %w", err)
	}

	// Wait for the supervisor to actually exit (or time out).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !state.IsProcessAlive(holder) {
			_ = client.Close()
			fmt.Println("Stopped.")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = client.Close()
	return fmt.Errorf("supervisor did not exit within 15s (PID %d)", holder)
}
