package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show per-service status across all running pairin supervisors",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	insts, err := state.ListInstances()
	if err != nil {
		return fmt.Errorf("reading registry: %w", err)
	}
	if len(insts) == 0 {
		fmt.Println("No pairin instances running.")
		return nil
	}
	sort.Slice(insts, func(i, j int) bool {
		if insts[i].ProjectName != insts[j].ProjectName {
			return insts[i].ProjectName < insts[j].ProjectName
		}
		return insts[i].ConfigPath < insts[j].ConfigPath
	})

	for i, inst := range insts {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s  (%s)\n", projectLabel(inst), inst.ConfigPath)
		fmt.Printf("  supervisor=%d  uptime=%s  socket=%s\n",
			inst.SupervisorPID, formatUptime(time.Since(inst.StartedAt)), inst.SocketPath)

		snap, _ := state.Load(inst.ConfigPath)
		if snap == nil || len(snap.Services) == 0 {
			fmt.Println("  (no services recorded)")
			continue
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, s := range snap.Services {
			alive := "dead"
			if state.IsProcessAlive(s.PID) {
				alive = "running"
			}
			fmt.Fprintf(tw, "  %s\t%s\tPID %d\t%s\n", s.Name, alive, s.PID, s.LogFile)
		}
		tw.Flush()
	}
	return nil
}
