package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List all running pairin supervisors on this host",
	RunE:  runLs,
}

func init() {
	rootCmd.AddCommand(lsCmd)
}

func runLs(cmd *cobra.Command, args []string) error {
	insts, err := state.ListInstances()
	if err != nil {
		return fmt.Errorf("reading registry: %w", err)
	}
	if len(insts) == 0 {
		fmt.Println("No pairin instances running.")
		return nil
	}

	// Stable ordering: by project name, falling back to config path.
	sort.Slice(insts, func(i, j int) bool {
		if insts[i].ProjectName != insts[j].ProjectName {
			return insts[i].ProjectName < insts[j].ProjectName
		}
		return insts[i].ConfigPath < insts[j].ConfigPath
	})

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSUP PID\tUPTIME\tSTATUS\tCONFIG")
	for _, inst := range insts {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
			projectLabel(inst),
			inst.SupervisorPID,
			formatUptime(time.Since(inst.StartedAt)),
			summarize(inst),
			inst.ConfigPath,
		)
	}
	return tw.Flush()
}

// summarize reads the per-project state.json (if any) and counts services
// that are still alive vs. recorded-but-dead. If there's no state.json we
// fall back to "-".
func summarize(inst state.Instance) string {
	snap, err := state.Load(inst.ConfigPath)
	if err != nil || snap == nil {
		return "-"
	}
	var running, dead int
	for _, s := range snap.Services {
		if state.IsProcessAlive(s.PID) {
			running++
		} else {
			dead++
		}
	}
	if running == 0 && dead == 0 {
		return "0 services"
	}
	parts := []string{fmt.Sprintf("%d running", running)}
	if dead > 0 {
		parts = append(parts, fmt.Sprintf("%d dead", dead))
	}
	return strings.Join(parts, ", ")
}

// projectLabel prefers the toml project name; falls back to the config's
// parent directory if the project name is empty.
func projectLabel(inst state.Instance) string {
	if inst.ProjectName != "" {
		return inst.ProjectName
	}
	// Derive from config path.
	dir := inst.ConfigPath
	if idx := strings.LastIndex(dir, "/"); idx > 0 {
		dir = dir[:idx]
		if idx2 := strings.LastIndex(dir, "/"); idx2 >= 0 {
			return dir[idx2+1:]
		}
		return dir
	}
	return inst.ConfigPath
}

// formatUptime renders a duration as a short human string (e.g. "2h13m", "5d").
func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}
