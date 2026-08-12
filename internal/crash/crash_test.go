package crash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRecoversAndWritesReport(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// A panicking goroutine must not take the process with it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer Guard("test goroutine")
		panic("boom")
	}()
	<-done

	path := LastReport()
	if path == "" {
		t.Fatal("no crash report was recorded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}

	body := string(data)
	for _, want := range []string{"boom", "test goroutine", "pairin crash report"} {
		if !strings.Contains(body, want) {
			t.Errorf("report is missing %q:\n%s", want, body)
		}
	}
	// The stack must point at the panicking goroutine, not just at Guard.
	if !strings.Contains(body, "crash_test.go") {
		t.Errorf("report has no stack from the panic site:\n%s", body)
	}
}

func TestReportGoesUnderStateHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	path := Report("ctx", "oops", []byte("stack"))
	if path == "" {
		t.Fatal("Report returned no path")
	}
	want := filepath.Join(base, "pairin")
	if !strings.HasPrefix(path, want) {
		t.Errorf("report written to %s, want it under %s", path, want)
	}
}
