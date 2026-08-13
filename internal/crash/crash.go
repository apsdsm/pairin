// Package crash captures panics so that a bug in one corner of pairin produces
// a readable report on disk instead of a process that silently disappears —
// taking the user's terminal with it, or in the supervisor's case, every
// managed service.
//
// Two entry points:
//
//	defer crash.Guard("tailer: web")   // in a goroutine that must not kill the process
//	path := crash.Report(ctx, r, stack) // when you've recovered the panic yourself
//
// Reports are written to $XDG_STATE_HOME/pairin/crash-<timestamp>-<pid>.log.
// Writing a report is strictly best-effort: a failure to record a panic must
// never turn into a second panic.
package crash

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/apsdsm/pairin/internal/state"
)

// Version is stamped into every report. Set by cmd at init so this package
// doesn't have to import cmd (which would be a cycle).
var Version = "unknown"

var (
	mu   sync.Mutex
	last string
)

// Guard recovers from a panic in the calling goroutine and writes a report.
// Install it as the first statement of any long-lived goroutine:
//
//	go func() {
//	    defer crash.Guard("healthcheck: api")
//	    ...
//	}()
//
// The goroutine dies, but the process survives. Callers that need to know a
// panic happened should use recover directly and call Report.
func Guard(context string) {
	if r := recover(); r != nil {
		Report(context, r, debug.Stack())
	}
}

// Report writes a crash report describing r and returns the path it was
// written to. An empty return means the report could not be persisted; the
// details are still echoed to stderr in that case.
func Report(context string, r any, stack []byte) string {
	body := format(context, r, stack)

	path, err := reportPath()
	if err == nil {
		err = os.WriteFile(path, []byte(body), 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairin: could not write crash report: %v\n%s", err, body)
		return ""
	}

	mu.Lock()
	last = path
	mu.Unlock()
	return path
}

// LastReport returns the path of the most recent report written by this
// process, or "" if there hasn't been one.
func LastReport() string {
	mu.Lock()
	defer mu.Unlock()
	return last
}

func format(context string, r any, stack []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pairin crash report\n")
	fmt.Fprintf(&b, "time:    %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "version: %s\n", Version)
	fmt.Fprintf(&b, "pid:     %d\n", os.Getpid())
	fmt.Fprintf(&b, "context: %s\n", context)
	if wd, err := os.Getwd(); err == nil {
		fmt.Fprintf(&b, "cwd:     %s\n", wd)
	}
	fmt.Fprintf(&b, "\npanic: %v\n\n", r)
	b.Write(stack)
	return b.String()
}

func reportPath() (string, error) {
	dir, err := state.BaseDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("crash-%s-%d.log", time.Now().Format("20060102-150405"), os.Getpid())
	return filepath.Join(dir, name), nil
}
