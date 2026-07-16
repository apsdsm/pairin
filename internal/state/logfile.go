package state

import (
	"errors"
	"os"
	"path/filepath"
)

// MaxLogBytes is the rotation threshold for per-service log files. When a
// service starts and its log file already exceeds this size, it is rotated
// to <name>.log.1 before the new run begins. Mid-session rotation is not
// possible because the child owns the fd; this constant is the ceiling per
// pairin session rather than an absolute cap.
const MaxLogBytes = 10 * 1024 * 1024 // 10 MiB

// LogFilePath returns the absolute path to a service's log file.
func LogFilePath(configPath, serviceName string) string {
	return filepath.Join(LogsDir(configPath), serviceName+".log")
}

// ClearLogs removes every file in the project's logs directory, including
// rotated .log.1 files. It must only be called when no supervisor is running,
// since a live supervisor holds open fds to these files and would keep
// writing to the unlinked inodes.
func ClearLogs(configPath string) error {
	dir := LogsDir(configPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// RotateIfLarge renames <path> to <path>.1 when it exceeds MaxLogBytes. Any
// existing <path>.1 is overwritten. If the file does not exist or is smaller
// than the threshold, no action is taken.
func RotateIfLarge(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size() < MaxLogBytes {
		return nil
	}
	return os.Rename(path, path+".1")
}
