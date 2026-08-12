package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

// supervisorCmd is the background worker that owns services and serves the
// control socket. It's not meant to be invoked by users directly; `pairin up`
// re-exec's self with this subcommand after double-forking. Hidden from help.
var supervisorCmd = &cobra.Command{
	Use:    "supervisor",
	Short:  "Internal: run the service supervisor",
	Hidden: true,
	RunE:   runSupervisor,
}

var (
	supervisorConfigPath string
	supervisorAdopt      bool
)

func init() {
	supervisorCmd.Flags().StringVar(&supervisorConfigPath, "config", "", "path to .pairinrc.toml (required)")
	supervisorCmd.Flags().BoolVar(&supervisorAdopt, "adopt", false, "adopt services listed in state.json at startup")
	rootCmd.AddCommand(supervisorCmd)
}

func runSupervisor(cmd *cobra.Command, args []string) (err error) {
	// A panic here would take every managed service down with it. Record what
	// happened and stop the services deliberately rather than by falling over.
	defer func() {
		if r := recover(); r != nil {
			path := crash.Report("supervisor", r, debug.Stack())
			err = fmt.Errorf("supervisor panicked; crash report: %s", path)
		}
	}()

	if supervisorConfigPath == "" {
		return fmt.Errorf("--config is required")
	}

	cfg, err := config.LoadFrom(supervisorConfigPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := state.EnsureDirs(cfg.Path); err != nil {
		return err
	}

	// Acquire the supervisor lock. We overwrite any stale holder — the user
	// already agreed (via the up prompt) to take over.
	if err := os.WriteFile(supervisorPidPath(cfg.Path), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("writing supervisor pid: %w", err)
	}
	defer os.Remove(supervisorPidPath(cfg.Path))

	// Announce ourselves to the global registry so `pairin ls` can find us.
	// Best-effort: a failed registration shouldn't block service startup.
	inst := state.Instance{
		SupervisorPID: os.Getpid(),
		ConfigPath:    cfg.Path,
		ProjectName:   cfg.Project.Name,
		SocketPath:    state.SocketPath(cfg.Path),
		StartedAt:     time.Now(),
	}
	if err := state.Register(inst); err != nil {
		fmt.Fprintf(os.Stderr, "pairin: registry registration failed: %v\n", err)
	}
	defer state.Unregister(cfg.Path)

	mgr := process.NewManager(cfg)

	// If we were asked to adopt, pull the survivors out of state.json before
	// installing the server (so the first snapshot reflects reality).
	if supervisorAdopt {
		if snap, err := state.Load(cfg.Path); err == nil && snap != nil {
			for _, s := range snap.LiveServices() {
				for i, svc := range mgr.Services {
					if svc.Config.Name == s.Name {
						mgr.AdoptService(i, s.PID, s.PGID, s.LogFile)
						break
					}
				}
			}
		}
	}

	server := control.NewServer(mgr, cfg)
	if err := server.Start(state.SocketPath(cfg.Path)); err != nil {
		return fmt.Errorf("starting control server: %w", err)
	}
	defer os.Remove(state.SocketPath(cfg.Path))

	// Non-adopted services still need to be launched.
	mgr.StartAll()()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-server.Done():
	case <-sigCh:
		mgr.StopAll()
		server.Shutdown()
	}

	// Flush state and give sockets a moment to close gracefully.
	time.Sleep(50 * time.Millisecond)
	return nil
}

func supervisorPidPath(configPath string) string {
	return state.LockPath(configPath)
}
