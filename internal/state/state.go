// Package state persists pairin's per-project runtime state to disk so that
// services can survive a pairin crash or SSH disconnect and be adopted (or
// cleanly restarted) by the next pairin invocation.
//
// Layout, rooted at the directory containing .pairinrc.toml:
//
//	.pairin/
//	  state.json      current services and PIDs
//	  pairin.pid      lockfile holding the active pairin process PID
//	  logs/           per-service log files (<service>.log, <service>.log.1)
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dirName       = ".pairin"
	stateFile     = "state.json"
	lockFile      = "supervisor.pid"
	logsSubdir    = "logs"
	socketFile    = "control.sock"
	supervisorLog = "supervisor.log"
)

// Dir returns the .pairin state directory for a given config file path.
func Dir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), dirName)
}

// LogsDir returns the per-project log directory.
func LogsDir(configPath string) string {
	return filepath.Join(Dir(configPath), logsSubdir)
}

// SocketPath returns the path to the supervisor's control socket.
func SocketPath(configPath string) string {
	return filepath.Join(Dir(configPath), socketFile)
}

// SupervisorLogPath returns the path where supervisor stdout/stderr is captured.
func SupervisorLogPath(configPath string) string {
	return filepath.Join(Dir(configPath), supervisorLog)
}

// LockPath returns the path to the supervisor lockfile.
func LockPath(configPath string) string {
	return filepath.Join(Dir(configPath), lockFile)
}

// EnsureDirs creates the state and logs directories if they don't exist.
func EnsureDirs(configPath string) error {
	if err := os.MkdirAll(LogsDir(configPath), 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	return nil
}

// ServiceState is the on-disk record for a single managed service.
type ServiceState struct {
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	PGID      int       `json:"pgid"`
	LogFile   string    `json:"log_file"`
	Cmd       string    `json:"cmd"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
}

// State is the on-disk snapshot of managed services.
type State struct {
	ConfigPath string         `json:"config_path"`
	Services   []ServiceState `json:"services"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// Load reads state.json. Returns (nil, nil) if the file does not exist.
func Load(configPath string) (*State, error) {
	path := filepath.Join(Dir(configPath), stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

// Save writes state.json atomically (write tmp + rename).
func Save(configPath string, s *State) error {
	if err := EnsureDirs(configPath); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	finalPath := filepath.Join(Dir(configPath), stateFile)
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

// Clear removes state.json. Log files are left untouched.
func Clear(configPath string) error {
	path := filepath.Join(Dir(configPath), stateFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AcquireLock writes the current pairin PID to pairin.pid. If the lock is
// already held by a live process, returns ErrLocked with the holder's PID.
// If the lock holder is dead, the stale lock is overwritten.
func AcquireLock(configPath string) error {
	if err := EnsureDirs(configPath); err != nil {
		return err
	}
	path := filepath.Join(Dir(configPath), lockFile)

	if data, err := os.ReadFile(path); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
			if IsProcessAlive(pid) {
				return &LockedError{PID: pid}
			}
		}
	}

	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// ReleaseLock removes pairin.pid. Missing file is not an error.
func ReleaseLock(configPath string) error {
	path := filepath.Join(Dir(configPath), lockFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LockHolder returns the PID recorded in pairin.pid, or 0 if no valid lock.
func LockHolder(configPath string) int {
	path := filepath.Join(Dir(configPath), lockFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// LockedError is returned by AcquireLock when another live pairin holds the lock.
type LockedError struct{ PID int }

func (e *LockedError) Error() string {
	return fmt.Sprintf("pairin is already running in this project (PID %d)", e.PID)
}

// IsProcessAlive reports whether a process with the given PID exists.
// Uses signal 0 which checks for existence without delivering a signal.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// LiveServices returns the subset of services in s whose PIDs are still alive.
func (s *State) LiveServices() []ServiceState {
	if s == nil {
		return nil
	}
	out := make([]ServiceState, 0, len(s.Services))
	for _, svc := range s.Services {
		if IsProcessAlive(svc.PID) {
			out = append(out, svc)
		}
	}
	return out
}
