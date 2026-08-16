package process

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/state"
)

// ---------------------------------------------------------------------------
// RingBuffer
// ---------------------------------------------------------------------------

func TestRingBuffer_Empty(t *testing.T) {
	rb := NewRingBuffer(5)
	if lines := rb.Lines(); lines != nil {
		t.Fatalf("expected nil, got %v", lines)
	}
}

func TestRingBuffer_AddAndRetrieve(t *testing.T) {
	rb := NewRingBuffer(5)
	rb.Add("a")
	rb.Add("b")
	rb.Add("c")

	lines := rb.Lines()
	want := []string{"a", "b", "c"}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d", len(want), len(lines))
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d: expected %q, got %q", i, w, lines[i])
		}
	}
}

func TestRingBuffer_Wraps(t *testing.T) {
	rb := NewRingBuffer(3)
	rb.Add("a")
	rb.Add("b")
	rb.Add("c")
	rb.Add("d") // evicts "a"
	rb.Add("e") // evicts "b"

	lines := rb.Lines()
	want := []string{"c", "d", "e"}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d", len(want), len(lines))
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d: expected %q, got %q", i, w, lines[i])
		}
	}
}

func TestRingBuffer_ExactCapacity(t *testing.T) {
	rb := NewRingBuffer(3)
	rb.Add("a")
	rb.Add("b")
	rb.Add("c")

	lines := rb.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "a" || lines[1] != "b" || lines[2] != "c" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// Status.String()
// ---------------------------------------------------------------------------

func TestStatus_String(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusStopped, "stopped"},
		{StatusWaiting, "waiting"},
		{StatusStarting, "starting"},
		{StatusRunning, "running"},
		{StatusCrashed, "crashed"},
		{StatusRestarting, "restarting"},
		{Status(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("Status(%d).String() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Healthcheck functions
// ---------------------------------------------------------------------------

func TestCheckTCP_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	if !checkTCP(addr) {
		t.Errorf("checkTCP(%q) = false, want true", addr)
	}
}

func TestCheckTCP_Failure(t *testing.T) {
	// Use a port that's almost certainly not listening
	if checkTCP("127.0.0.1:1") {
		t.Error("checkTCP on closed port returned true, want false")
	}
}

func TestCheckHTTP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if !checkHTTP(srv.URL) {
		t.Errorf("checkHTTP(%q) = false, want true", srv.URL)
	}
}

func TestCheckHTTP_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	if checkHTTP(srv.URL) {
		t.Errorf("checkHTTP(%q) = true for 500, want false", srv.URL)
	}
}

func TestCheckHTTP_ConnectionRefused(t *testing.T) {
	if checkHTTP("http://127.0.0.1:1") {
		t.Error("checkHTTP on unreachable host returned true, want false")
	}
}

func TestRunHealthcheck_TCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	hc := fmt.Sprintf("tcp://%s", ln.Addr().String())
	if !runHealthcheck(hc) {
		t.Errorf("runHealthcheck(%q) = false, want true", hc)
	}
}

func TestRunHealthcheck_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if !runHealthcheck(srv.URL) {
		t.Errorf("runHealthcheck(%q) = false, want true", srv.URL)
	}
}

func TestRunHealthcheck_UnknownScheme(t *testing.T) {
	if runHealthcheck("ftp://localhost:21") {
		t.Error("runHealthcheck with unknown scheme returned true, want false")
	}
}

func TestRunHealthcheck_EmptyString(t *testing.T) {
	if runHealthcheck("") {
		t.Error("runHealthcheck with empty string returned true, want false")
	}
}

// ---------------------------------------------------------------------------
// allDepsHealthy
// ---------------------------------------------------------------------------

func newTestManager(services []config.Service) *Manager {
	cfg := &config.Config{Services: services}
	return NewManager(cfg)
}

func TestAllDepsHealthy_NoDeps(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi"},
	})
	if !m.allDepsHealthy(0) {
		t.Error("allDepsHealthy with no deps should return true")
	}
}

func TestAllDepsHealthy_AllHealthy(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
		{Name: "cache", Cmd: "echo hi", Healthcheck: "tcp://localhost:6379"},
		{Name: "web", Cmd: "echo hi", DependsOn: []string{"db", "cache"}},
	})

	m.Services[0].Healthy = true
	m.Services[1].Healthy = true

	if !m.allDepsHealthy(2) {
		t.Error("allDepsHealthy should return true when all deps are healthy")
	}
}

func TestAllDepsHealthy_SomeUnhealthy(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
		{Name: "cache", Cmd: "echo hi", Healthcheck: "tcp://localhost:6379"},
		{Name: "web", Cmd: "echo hi", DependsOn: []string{"db", "cache"}},
	})

	m.Services[0].Healthy = true
	m.Services[1].Healthy = false

	if m.allDepsHealthy(2) {
		t.Error("allDepsHealthy should return false when some deps are unhealthy")
	}
}

func TestAllDepsHealthy_NoneHealthy(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
		{Name: "web", Cmd: "echo hi", DependsOn: []string{"db"}},
	})

	if m.allDepsHealthy(1) {
		t.Error("allDepsHealthy should return false when deps are not healthy")
	}
}

// ---------------------------------------------------------------------------
// tryStartWaiting
// ---------------------------------------------------------------------------

func TestTryStartWaiting_StartsReadyServices(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
		{Name: "web", Dir: tmpDir, Cmd: "echo hi", DependsOn: []string{"db"}},
	})

	// Simulate: db is running and healthy, web is waiting
	m.Services[0].Status = StatusRunning
	m.Services[0].Healthy = true
	m.Services[1].Status = StatusWaiting

	m.tryStartWaiting()

	// Give startService a moment to update status
	time.Sleep(100 * time.Millisecond)

	m.Services[1].mu.Lock()
	status := m.Services[1].Status
	m.Services[1].mu.Unlock()

	// Service should have progressed past StatusWaiting
	if status == StatusWaiting {
		t.Error("expected web to no longer be waiting after deps became healthy")
	}
}

func TestTryStartWaiting_DoesNotStartIfDepsUnhealthy(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
		{Name: "web", Cmd: "echo hi", DependsOn: []string{"db"}},
	})

	m.Services[0].Status = StatusRunning
	m.Services[0].Healthy = false
	m.Services[1].Status = StatusWaiting

	m.tryStartWaiting()

	m.Services[1].mu.Lock()
	status := m.Services[1].Status
	m.Services[1].mu.Unlock()

	if status != StatusWaiting {
		t.Errorf("expected web to remain waiting, got %v", status)
	}
}

// ---------------------------------------------------------------------------
// StartAll with dependencies
// ---------------------------------------------------------------------------

func TestStartAll_NoDeps_AllStart(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "a", Dir: tmpDir, Cmd: "sleep 60"},
		{Name: "b", Dir: tmpDir, Cmd: "sleep 60"},
	})

	cmd := m.StartAll()
	cmd() // execute synchronously

	// Both should be running
	for i, svc := range m.Services {
		svc.mu.Lock()
		status := svc.Status
		svc.mu.Unlock()
		if status != StatusRunning {
			t.Errorf("service %d: expected running, got %v", i, status)
		}
	}

	m.StopAll()
}

func TestStartAll_WithDeps_WaitsForUnhealthy(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: "tcp://127.0.0.1:1"},
		{Name: "web", Dir: tmpDir, Cmd: "echo hi", DependsOn: []string{"db"}},
	})

	cmd := m.StartAll()
	cmd()

	// db should be running (started immediately), web should be waiting
	m.Services[0].mu.Lock()
	dbStatus := m.Services[0].Status
	m.Services[0].mu.Unlock()

	m.Services[1].mu.Lock()
	webStatus := m.Services[1].Status
	m.Services[1].mu.Unlock()

	if dbStatus != StatusRunning {
		t.Errorf("db: expected running, got %v", dbStatus)
	}
	if webStatus != StatusWaiting {
		t.Errorf("web: expected waiting, got %v", webStatus)
	}

	m.StopAll()
}

// ---------------------------------------------------------------------------
// NewManager / nameToIdx
// ---------------------------------------------------------------------------

func TestNewManager_BuildsNameIndex(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "gamma"},
	})

	tests := map[string]int{
		"alpha": 0,
		"beta":  1,
		"gamma": 2,
	}

	for name, wantIdx := range tests {
		gotIdx, ok := m.nameToIdx[name]
		if !ok {
			t.Errorf("nameToIdx missing key %q", name)
			continue
		}
		if gotIdx != wantIdx {
			t.Errorf("nameToIdx[%q] = %d, want %d", name, gotIdx, wantIdx)
		}
	}
}

// ---------------------------------------------------------------------------
// Healthcheck poller integration
// ---------------------------------------------------------------------------

func TestHealthcheckPoller_DetectsHealthy(t *testing.T) {
	// Start a TCP listener that the healthcheck will find
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept connections in background so checkTCP succeeds
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tmpDir := t.TempDir()
	hc := fmt.Sprintf("tcp://%s", ln.Addr().String())

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: hc},
	})

	// Manually set service state as if startService ran
	svc := m.Services[0]
	svc.mu.Lock()
	svc.Status = StatusRunning
	svc.generation = 1
	svc.mu.Unlock()

	// Start the poller (normally called from startService with lock held)
	svc.mu.Lock()
	m.startHealthcheckPoller(0)
	svc.mu.Unlock()

	// Wait for the poller to detect healthy (polls every 2s)
	deadline := time.After(5 * time.Second)
	for {
		svc.mu.Lock()
		healthy := svc.Healthy
		svc.mu.Unlock()

		if healthy {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for healthcheck to become healthy")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Clean up poller
	svc.mu.Lock()
	if svc.healthCancel != nil {
		svc.healthCancel()
	}
	svc.mu.Unlock()
}

func TestHealthcheckPoller_TriggersWaitingDeps(t *testing.T) {
	// Start a TCP listener for the healthcheck
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tmpDir := t.TempDir()
	hc := fmt.Sprintf("tcp://%s", ln.Addr().String())

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: hc},
		{Name: "web", Dir: tmpDir, Cmd: "echo hi", DependsOn: []string{"db"}},
	})

	// Start db as running, web as waiting
	db := m.Services[0]
	db.mu.Lock()
	db.Status = StatusRunning
	db.generation = 1
	db.mu.Unlock()

	web := m.Services[1]
	web.mu.Lock()
	web.Status = StatusWaiting
	web.mu.Unlock()

	// Start the poller for db - it should detect healthy and trigger web
	db.mu.Lock()
	m.startHealthcheckPoller(0)
	db.mu.Unlock()

	// Wait for web to leave waiting status
	deadline := time.After(5 * time.Second)
	for {
		web.mu.Lock()
		status := web.Status
		web.mu.Unlock()

		if status != StatusWaiting {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for dependent service to start")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Clean up
	db.mu.Lock()
	if db.healthCancel != nil {
		db.healthCancel()
	}
	db.mu.Unlock()
	m.StopAll()
}

// ---------------------------------------------------------------------------
// Service.GetLines (thread safety smoke test)
// ---------------------------------------------------------------------------

func TestGetLines_ThreadSafe(t *testing.T) {
	svc := &Service{
		Logs: NewRingBuffer(100),
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			svc.mu.Lock()
			svc.Logs.Add(fmt.Sprintf("line %d", i))
			svc.mu.Unlock()
		}
	}()

	// Reader
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = svc.GetLines()
		}
	}()

	wg.Wait()
}

// ---------------------------------------------------------------------------
// stopService resets health
// ---------------------------------------------------------------------------

func TestStopService_ResetsHealth(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: "tcp://localhost:5432"},
	})

	// Start the service so there's something to stop
	cmd := m.StartAll()
	cmd()

	svc := m.Services[0]
	svc.mu.Lock()
	svc.Healthy = true
	svc.mu.Unlock()

	m.stopService(0)

	svc.mu.Lock()
	healthy := svc.Healthy
	cancel := svc.healthCancel
	svc.mu.Unlock()

	if healthy {
		t.Error("expected Healthy to be false after stop")
	}
	if cancel != nil {
		t.Error("expected healthCancel to be nil after stop")
	}
}

// ---------------------------------------------------------------------------
// send() with nil program (no-op, shouldn't panic)
// ---------------------------------------------------------------------------

func TestSend_NilProgram(t *testing.T) {
	m := newTestManager([]config.Service{})
	// Should not panic
	m.send(LogMsg{Index: 0, Line: "test"})
}

// ---------------------------------------------------------------------------
// Healthcheck poller respects cancellation
// ---------------------------------------------------------------------------

func TestHealthcheckPoller_Cancellation(t *testing.T) {
	tmpDir := t.TempDir()
	// Point at a port that will fail - we just want to test cancellation
	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: "tcp://127.0.0.1:1"},
	})

	svc := m.Services[0]
	svc.mu.Lock()
	svc.Status = StatusRunning
	svc.generation = 1
	m.startHealthcheckPoller(0)
	cancel := svc.healthCancel
	svc.mu.Unlock()

	// Cancel immediately
	cancel()

	// Verify poller goroutine stops (no way to directly observe, but ensure no panic)
	time.Sleep(100 * time.Millisecond)

	svc.mu.Lock()
	healthy := svc.Healthy
	svc.mu.Unlock()

	if healthy {
		t.Error("expected Healthy to remain false after cancellation")
	}
}

// ---------------------------------------------------------------------------
// Healthcheck poller respects generation guard
// ---------------------------------------------------------------------------

func TestHealthcheckPoller_StaleGeneration(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tmpDir := t.TempDir()
	hc := fmt.Sprintf("tcp://%s", ln.Addr().String())

	m := newTestManager([]config.Service{
		{Name: "db", Dir: tmpDir, Cmd: "sleep 60", Healthcheck: hc},
	})

	svc := m.Services[0]
	svc.mu.Lock()
	svc.Status = StatusRunning
	svc.generation = 1
	m.startHealthcheckPoller(0)
	svc.mu.Unlock()

	// Bump the generation to simulate a restart
	svc.mu.Lock()
	svc.generation = 2
	svc.mu.Unlock()

	// Wait past a poll cycle
	time.Sleep(3 * time.Second)

	svc.mu.Lock()
	healthy := svc.Healthy
	cancel := svc.healthCancel
	svc.mu.Unlock()

	// The stale poller should have noticed the generation mismatch and exited
	// without setting Healthy
	if healthy {
		t.Error("stale poller should not have updated Healthy")
	}

	// Clean up
	if cancel != nil {
		cancel()
	}
}

// ---------------------------------------------------------------------------
// detectBranch (basic smoke test)
// ---------------------------------------------------------------------------

func TestDetectBranch_InvalidDir(t *testing.T) {
	branch := detectBranch("/nonexistent/path")
	if branch != "?" {
		t.Errorf("expected '?' for invalid dir, got %q", branch)
	}
}

func TestDetectBranch_ValidGitRepo(t *testing.T) {
	// The test is running inside this git repo
	wd, err := os.Getwd()
	if err != nil {
		t.Skip("cannot get working directory")
	}
	branch := detectBranch(wd)
	// Should return something (the current branch), not "?"
	if branch == "?" {
		t.Skip("not in a git repo")
	}
	if branch == "" {
		t.Error("expected non-empty branch name")
	}
}

// ---------------------------------------------------------------------------
// shouldAutoRestart
// ---------------------------------------------------------------------------

func TestShouldAutoRestart_PolicyNo(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "no"},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	if m.shouldAutoRestart(svc, true) {
		t.Error("expected no auto-restart with policy 'no'")
	}
}

func TestShouldAutoRestart_PolicyEmpty(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi"},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	if m.shouldAutoRestart(svc, true) {
		t.Error("expected no auto-restart with empty (default) policy")
	}
}

func TestShouldAutoRestart_Always_OnFailure(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always"},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	if !m.shouldAutoRestart(svc, true) {
		t.Error("expected auto-restart with policy 'always' on failure")
	}
}

func TestShouldAutoRestart_Always_OnSuccess(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always"},
	})
	svc := m.Services[0]
	svc.Status = StatusStopped
	if !m.shouldAutoRestart(svc, false) {
		t.Error("expected auto-restart with policy 'always' on success")
	}
}

func TestShouldAutoRestart_OnFailure_Crash(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "on-failure"},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	if !m.shouldAutoRestart(svc, true) {
		t.Error("expected auto-restart with policy 'on-failure' on crash")
	}
}

func TestShouldAutoRestart_OnFailure_NormalExit(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "on-failure"},
	})
	svc := m.Services[0]
	svc.Status = StatusStopped
	if m.shouldAutoRestart(svc, false) {
		t.Error("expected no auto-restart with policy 'on-failure' on normal exit")
	}
}

func TestShouldAutoRestart_OnSuccess_NormalExit(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "on-success"},
	})
	svc := m.Services[0]
	svc.Status = StatusStopped
	if !m.shouldAutoRestart(svc, false) {
		t.Error("expected auto-restart with policy 'on-success' on normal exit")
	}
}

func TestShouldAutoRestart_OnSuccess_Crash(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "on-success"},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	if m.shouldAutoRestart(svc, true) {
		t.Error("expected no auto-restart with policy 'on-success' on crash")
	}
}

func TestShouldAutoRestart_MaxRestartsReached(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always", MaxRestarts: 3},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	svc.RestartCount = 3
	if m.shouldAutoRestart(svc, true) {
		t.Error("expected no auto-restart when max_restarts reached")
	}
}

func TestShouldAutoRestart_MaxRestartsNotReached(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always", MaxRestarts: 3},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	svc.RestartCount = 2
	if !m.shouldAutoRestart(svc, true) {
		t.Error("expected auto-restart when under max_restarts")
	}
}

func TestShouldAutoRestart_UnlimitedRestarts(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always", MaxRestarts: 0},
	})
	svc := m.Services[0]
	svc.Status = StatusCrashed
	svc.RestartCount = 100
	if !m.shouldAutoRestart(svc, true) {
		t.Error("expected auto-restart with unlimited restarts (max_restarts=0)")
	}
}

func TestShouldAutoRestart_IntentionalStop(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always"},
	})
	svc := m.Services[0]
	// StatusStarting simulates a state that's not crashed/stopped
	svc.Status = StatusStarting
	if m.shouldAutoRestart(svc, true) {
		t.Error("expected no auto-restart when status is not crashed/stopped")
	}
}

// ---------------------------------------------------------------------------
// Auto-restart integration
// ---------------------------------------------------------------------------

func TestAutoRestart_OnFailure(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "crasher", Dir: tmpDir, Cmd: "exit 1", Restart: "on-failure", RestartDelay: "500ms", MaxRestarts: 2},
	})

	cmd := m.StartAll()
	cmd()

	// Wait for it to crash and auto-restart a couple of times
	deadline := time.After(10 * time.Second)
	for {
		svc := m.Services[0]
		svc.mu.Lock()
		count := svc.RestartCount
		svc.mu.Unlock()

		if count >= 2 {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for auto-restarts, count=%d", count)
		case <-time.After(100 * time.Millisecond):
		}
	}

	// After hitting max_restarts, should stay crashed
	time.Sleep(1 * time.Second)
	svc := m.Services[0]
	svc.mu.Lock()
	count := svc.RestartCount
	status := svc.Status
	svc.mu.Unlock()

	if count != 2 {
		t.Errorf("expected restart count of 2, got %d", count)
	}
	if status != StatusCrashed {
		t.Errorf("expected crashed status after max_restarts, got %v", status)
	}

	m.StopAll()
}

func TestManualRestart_ResetsRestartCount(t *testing.T) {
	m := newTestManager([]config.Service{
		{Name: "web", Cmd: "echo hi", Restart: "always", MaxRestarts: 5},
	})
	svc := m.Services[0]
	svc.RestartCount = 4

	// Simulate manual restart resetting the counter
	svc.mu.Lock()
	svc.RestartCount = 0
	svc.mu.Unlock()

	if svc.RestartCount != 0 {
		t.Errorf("expected restart count to be reset to 0, got %d", svc.RestartCount)
	}
}

// ---------------------------------------------------------------------------
// Shutdown path
// ---------------------------------------------------------------------------

func TestStopAll_SetsQuittingFlag(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "a", Dir: tmpDir, Cmd: "sleep 60"},
	})

	cmd := m.StartAll()
	cmd()

	m.StopAll()

	if !m.quitting.Load() {
		t.Error("expected quitting flag to be true after StopAll")
	}
}

func TestStopAll_NilsProgramAfterShutdown(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "a", Dir: tmpDir, Cmd: "sleep 60"},
	})
	// Simulate a sink being set (field was renamed from program to sink when
	// the broadcaster abstraction landed). StopAll must clear it.
	m.mu.Lock()
	m.sink = nil
	m.mu.Unlock()

	cmd := m.StartAll()
	cmd()

	m.StopAll()

	m.mu.Lock()
	p := m.sink
	m.mu.Unlock()

	if p != nil {
		t.Error("expected sink to be nil after StopAll")
	}
}

func TestStopAll_CompletesWithinTimeout(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "a", Dir: tmpDir, Cmd: "sleep 60"},
		{Name: "b", Dir: tmpDir, Cmd: "sleep 60"},
		{Name: "c", Dir: tmpDir, Cmd: "sleep 60"},
	})

	cmd := m.StartAll()
	cmd()

	done := make(chan struct{})
	go func() {
		m.StopAll()
		close(done)
	}()

	select {
	case <-done:
		// Good — StopAll returned
	case <-time.After(10 * time.Second):
		t.Fatal("StopAll blocked for more than 10 seconds")
	}
}

func TestStopService_KillsSIGINTIgnoringProcess(t *testing.T) {
	tmpDir := t.TempDir()

	// Process that traps SIGINT and refuses to die — must be killed with SIGKILL
	m := newTestManager([]config.Service{
		{Name: "stubborn", Dir: tmpDir, Cmd: "trap '' INT; sleep 60"},
	})

	cmd := m.StartAll()
	cmd()

	svc := m.Services[0]
	svc.mu.Lock()
	pid := svc.PID
	svc.mu.Unlock()

	if pid == 0 {
		t.Fatal("expected service to have a PID")
	}

	done := make(chan struct{})
	go func() {
		m.stopService(0)
		close(done)
	}()

	// stopService should complete: 5s SIGINT grace + SIGKILL fallback + 3s max
	select {
	case <-done:
		// Good
	case <-time.After(12 * time.Second):
		t.Fatal("stopService blocked on a SIGINT-ignoring process")
	}
}

func TestStopAll_KillsSIGINTIgnoringProcesses(t *testing.T) {
	tmpDir := t.TempDir()

	// Multiple SIGINT-ignoring processes — the scenario that caused the original freeze
	m := newTestManager([]config.Service{
		{Name: "a", Dir: tmpDir, Cmd: "trap '' INT; sleep 60"},
		{Name: "b", Dir: tmpDir, Cmd: "trap '' INT; sleep 60"},
	})

	cmd := m.StartAll()
	cmd()

	done := make(chan struct{})
	go func() {
		m.StopAll()
		close(done)
	}()

	select {
	case <-done:
		// Good — all stopped despite ignoring SIGINT
	case <-time.After(12 * time.Second):
		t.Fatal("StopAll blocked on SIGINT-ignoring processes")
	}
}

func TestStopService_HandlesAlreadyExitedProcess(t *testing.T) {
	tmpDir := t.TempDir()

	// Process that exits immediately
	m := newTestManager([]config.Service{
		{Name: "fast", Dir: tmpDir, Cmd: "true"},
	})

	cmd := m.StartAll()
	cmd()

	// Wait for the process to exit on its own
	deadline := time.After(3 * time.Second)
	for {
		svc := m.Services[0]
		svc.mu.Lock()
		status := svc.Status
		svc.mu.Unlock()

		if status == StatusStopped || status == StatusCrashed {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for process to exit")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Now call stopService on the already-exited process — should not hang
	done := make(chan struct{})
	go func() {
		m.stopService(0)
		close(done)
	}()

	select {
	case <-done:
		// Good
	case <-time.After(3 * time.Second):
		t.Fatal("stopService blocked on already-exited process")
	}
}

func TestStopAll_QuittingFlagPreventsAutoRestart(t *testing.T) {
	tmpDir := t.TempDir()

	m := newTestManager([]config.Service{
		{Name: "crasher", Dir: tmpDir, Cmd: "sleep 60", Restart: "always", RestartDelay: "100ms"},
	})

	cmd := m.StartAll()
	cmd()

	// Verify service is running
	svc := m.Services[0]
	svc.mu.Lock()
	status := svc.Status
	svc.mu.Unlock()

	if status != StatusRunning {
		t.Fatalf("expected running before StopAll, got %v", status)
	}

	m.StopAll()

	// Wait a bit longer than the restart delay to ensure no restart fires
	time.Sleep(500 * time.Millisecond)

	svc.mu.Lock()
	restartCount := svc.RestartCount
	finalStatus := svc.Status
	svc.mu.Unlock()

	if restartCount != 0 {
		t.Errorf("expected 0 restarts during shutdown, got %d", restartCount)
	}
	if finalStatus == StatusRunning || finalStatus == StatusRestarting {
		t.Errorf("service should not be running/restarting after StopAll, got %v", finalStatus)
	}
}

func TestAutoRestart_QuittingFlagDuringDelay(t *testing.T) {
	tmpDir := t.TempDir()

	// Service that crashes immediately with a long restart delay
	m := newTestManager([]config.Service{
		{Name: "crasher", Dir: tmpDir, Cmd: "exit 1", Restart: "on-failure", RestartDelay: "5s"},
	})

	cmd := m.StartAll()
	cmd()

	// Wait for it to enter restarting status (crash + begin restart delay)
	deadline := time.After(3 * time.Second)
	for {
		svc := m.Services[0]
		svc.mu.Lock()
		status := svc.Status
		svc.mu.Unlock()

		if status == StatusRestarting {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for restarting status")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Now call StopAll while it's in the restart delay sleep
	done := make(chan struct{})
	go func() {
		m.StopAll()
		close(done)
	}()

	// StopAll should return quickly — not wait for the 5s restart delay
	select {
	case <-done:
		// Good
	case <-time.After(3 * time.Second):
		t.Fatal("StopAll blocked waiting for restart delay to finish")
	}

	// The service should NOT have been restarted
	svc := m.Services[0]
	svc.mu.Lock()
	restartCount := svc.RestartCount
	svc.mu.Unlock()

	// It entered restarting once, but the quitting flag should prevent
	// the actual restart from happening after the delay completes
	if restartCount > 1 {
		t.Errorf("expected at most 1 restart count, got %d", restartCount)
	}
}

// ClearLogs is the running-supervisor counterpart to `pairin --clear-logs`.
// It truncates rather than unlinks, because the service holds an open fd and
// would otherwise keep writing to an inode nobody can read.
func TestClearLogs(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Project:  config.Project{Name: "test"},
		Services: []config.Service{{Name: "alpha", Cmd: "true"}, {Name: "beta", Cmd: "true"}},
		Path:     filepath.Join(dir, ".pairinrc.toml"),
	}
	mgr := NewManager(cfg)

	if err := state.EnsureDirs(cfg.Path); err != nil {
		t.Fatalf("state dirs: %v", err)
	}
	for _, svc := range mgr.Services {
		if err := os.WriteFile(svc.LogFile, []byte("old line\nanother\n"), 0o644); err != nil {
			t.Fatalf("seeding log: %v", err)
		}
		if err := os.WriteFile(svc.LogFile+".1", []byte("rotated\n"), 0o644); err != nil {
			t.Fatalf("seeding rotated log: %v", err)
		}
		svc.Logs.Add("buffered line")
	}

	// Clearing one service leaves the other alone.
	mgr.ClearLogs("alpha")

	alpha, beta := mgr.Services[0], mgr.Services[1]

	if info, err := os.Stat(alpha.LogFile); err != nil {
		t.Errorf("alpha's log was removed rather than truncated: %v", err)
	} else if info.Size() != 0 {
		t.Errorf("alpha's log is %d bytes, want 0", info.Size())
	}
	if _, err := os.Stat(alpha.LogFile + ".1"); !os.IsNotExist(err) {
		t.Errorf("alpha's rotated log survived: %v", err)
	}
	if got := alpha.GetLines(); len(got) != 0 {
		t.Errorf("alpha's ring buffer still holds %v", got)
	}

	if info, err := os.Stat(beta.LogFile); err != nil || info.Size() == 0 {
		t.Errorf("beta's log was cleared too")
	}
	if got := beta.GetLines(); len(got) != 1 {
		t.Errorf("beta's ring buffer = %v, want its one line", got)
	}

	// An empty name clears everything.
	mgr.ClearLogs("")
	if info, err := os.Stat(beta.LogFile); err != nil || info.Size() != 0 {
		t.Errorf("clearing all did not empty beta's log")
	}
	if got := beta.GetLines(); len(got) != 0 {
		t.Errorf("clearing all left beta's buffer holding %v", got)
	}
}

// A truncated log must still be appended to correctly. Services are started
// with O_APPEND, so writes resume from zero rather than leaving a sparse gap
// where the old contents were.
func TestClearLogsKeepsAppendWritesContiguous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.log")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := f.WriteString("before clearing\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := state.TruncateLog(path); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// The same still-open handle keeps writing.
	if _, err := f.WriteString("after clearing\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "after clearing\n" {
		t.Errorf("log holds %q, want just the line written after clearing", data)
	}
}

// Ports are discovered from the kernel rather than declared, so this binds a
// real socket and checks it reaches the service the manager reports.
func TestRefreshPortsDiscoversARealListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port discovery reads /proc; only implemented for Linux")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	want := ln.Addr().(*net.TCPAddr).Port

	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}

	mgr := newTestManager([]config.Service{{Name: "listener"}, {Name: "silent"}})
	// Point the first service at this process's group; leave the second with
	// no process group at all, as a stopped service has.
	mgr.Services[0].PGID = pgid

	var mu sync.Mutex
	var msgs []PortsMsg
	mgr.SetSink(sinkFunc(func(m tea.Msg) {
		if pm, ok := m.(PortsMsg); ok {
			mu.Lock()
			msgs = append(msgs, pm)
			mu.Unlock()
		}
	}))

	mgr.refreshPorts()

	found := mgr.Services[0].View().Ports
	if !containsPort(found, want) {
		t.Errorf("service ports = %v, want them to include %d", found, want)
	}
	if got := mgr.Services[1].View().Ports; len(got) != 0 {
		t.Errorf("a service with no process group reported ports %v", got)
	}

	mu.Lock()
	first := len(msgs)
	mu.Unlock()
	if first == 0 {
		t.Fatal("discovering ports published no event")
	}

	// Nothing changed, so a second pass must stay quiet rather than re-announce
	// the same ports to every connected client every poll.
	mgr.refreshPorts()
	mu.Lock()
	second := len(msgs)
	mu.Unlock()
	if second != first {
		t.Errorf("unchanged ports published %d more events", second-first)
	}
}

func containsPort(ports []int, want int) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}

type sinkFunc func(tea.Msg)

func (f sinkFunc) Send(msg tea.Msg) { f(msg) }
