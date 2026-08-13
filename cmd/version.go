package cmd

import (
	"fmt"

	"github.com/apsdsm/pairin/internal/crash"
	"github.com/spf13/cobra"
)

const Version = "0.4.0"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("pairin", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// Stamp crash reports with the version. Lives here rather than in the crash
	// package so that package can stay free of a dependency on cmd.
	crash.Version = Version
}
