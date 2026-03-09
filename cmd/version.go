package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

const Version = "0.2.0"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("pairin", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
