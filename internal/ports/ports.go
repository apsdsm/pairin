// Package ports discovers which TCP ports a service is listening on, by
// inspecting the kernel's socket table rather than asking the config.
//
// A declared port (a healthcheck, say) says what a service is *supposed* to
// expose. This says what it actually does — which catches the dev servers whose
// port lives inside a framework config pairin can't see, and catches a service
// listening somewhere nobody wrote down.
//
// The lookup is by process group. Services are started with Setpgid, so every
// descendant shares the service's PGID however many shells and wrappers the
// command goes through.
//
// Known blind spot: a service that runs `docker compose up` has its ports bound
// by the docker daemon, which is in nobody's process group but its own. Those
// services report nothing, correctly — the ports exist, but not in any process
// this service owns.
package ports

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// procRoot is the kernel's process filesystem. Overridden in tests.
const procRoot = "/proc"

// listenState is the value /proc/net/tcp uses for TCP_LISTEN.
const listenState = "0A"

// Listening returns the TCP ports each process group is listening on, keyed by
// PGID and sorted ascending. Process groups with no listeners are absent.
//
// Errors are swallowed: this is decoration on a dashboard, and a permission
// problem reading one process's file descriptors is not worth failing over.
func Listening(pgids []int) map[int][]int {
	return scan(procRoot, pgids)
}

func scan(root string, pgids []int) map[int][]int {
	if len(pgids) == 0 {
		return nil
	}
	wanted := make(map[int]bool, len(pgids))
	for _, p := range pgids {
		if p > 0 {
			wanted[p] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	// inode -> port, for listening sockets only.
	byInode := map[string]int{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		readListeners(filepath.Join(root, name), byInode)
	}
	if len(byInode) == 0 {
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	found := map[int]map[int]bool{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		pgid, ok := processGroup(filepath.Join(root, e.Name(), "stat"))
		if !ok || !wanted[pgid] {
			continue
		}

		for _, inode := range socketInodes(filepath.Join(root, e.Name(), "fd")) {
			port, ok := byInode[inode]
			if !ok {
				continue
			}
			if found[pgid] == nil {
				found[pgid] = map[int]bool{}
			}
			// Deduplicated: a service bound on both IPv4 and IPv6 holds two
			// sockets on one port, and that is one port to a reader.
			found[pgid][port] = true
		}
	}

	if len(found) == 0 {
		return nil
	}
	out := make(map[int][]int, len(found))
	for pgid, set := range found {
		list := make([]int, 0, len(set))
		for port := range set {
			list = append(list, port)
		}
		sort.Ints(list)
		out[pgid] = list
	}
	return out
}

// readListeners adds the listening sockets in a /proc/net/tcp-style file to
// byInode. The columns are: sl, local_address, rem_address, st, ..., inode.
func readListeners(path string, byInode map[string]int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != listenState {
			continue
		}
		colon := strings.LastIndex(f[1], ":")
		if colon < 0 {
			continue
		}
		port, err := strconv.ParseUint(f[1][colon+1:], 16, 32)
		if err != nil || port == 0 {
			continue
		}
		byInode[f[9]] = int(port)
	}
}

// processGroup reads a process's PGID out of its stat file.
//
// The fields are positional, but the second one is the executable name in
// parentheses and may itself contain spaces and parentheses — so the parse
// starts after the *last* closing paren, where state, ppid and pgrp follow.
func processGroup(statPath string) (int, bool) {
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	close := strings.LastIndex(string(data), ")")
	if close < 0 {
		return 0, false
	}
	f := strings.Fields(string(data)[close+1:])
	if len(f) < 3 {
		return 0, false
	}
	pgid, err := strconv.Atoi(f[2])
	if err != nil {
		return 0, false
	}
	return pgid, true
}

// socketInodes returns the socket inodes a process holds open. File descriptor
// links read as "socket:[12345]".
func socketInodes(fdDir string) []string {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue // the fd closed between listing and reading
		}
		if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		out = append(out, target[len("socket:[") : len(target)-1])
	}
	return out
}
