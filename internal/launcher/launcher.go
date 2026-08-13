// Package launcher spawns detached supervisors. It lives apart from cmd so
// that both the CLI and the fleet dashboard can start a project the same way,
// rather than the dashboard growing a second, subtly different copy.
package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/state"
)

// DefaultTimeout is how long to wait for a spawned supervisor to start serving.
const DefaultTimeout = 5 * time.Second

// Spawn re-execs this binary with the hidden `supervisor` subcommand in a new
// session, so the supervisor survives the parent exiting or the tty closing.
func Spawn(configPath string, adopt bool) error {
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
	// Setsid makes the child a session leader so it doesn't receive SIGHUP when
	// the parent TUI exits (detach) or the tty closes (SSH drop).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logF.Close()
		return fmt.Errorf("starting supervisor: %w", err)
	}
	// The parent releases its copy of the log fd; the supervisor has its own.
	logF.Close()
	// We don't Wait — the supervisor runs independently, and init reaps it.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

// WaitReady polls until the supervisor's control socket accepts a connection.
func WaitReady(configPath string, timeout time.Duration) error {
	sock := state.SocketPath(configPath)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			// The socket file exists; connect to be sure it's actually serving.
			if client, dErr := control.Dial(sock); dErr == nil {
				_ = client.Close()
				return nil
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	return errors.New("supervisor did not become ready in time (check .pairin/supervisor.log)")
}

// Start spawns a supervisor and waits for it to serve.
//
// Orphaned services from a previous supervisor are adopted rather than killed.
// The CLI can afford to ask, but a caller without a terminal cannot, and
// adoption is the choice that doesn't destroy work in progress.
func Start(configPath string, timeout time.Duration) error {
	adopt := false
	if snap, err := state.Load(configPath); err == nil && snap != nil {
		adopt = len(snap.LiveServices()) > 0
	}
	if err := Spawn(configPath, adopt); err != nil {
		return err
	}
	return WaitReady(configPath, timeout)
}
