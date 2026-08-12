package cmd

import "github.com/spf13/cobra"

// upCmd is an explicit alias for the default command so `pairin up` works
// the same as `pairin` alone.
var upCmd = &cobra.Command{
	Use:   "up [project]",
	Short: "Start or attach to the supervisor for a project (default)",
	Long: "Start or attach to the supervisor for a project.\n\n" +
		"With no argument, uses the .pairinrc.toml found from the current directory.\n" +
		"With a registered project name, works from anywhere — see `pairin projects`.",
	Args: cobra.MaximumNArgs(1),
	RunE: runUp,
}

func init() {
	upCmd.Flags().BoolVarP(&detachFlag, "detach", "d", false, "start the supervisor in the background and exit without attaching a TUI")
	upCmd.Flags().StringVarP(&configFlag, "config", "c", "", "path to a .pairinrc.toml (defaults to searching cwd up to root)")
	upCmd.Flags().BoolVar(&clearLogsFlag, "clear-logs", false, "delete existing service logs before starting, so the TUI doesn't preload old history")
	upCmd.Flags().BoolVar(&noRegisterFlag, "no-register", false, "don't add this project to the catalog")
	rootCmd.AddCommand(upCmd)
}
