package control

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/process"
)

// newTestServer starts a control server backed by a manager whose services are
// never launched — these tests exercise the transport, not process control.
func newTestServer(t *testing.T) (*Server, *process.Manager, string) {
	t.Helper()

	dir := t.TempDir()
	cfg := &config.Config{
		Project:  config.Project{Name: "test"},
		Services: []config.Service{{Name: "alpha", Cmd: "true"}, {Name: "beta", Cmd: "true"}},
		Path:     filepath.Join(dir, ".pairinrc.toml"),
	}

	mgr := process.NewManager(cfg)
	srv := NewServer(mgr, cfg)

	// Unix socket paths are limited to ~100 bytes, so keep this short rather
	// than nesting it under the temp config's .pairin directory.
	sock := filepath.Join(dir, "c.sock")
	if err := srv.Start(sock); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	return srv, mgr, sock
}

func readEvent(t *testing.T, dec *json.Decoder, conn net.Conn, timeout time.Duration) Event {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("setting read deadline: %v", err)
	}
	var evt Event
	if err := dec.Decode(&evt); err != nil {
		t.Fatalf("decoding event: %v", err)
	}
	return evt
}

// TestSnapshotOnConnect covers the protocol's one guarantee to a new client:
// the first thing it receives describes the whole world.
func TestSnapshotOnConnect(t *testing.T) {
	_, _, sock := newTestServer(t)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()

	evt := readEvent(t, json.NewDecoder(conn), conn, 2*time.Second)
	if evt.Kind != EvtSnapshot {
		t.Fatalf("first event = %q, want %q", evt.Kind, EvtSnapshot)
	}
	if got := len(evt.Snapshot.Services); got != 2 {
		t.Fatalf("snapshot has %d services, want 2", got)
	}
}

// TestWedgedClientDoesNotBlockOthers is the regression test for the bug that
// motivated the send queue: a client that stops reading its socket used to
// block the broadcast loop, and with it the Manager goroutine that produced
// the event and every other connected client.
func TestWedgedClientDoesNotBlockOthers(t *testing.T) {
	srv, _, sock := newTestServer(t)

	// A client that connects and then never reads a byte.
	wedged, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing wedged client: %v", err)
	}
	defer wedged.Close()

	healthy, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing healthy client: %v", err)
	}
	defer healthy.Close()

	dec := json.NewDecoder(healthy)
	readEvent(t, dec, healthy, 2*time.Second) // initial snapshot

	// Give both clients time to be registered before flooding.
	time.Sleep(50 * time.Millisecond)

	// Far more data than a socket buffer will hold for the wedged client.
	const floodSize = clientQueueSize * 3
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < floodSize; i++ {
			srv.broadcast(Event{Kind: EvtLog, Log: &LogEvent{
				Service: "alpha",
				Line:    fmt.Sprintf("line %d %s", i, string(make([]byte, 256))),
			}})
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("broadcast blocked on a wedged client")
	}

	// The healthy client must still be receiving.
	for i := 0; i < 10; i++ {
		evt := readEvent(t, dec, healthy, 5*time.Second)
		if evt.Kind != EvtLog && evt.Kind != EvtSnapshot {
			t.Fatalf("unexpected event kind %q", evt.Kind)
		}
	}
}

// TestOverflowTriggersResync checks the correctness half of the drop policy:
// a client that falls behind loses events, so once it catches up the server
// must hand it a fresh snapshot rather than leave it with a partial view.
func TestOverflowTriggersResync(t *testing.T) {
	srv, _, sock := newTestServer(t)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()

	dec := json.NewDecoder(conn)
	readEvent(t, dec, conn, 2*time.Second) // initial snapshot

	// Overflow the queue while the client isn't reading.
	for i := 0; i < clientQueueSize*3; i++ {
		srv.broadcast(Event{Kind: EvtLog, Log: &LogEvent{Service: "alpha", Line: "x"}})
	}

	// Drain until a second snapshot shows up.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		evt := readEvent(t, dec, conn, 5*time.Second)
		if evt.Kind == EvtSnapshot {
			return
		}
	}
	t.Fatal("no resync snapshot after dropping events")
}

// TestClientReconnect covers a supervisor restart: the client must recover
// without the caller rebuilding it, and the mirrored Service pointers the TUI
// is holding must survive the round trip.
func TestClientReconnect(t *testing.T) {
	srv, mgr, sock := newTestServer(t)

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer client.Close()

	before := client.ServiceList()
	if len(before) != 2 {
		t.Fatalf("mirrored %d services, want 2", len(before))
	}

	// Supervisor goes away.
	srv.Shutdown()
	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client did not notice the supervisor going away")
	}

	// A new supervisor comes up on the same socket.
	cfg := &config.Config{
		Project:  config.Project{Name: "test"},
		Services: []config.Service{{Name: "alpha", Cmd: "true"}, {Name: "beta", Cmd: "true"}},
		Path:     filepath.Join(filepath.Dir(sock), ".pairinrc.toml"),
	}
	srv2 := NewServer(mgr, cfg)
	if err := srv2.Start(sock); err != nil {
		t.Fatalf("restarting server: %v", err)
	}
	defer srv2.Shutdown()

	if err := client.Reconnect(); err != nil {
		t.Fatalf("reconnecting: %v", err)
	}

	after := client.ServiceList()
	if len(after) != len(before) {
		t.Fatalf("service count changed across reconnect: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("service pointer %d was replaced across reconnect", i)
		}
	}

	// Done() must hand back the live channel, not the one that already closed.
	select {
	case <-client.Done():
		t.Fatal("Done() still reports the previous connection as closed")
	default:
	}
}

// TestConcurrentMirrorAccess is a race-detector test: it reproduces the shape
// of the TUI rendering a service while the read loop is updating it.
func TestConcurrentMirrorAccess(t *testing.T) {
	_, _, sock := newTestServer(t)

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer client.Close()

	svc := client.ServiceList()[0]

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			client.apply(Event{Kind: EvtStatus, Status: &StatusEvent{
				Service: "alpha", Status: "running", PID: i, Branch: "main", RestartCount: i,
			}})
			client.apply(Event{Kind: EvtLog, Log: &LogEvent{Service: "alpha", Line: "line"}})
			client.apply(Event{Kind: EvtHealth, Health: &HealthEvent{Service: "alpha", Healthy: i%2 == 0}})
		}
	}()

	for i := 0; i < 2000; i++ {
		v := svc.View()
		_ = v.Status
		_ = v.PID
		_ = svc.GetLines()
	}
	<-done
}

// TestLogSubscriptionFilters covers the protocol addition the fleet dashboard
// depends on: with a socket open per project, streaming every log line on the
// host to render a grid of service names would be pure waste.
func TestLogSubscriptionFilters(t *testing.T) {
	srv, _, sock := newTestServer(t)

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer client.Close()

	var mu sync.Mutex
	var logs, statuses int
	client.SetSink(sinkFunc(func(msg tea.Msg) {
		mu.Lock()
		defer mu.Unlock()
		switch msg.(type) {
		case process.LogMsg:
			logs++
		case process.StatusMsg:
			statuses++
		}
	}))

	counts := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return logs, statuses
	}

	if err := client.SubscribeLogs(LogsNone); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// The subscription is applied on the server's read loop; give it a moment.
	time.Sleep(100 * time.Millisecond)

	srv.broadcast(Event{Kind: EvtLog, Log: &LogEvent{Service: "alpha", Line: "hidden"}})
	srv.broadcast(Event{Kind: EvtStatus, Status: &StatusEvent{Service: "alpha", Status: "running"}})
	time.Sleep(200 * time.Millisecond)

	if gotLogs, gotStatuses := counts(); gotLogs != 0 || gotStatuses != 1 {
		t.Errorf("with LogsNone: %d logs, %d statuses; want 0 and 1", gotLogs, gotStatuses)
	}

	// Narrowing to one service must pass that service and no other.
	if err := client.SubscribeLogs(LogsOnly, "alpha"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	srv.broadcast(Event{Kind: EvtLog, Log: &LogEvent{Service: "alpha", Line: "wanted"}})
	srv.broadcast(Event{Kind: EvtLog, Log: &LogEvent{Service: "beta", Line: "unwanted"}})
	time.Sleep(200 * time.Millisecond)

	if gotLogs, _ := counts(); gotLogs != 1 {
		t.Errorf("with LogsOnly(alpha): %d logs, want 1", gotLogs)
	}
}

// A subscription is per-connection server state, so a reconnect that didn't
// re-send it would silently resume the full firehose.
func TestSubscriptionSurvivesReconnect(t *testing.T) {
	srv, mgr, sock := newTestServer(t)

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer client.Close()

	if err := client.SubscribeLogs(LogsNone); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	srv.Shutdown()
	<-client.Done()

	cfg := &config.Config{
		Project:  config.Project{Name: "test"},
		Services: []config.Service{{Name: "alpha", Cmd: "true"}, {Name: "beta", Cmd: "true"}},
		Path:     filepath.Join(filepath.Dir(sock), ".pairinrc.toml"),
	}
	srv2 := NewServer(mgr, cfg)
	if err := srv2.Start(sock); err != nil {
		t.Fatalf("restarting server: %v", err)
	}
	defer srv2.Shutdown()

	var mu sync.Mutex
	logs := 0
	client.SetSink(sinkFunc(func(msg tea.Msg) {
		if _, ok := msg.(process.LogMsg); ok {
			mu.Lock()
			logs++
			mu.Unlock()
		}
	}))

	if err := client.Reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	srv2.broadcast(Event{Kind: EvtLog, Log: &LogEvent{Service: "alpha", Line: "should not arrive"}})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if logs != 0 {
		t.Errorf("received %d log events after reconnect; the subscription was not re-sent", logs)
	}
}

// sinkFunc adapts a function to process.Sink.
type sinkFunc func(tea.Msg)

func (f sinkFunc) Send(msg tea.Msg) { f(msg) }
