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
	rootCmd.AddCommand(upCmd)
}
