package tui

import (
	"errors"
	"io"
	"os"
	"strings"
)

// tailLines returns up to maxLines trailing lines of the file at path. At
// most readCap bytes are read from the end of the file so the call is fast
// regardless of total log size. Missing files return (nil, nil) — no error.
func tailLines(path string, maxLines, readCap int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := int64(0)
	if size > readCap {
		start = size - readCap
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}

	text := string(buf)
	lines := strings.Split(text, "\n")

	// If we didn't start from 0, the first "line" is a partial from the
	// middle of some line — discard it so output isn't misleading.
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	// A trailing newline leaves an empty last element; trim it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if int64(len(lines)) > maxLines {
		lines = lines[int64(len(lines))-maxLines:]
	}
	return lines, nil
}
