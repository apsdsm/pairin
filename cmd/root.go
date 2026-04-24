package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/apsdsm/pairin/internal/tui"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "pairin",
	Short: "Local development process manager",
	RunE:  runDashboard,
}

func Execute() error {
	return rootCmd.Execute()
}

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Decide how to handle any leftover state from a previous pairin session
	// before acquiring the lock — the user may choose to quit, in which case
	// we want to leave the old state untouched.
	adopted, err := resolveStaleState(cfg.Path)
	if err != nil {
		return err
	}

	if err := state.AcquireLock(cfg.Path); err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}

	mgr := process.NewManager(cfg)

	// Seed adopted services before the TUI starts so StartAll sees them.
	for _, s := range adopted {
		for i, svc := range mgr.Services {
			if svc.Config.Name == s.Name {
				mgr.AdoptService(i, s.PID, s.PGID, s.LogFile)
				break
			}
		}
	}

	model := tui.NewDashboardModel(cfg, mgr)

	p := tea.NewProgram(model, tea.WithAltScreen())
	mgr.SetProgram(p)

	if _, err := p.Run(); err != nil {
		_ = state.ReleaseLock(cfg.Path)
		return fmt.Errorf("TUI error: %w", err)
	}

	if finalErr := mgr.Error(); finalErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", finalErr)
	}

	return nil
}

// resolveStaleState inspects existing .pairin/ state for a previous pairin
// session whose services may still be running. Returns the slice of services
// the caller should adopt (possibly empty). If the user chooses to quit, the
// function calls os.Exit directly so the caller doesn't need to unwind.
func resolveStaleState(configPath string) ([]state.ServiceState, error) {
	holder := state.LockHolder(configPath)
	liveHolder := holder > 0 && state.IsProcessAlive(holder)
	if liveHolder {
		return nil, fmt.Errorf("pairin is already running in this project (PID %d). Quit that instance first", holder)
	}

	snap, err := state.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading prior state: %w", err)
	}
	if snap == nil {
		return nil, nil
	}

	live := snap.LiveServices()
	if len(live) == 0 {
		// Previous run left state behind but nothing is actually running — clean up silently.
		_ = state.Clear(configPath)
		_ = state.ReleaseLock(configPath)
		return nil, nil
	}

	fmt.Println()
	fmt.Println("A previous pairin session exited without cleaning up.")
	fmt.Printf("%d service(s) still running:\n", len(live))
	for _, s := range live {
		fmt.Printf("  %-20s PID %d\n", s.Name, s.PID)
	}
	fmt.Println()
	fmt.Println("  [A]dopt       take over the running processes (read logs, stop on quit)")
	fmt.Println("  [R]estart     kill the orphans and start fresh")
	fmt.Println("  [Q]uit        exit without touching them")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("choice [a/r/q]: ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading choice: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "a", "adopt":
			return live, nil
		case "r", "restart":
			killOrphans(live)
			_ = state.Clear(configPath)
			_ = state.ReleaseLock(configPath)
			return nil, nil
		case "q", "quit", "":
			os.Exit(0)
		}
	}
}

// killOrphans sends SIGINT then SIGKILL to each orphan's process group.
// Best-effort: errors are ignored because the process may exit between checks.
func killOrphans(svcs []state.ServiceState) {
	for _, s := range svcs {
		pgid := s.PGID
		if pgid == 0 {
			if p, err := syscall.Getpgid(s.PID); err == nil {
				pgid = p
			}
		}
		if pgid != 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		} else {
			_ = syscall.Kill(s.PID, syscall.SIGINT)
		}
	}
	// Give them up to 3s to exit cleanly, then SIGKILL anything still alive.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !anyAlive(svcs) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, s := range svcs {
		if !state.IsProcessAlive(s.PID) {
			continue
		}
		pgid := s.PGID
		if pgid != 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = syscall.Kill(s.PID, syscall.SIGKILL)
		}
	}
}

func anyAlive(svcs []state.ServiceState) bool {
	for _, s := range svcs {
		if state.IsProcessAlive(s.PID) {
			return true
		}
	}
	return false
}
