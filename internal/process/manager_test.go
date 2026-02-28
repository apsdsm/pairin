package process

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/apsdsm/pairin/internal/config"
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
