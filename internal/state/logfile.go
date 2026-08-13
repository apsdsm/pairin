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

// TruncateLog empties a service's log in place and discards its rotated
// predecessor.
//
// Unlike ClearLogs this is safe while the supervisor is running. Services are
// started with O_APPEND, so every write goes to the current end of the file:
// after truncating to zero the child simply resumes writing from the start,
// with no sparse gap. Unlinking the file would instead leave the child writing
// to an inode nobody can read any more. The log tailer already detects a file
// shrinking below its read offset and starts over.
func TruncateLog(path string) error {
	if err := os.Truncate(path, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path + ".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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
