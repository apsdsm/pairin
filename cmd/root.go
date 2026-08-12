package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/catalog"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/control"
	"github.com/apsdsm/pairin/internal/crash"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/apsdsm/pairin/internal/tui"
	"github.com/spf13/cobra"
)

// detachFlag is set by `-d` / `--detach` on the root command and `pairin up`.
// When true, runUp spawns (or confirms) the supervisor and exits without
// attaching a TUI, mirroring `docker compose up -d`.
var detachFlag bool

// configFlag is set by `-c` / `--config` on the user-facing commands that
// need a project config (root/up, attach, down). Empty means "search from
// cwd up to root for .pairinrc.toml".
var configFlag string

// clearLogsFlag is set by `--clear-logs` on the root command and `pairin up`.
// When true, runUp wipes .pairin/logs/ before spawning the supervisor so the
// TUI doesn't preload history from previous sessions.
var clearLogsFlag bool

// noRegisterFlag opts out of adding the project to the catalog on `up`.
var noRegisterFlag bool

var rootCmd = &cobra.Command{
	Use:   "pairin [project]",
	Short: "Local development process manager",
	Long: "Start or attach to the supervisor for a project.\n\n" +
		"With no argument, uses the .pairinrc.toml found from the current directory.\n" +
		"With a registered project name, works from anywhere — see `pairin projects`.",
	Args:          cobra.MaximumNArgs(1),
	RunE:          runUp,
	SilenceUsage:  true,
	SilenceErrors: false,
}

func init() {
	rootCmd.Flags().BoolVarP(&detachFlag, "detach", "d", false, "start the supervisor in the background and exit without attaching a TUI")
	rootCmd.Flags().StringVarP(&configFlag, "config", "c", "", "path to a .pairinrc.toml (defaults to searching cwd up to root)")
	rootCmd.Flags().BoolVar(&clearLogsFlag, "clear-logs", false, "delete existing service logs before starting, so the TUI doesn't preload old history")
	rootCmd.Flags().BoolVar(&noRegisterFlag, "no-register", false, "don't add this project to the catalog")
}

// resolveConfig loads the config a command should act on, in priority order:
// an explicit --config, a registered project name given as an argument, or a
// search upward from the current directory.
//
// Relative paths are resolved against the working directory; the supervisor is
// always handed an absolute path so its own cwd doesn't matter.
func resolveConfig(cmd *cobra.Command, args []string) (*config.Config, error) {
	// An empty argument is treated as no argument rather than as a project
	// named "", which nothing can ever match.
	if len(args) > 0 && strings.TrimSpace(args[0]) == "" {
		args = nil
	}

	if len(args) > 0 && configFlag != "" {
		return nil, fmt.Errorf("give either a project name or --config, not both")
	}

	if len(args) > 0 {
		cat, err := catalog.Load()
		if err != nil {
			return nil, fmt.Errorf("loading catalog: %w", err)
		}
		entry, err := cat.Find(args[0])
		if err != nil {
			return nil, withCommandHint(cmd, args[0], err)
		}
		cfg, err := config.LoadFrom(entry.Config)
		if err != nil {
			return nil, fmt.Errorf("loading config for %q: %w", entry.Name, err)
		}
		return cfg, nil
	}

	if configFlag == "" {
		return config.Load()
	}
	abs, err := filepath.Abs(configFlag)
	if err != nil {
		return nil, fmt.Errorf("resolving --config path: %w", err)
	}
	return config.LoadFrom(abs)
}

// loadConfig is resolveConfig with no positional argument.
func loadConfig() (*config.Config, error) { return resolveConfig(nil, nil) }

// withCommandHint softens the "no such project" error when the argument looks
// like a mistyped subcommand — `pairin statu` should point at `status` rather
// than send the user off to check their catalog.
func withCommandHint(cmd *cobra.Command, arg string, err error) error {
	if cmd == nil || arg == "" {
		return err
	}
	for _, sub := range cmd.Root().Commands() {
		if sub.Hidden {
			continue
		}
		if strings.HasPrefix(sub.Name(), arg) {
			return fmt.Errorf("%w\n(did you mean `pairin %s`?)", err, sub.Name())
		}
	}
	return err
}

// autoRegister adds the project to the catalog so it can be started by name
// later. Best-effort: a catalog problem must not stop services from starting.
func autoRegister(cfg *config.Config) {
	if noRegisterFlag {
		return
	}
	cat, err := catalog.Load()
	if err != nil {
		return
	}
	added, err := cat.Add(catalog.Project{Display: cfg.Project.Name, Config: cfg.Path})
	if err != nil || !added {
		return
	}
	_ = cat.Save()
}

func Execute() error {
	return rootCmd.Execute()
}

// runUp implements the default command. If a supervisor is already running
// in this project, attach a TUI to it (or print a status line and exit if
// --detach is set). Otherwise, prompt about any stale state, spawn a
// detached supervisor, and either attach a TUI or exit cleanly.
func runUp(cmd *cobra.Command, args []string) error {
	cfg, err := resolveConfig(cmd, args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	autoRegister(cfg)

	// Case 1: supervisor already running.
	if holder := state.LockHolder(cfg.Path); holder > 0 && state.IsProcessAlive(holder) {
		if clearLogsFlag {
			return fmt.Errorf("cannot clear logs while a supervisor is running (PID %d); run `pairin down` first", holder)
		}
		if detachFlag {
			fmt.Printf("Supervisor already running for this project (PID %d). Use `pairin attach` to view logs or `pairin down` to stop.\n", holder)
			return nil
		}
		return attachTUI(cfg)
	}

	// Case 2: no live supervisor, but stale state may have orphaned services.
	adoptRequested, err := resolveStaleState(cfg.Path)
	if err != nil {
		return err
	}

	// Case 3: spawn a fresh (or adopting) supervisor. Logs are cleared here,
	// before the supervisor exists, so no open fds point at the deleted files.
	// Adopted services simply recreate theirs on the next write (O_CREATE|O_APPEND).
	if clearLogsFlag {
		if err := state.ClearLogs(cfg.Path); err != nil {
			return fmt.Errorf("clearing logs: %w", err)
		}
	}
	if err := spawnSupervisor(cfg.Path, adoptRequested); err != nil {
		return err
	}
	if err := waitForSupervisor(cfg.Path, 5*time.Second); err != nil {
		return err
	}
	if detachFlag {
		holder := state.LockHolder(cfg.Path)
		fmt.Printf("Supervisor started (PID %d). Use `pairin attach` to view logs or `pairin down` to stop.\n", holder)
		return nil
	}
	return attachTUI(cfg)
}

// attachTUI connects a control Client to the supervisor and hands it to the TUI.
func attachTUI(cfg *config.Config) error {
	client, err := control.Dial(state.SocketPath(cfg.Path))
	if err != nil {
		return fmt.Errorf("attaching to supervisor: %w", err)
	}
	defer client.Close()

	model := tui.NewDashboardModel(cfg, client)
	// WithoutCatchPanics: Bubble Tea's own handler prints a stack to a screen
	// it is simultaneously tearing down and lets Run return a nil error, so a
	// TUI panic looks exactly like a clean exit. We'd rather have a report.
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithoutCatchPanics())
	client.SetProgram(p)

	if err := runProgram(p); err != nil {
		return err
	}
	return nil
}

// runProgram runs the Bubble Tea program, converting a panic into a crash
// report and an error rather than a silent exit. Services are owned by the
// supervisor, so a TUI crash leaves them running — the message says so, since
// the alternative reading ("everything just died") is the alarming one.
func runProgram(p *tea.Program) (err error) {
	defer func() {
		if r := recover(); r != nil {
			p.Kill() // restores the terminal
			path := crash.Report("tui", r, debug.Stack())
			if path != "" {
				err = fmt.Errorf("pairin's TUI crashed (your services are still running).\nCrash report: %s\nReattach with `pairin attach`", path)
			} else {
				err = fmt.Errorf("pairin's TUI crashed (your services are still running): %v", r)
			}
		}
	}()

	if _, runErr := p.Run(); runErr != nil {
		return fmt.Errorf("TUI error: %w", runErr)
	}
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
