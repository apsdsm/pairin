package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/apsdsm/pairin/internal/tui"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "pairin",
	Short: "Local development process manager",
	RunE:  runUp,
}

func Execute() error {
	return rootCmd.Execute()
}

// runUp implements the default command: if a supervisor is already running
// in this project, attach a TUI to it. Otherwise, prompt about any stale
// state, spawn a detached supervisor, and attach a TUI once the socket is up.
func runUp(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Case 1: supervisor already running. Just attach.
	if holder := state.LockHolder(cfg.Path); holder > 0 && state.IsProcessAlive(holder) {
		return attachTUI(cfg)
	}

	// Case 2: no live supervisor, but stale state may have orphaned services.
	adoptRequested, err := resolveStaleState(cfg.Path)
	if err != nil {
		return err
	}

	// Case 3: spawn a fresh (or adopting) supervisor and attach.
	if err := spawnSupervisor(cfg.Path, adoptRequested); err != nil {
		return err
	}
	if err := waitForSupervisor(cfg.Path, 5*time.Second); err != nil {
		return err
	}
	return attachTUI(cfg)
}

// attachTUI connects a control Client to the supervisor and hands it to the TUI.
func attachTUI(cfg *config.Config) error {
	client, err := control.Dial(state.SocketPath(cfg.Path))
	if err != nil {
		return fmt.Errorf("attaching to supervisor: %w", err)
	}

	model := tui.NewDashboardModel(cfg, client)
	p := tea.NewProgram(model, tea.WithAltScreen())
	client.SetProgram(p)

	if _, err := p.Run(); err != nil {
		_ = client.Close()
		return fmt.Errorf("TUI error: %w", err)
	}
	_ = client.Close()
	return nil
}

// spawnSupervisor re-execs this binary with the hidden `supervisor` subcommand
// in a new session so the supervisor survives the parent TUI's exit.
func spawnSupervisor(configPath string, adopt bool) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating pairin binary: %w", err)
	}

	args := []string{"supervisor", "--config", configPath}
	if adopt {
		args = append(args, "--adopt")
	}

	if err := state.EnsureDirs(configPath); err != nil {
		return err
	}
	logF, err := os.OpenFile(state.SupervisorLogPath(configPath), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening supervisor log: %w", err)
	}
	fmt.Fprintf(logF, "\n--- supervisor spawning %s ---\n", time.Now().Format(time.RFC3339))

	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	// Setsid: true makes the child a session leader so it doesn't receive
	// SIGHUP when the parent TUI exits (detach path) or the tty closes (SSH drop).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logF.Close()
		return fmt.Errorf("starting supervisor: %w", err)
	}
	// Parent releases its copy of the log fd; supervisor has its own.
	logF.Close()
	// We don't Wait — the supervisor runs independently. The OS will reap it
	// via init when the time comes.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

// waitForSupervisor polls for the control socket to become connectable.
func waitForSupervisor(configPath string, timeout time.Duration) error {
	sock := state.SocketPath(configPath)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			// Socket file exists; try a connect to be sure.
			client, dErr := control.Dial(sock)
			if dErr == nil {
				_ = client.Close()
				return nil
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	return errors.New("supervisor did not become ready in time (check .pairin/supervisor.log)")
}

// resolveStaleState handles the case where a previous supervisor is gone but
// its services may still be running. Returns true if the user wants the new
// supervisor to adopt them. Quit exits the process.
func resolveStaleState(configPath string) (bool, error) {
	snap, err := state.Load(configPath)
	if err != nil {
		return false, fmt.Errorf("reading prior state: %w", err)
	}
	if snap == nil {
		return false, nil
	}

	live := snap.LiveServices()
	if len(live) == 0 {
		_ = state.Clear(configPath)
		_ = state.ReleaseLock(configPath)
		return false, nil
	}

	fmt.Println()
	fmt.Println("A previous pairin supervisor exited without cleaning up.")
	fmt.Printf("%d service(s) still running:\n", len(live))
	for _, s := range live {
		fmt.Printf("  %-20s PID %d\n", s.Name, s.PID)
	}
	fmt.Println()
	fmt.Println("  [A]dopt       take over the running processes with a new supervisor")
	fmt.Println("  [R]estart     kill the orphans and start fresh")
	fmt.Println("  [Q]uit        exit without touching them")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("choice [a/r/q]: ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("reading choice: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "a", "adopt":
			return true, nil
		case "r", "restart":
			killOrphans(live)
			_ = state.Clear(configPath)
			_ = state.ReleaseLock(configPath)
			return false, nil
		case "q", "quit", "":
			os.Exit(0)
		}
	}
}

// killOrphans sends SIGINT then SIGKILL to each orphan's process group.
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
