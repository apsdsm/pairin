package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Instance is a registry entry describing a running pairin supervisor on this
// host. Registry files live in a well-known directory so `pairin ls` can find
// supervisors across any project on the machine.
type Instance struct {
	SupervisorPID int       `json:"supervisor_pid"`
	ConfigPath    string    `json:"config_path"`
	ProjectName   string    `json:"project_name"`
	SocketPath    string    `json:"socket_path"`
	StartedAt     time.Time `json:"started_at"`
}

// BaseDir returns pairin's host-wide state directory — the root that holds the
// instance registry and crash reports. Respects XDG_STATE_HOME, falling back
// to ~/.local/state.
func BaseDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "pairin"), nil
}

// InstanceDir returns the directory where supervisor registry files are
// written.
func InstanceDir() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "instances"), nil
}

// instanceFile returns the absolute path of the registry file for configPath.
// Keyed by a stable hash of the absolute config path so renaming a project
// on disk cleanly replaces its entry rather than leaking a stale one.
func instanceFile(configPath string) (string, error) {
	dir, err := InstanceDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configHash(configPath)+".json"), nil
}

func configHash(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16]
}

// Register writes or overwrites the registry file for a running supervisor.
// Idempotent: a supervisor restart with the same config path replaces its
// entry rather than leaving a stale one behind.
func Register(inst Instance) error {
	dir, err := InstanceDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating instance dir: %w", err)
	}
	path, err := instanceFile(inst.ConfigPath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Unregister removes the registry file for the given config path. Missing
// file is not an error — clean shutdown calls this unconditionally.
func Unregister(configPath string) error {
	path, err := instanceFile(configPath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListInstances returns all registered supervisors whose PIDs are still alive.
// Stale entries (dead PIDs) are pruned from the registry as a side effect, so
// the list stays self-cleaning over time.
func ListInstances() ([]Instance, error) {
	dir, err := InstanceDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var inst Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			// Malformed registry file — nothing useful, clean it up.
			_ = os.Remove(full)
			continue
		}
		if !IsProcessAlive(inst.SupervisorPID) {
			_ = os.Remove(full)
			continue
		}
		out = append(out, inst)
	}
	return out, nil
}
