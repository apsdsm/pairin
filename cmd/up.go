package cmd

import "github.com/spf13/cobra"

// upCmd is an explicit alias for the default command so `pairin up` works
// the same as `pairin` alone.
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start or attach to the supervisor for this project (default)",
	RunE:  runUp,
}

func init() {
	upCmd.Flags().BoolVarP(&detachFlag, "detach", "d", false, "start the supervisor in the background and exit without attaching a TUI")
	upCmd.Flags().StringVarP(&configFlag, "config", "c", "", "path to a .pairinrc.toml (defaults to searching cwd up to root)")
	upCmd.Flags().BoolVar(&clearLogsFlag, "clear-logs", false, "delete existing service logs before starting, so the TUI doesn't preload old history")
	rootCmd.AddCommand(upCmd)
}
