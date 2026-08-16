package ports

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// fakeProc builds a /proc-shaped tree so the parsing can be tested without
// depending on what happens to be running on the machine.
type fakeProc struct{ root string }

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	p := &fakeProc{root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(p.root, "net"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return p
}

// listeners writes a net/tcp file. Each entry is inode -> port.
func (p *fakeProc) listeners(t *testing.T, name string, entries map[string]int, state string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	i := 0
	for inode, port := range entries {
		b.WriteString(fmt.Sprintf("  %2d: 0100007F:%04X 00000000:0000 %s 00000000:00000000 00:00000000 00000000  1000        0 %s 1\n",
			i, port, state, inode))
		i++
	}
	if err := os.WriteFile(filepath.Join(p.root, name), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// process creates a fake process in a process group, holding the given socket inodes.
func (p *fakeProc) process(t *testing.T, pid, pgid int, comm string, inodes ...string) {
	t.Helper()
	dir := filepath.Join(p.root, fmt.Sprint(pid))
	fd := filepath.Join(dir, "fd")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// pid (comm) state ppid pgrp ...
	stat := fmt.Sprintf("%d (%s) S 1 %d 0 0 -1 4194304 0 0\n", pid, comm, pgid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	for i, inode := range inodes {
		// A dangling symlink: readlink returns the text, which is what /proc does.
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(fd, fmt.Sprint(i))); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
}

func TestScanFindsPortsByProcessGroup(t *testing.T) {
	p := newFakeProc(t)
	p.listeners(t, "net/tcp", map[string]int{"111": 40200, "222": 40201, "333": 9229}, listenState)

	// One service: a shell whose child holds the listening socket.
	p.process(t, 100, 100, "sh")
	p.process(t, 101, 100, "api", "111")
	// A second service, listening on two ports.
	p.process(t, 200, 200, "web", "222", "333")
	// Someone else's process entirely.
	p.process(t, 300, 300, "unrelated", "999")

	got := scan(p.root, []int{100, 200})

	if want := []int{40200}; !equal(got[100], want) {
		t.Errorf("pgid 100 ports = %v, want %v", got[100], want)
	}
	// Sorted ascending, not in file order.
	if want := []int{9229, 40201}; !equal(got[200], want) {
		t.Errorf("pgid 200 ports = %v, want %v", got[200], want)
	}
	if _, ok := got[300]; ok {
		t.Errorf("reported ports for a process group nobody asked about: %v", got[300])
	}
}

// A socket that isn't listening is a connection, not something you can visit.
func TestScanIgnoresNonListeningSockets(t *testing.T) {
	p := newFakeProc(t)
	p.listeners(t, "net/tcp", map[string]int{"111": 40200}, "01") // ESTABLISHED
	p.process(t, 100, 100, "api", "111")

	if got := scan(p.root, []int{100}); len(got) != 0 {
		t.Errorf("established connection reported as a listening port: %v", got)
	}
}

// A service bound on both IPv4 and IPv6 holds two sockets on one port, and
// that is one port to a reader.
func TestScanDeduplicatesAcrossAddressFamilies(t *testing.T) {
	p := newFakeProc(t)
	p.listeners(t, "net/tcp", map[string]int{"111": 40200}, listenState)
	p.listeners(t, "net/tcp6", map[string]int{"222": 40200}, listenState)
	p.process(t, 100, 100, "api", "111", "222")

	if want := []int{40200}; !equal(scan(p.root, []int{100})[100], want) {
		t.Errorf("ports = %v, want %v", scan(p.root, []int{100})[100], want)
	}
}

// Process names can contain spaces and parentheses, which is why the stat parse
// starts after the last closing paren rather than counting fields from the left.
func TestScanHandlesAwkwardProcessNames(t *testing.T) {
	p := newFakeProc(t)
	p.listeners(t, "net/tcp", map[string]int{"111": 40200}, listenState)
	p.process(t, 100, 100, "we (ird) name", "111")

	if want := []int{40200}; !equal(scan(p.root, []int{100})[100], want) {
		t.Errorf("a process with parens in its name was missed")
	}
}

func TestScanEdgeCases(t *testing.T) {
	p := newFakeProc(t)
	p.listeners(t, "net/tcp", map[string]int{"111": 40200}, listenState)
	p.process(t, 100, 100, "api", "111")

	if got := scan(p.root, nil); got != nil {
		t.Errorf("scan with no process groups = %v, want nil", got)
	}
	if got := scan(p.root, []int{0, -1}); got != nil {
		t.Errorf("scan with invalid pgids = %v, want nil", got)
	}
	if got := scan(filepath.Join(p.root, "nope"), []int{100}); got != nil {
		t.Errorf("scan of a missing proc root = %v, want nil", got)
	}
}

// The real thing, against the real kernel: bind a port in this process and
// confirm it is discovered under this process's own group.
func TestListeningFindsARealSocket(t *testing.T) {
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

	got := Listening([]int{pgid})
	for _, port := range got[pgid] {
		if port == want {
			return
		}
	}
	t.Errorf("port %d is listening in this process group but was not found; got %v", want, got[pgid])
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
