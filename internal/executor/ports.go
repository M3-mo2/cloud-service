package executor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sandboxd/internal/ports"
)

// This file implements the host-side reads behind native port discovery. The
// caller (the service) already knows WHICH pids belong to a workspace from the
// process registry, so this layer never scans /proc blindly: it reads the
// network tables of one of those pids (a pid that is inside the workspace's
// network namespace) and attributes each socket inode to the candidate pids by
// scanning their /proc/<pid>/fd. The work is bounded by ports.Limits and an
// explicit Complete flag reports whether a bound was hit.
//
// The whole point of reading /proc/<pid>/net/* is that the pid is ALREADY inside
// the workspace's namespace, so no namespace handling is needed. `ip netns exec
// <ns> ss` is deliberately not used: on the target host it fails with "Cannot
// open network namespace", because the workspace namespace is created by slirp
// rather than by `ip netns add`. See docs/DECISIONS.md ADR 96.

// netTableFiles are the /proc/<pid>/net/* tables read for a workspace. They are
// the connectionless tables for tcp/tcp6/udp/udp6 plus the unix table.
var netTableFiles = []string{"tcp", "tcp6", "udp", "udp6", "unix"}

// NetTablesForPids reads the /proc/<pid>/net/* tables for the first candidate
// pid that is still alive (a pid inside the workspace namespace) and attributes
// each socket inode to the candidate pids by scanning /proc/<pid>/fd. Only the
// supplied pids are examined; a pid that vanishes mid-scan is skipped, and a
// socket whose inode no candidate holds is left unattributed (reported, never
// dropped, never guessed).
//
// The work is bounded by limits: at most limits.MaxPids candidates are scanned
// and at most limits.MaxFDsPerPID file descriptors per pid. When a bound is hit,
// Complete is false and Notes says which one.
func NetTablesForPids(pids []int, limits ports.Limits) ports.NetTables {
	if limits.MaxPids <= 0 {
		limits.MaxPids = ports.DefaultLimits().MaxPids
	}
	if limits.MaxFDsPerPID <= 0 {
		limits.MaxFDsPerPID = ports.DefaultLimits().MaxFDsPerPID
	}

	candidates, pidCapHit := boundPids(pids, limits.MaxPids)
	nt := ports.NetTables{Holders: map[uint64][]int{}, Complete: true}
	if pidCapHit {
		nt.Complete = false
		nt.Notes = append(nt.Notes, "pid cap reached")
	}

	// Read the tables from a live candidate. All candidates in a workspace share
	// one network namespace, so any one of them yields the same tables.
	for _, pid := range candidates {
		if readNetTables(pid, &nt) {
			break
		}
	}

	// Attribute socket inodes over the candidates by scanning their open fds.
	fdCapHit := false
	for _, pid := range candidates {
		nt.PidsScanned++
		holders, scanned, capped := socketInodesOf(pid, limits.MaxFDsPerPID)
		nt.FDsScanned += scanned
		if capped {
			fdCapHit = true
		}
		for _, inode := range holders {
			nt.Holders[inode] = append(nt.Holders[inode], pid)
		}
	}
	if fdCapHit {
		nt.Complete = false
		nt.Notes = append(nt.Notes, "fd cap reached on a process")
	}
	return nt
}

// boundPids caps the candidate list at max, preserving order, and reports
// whether the cap was hit.
func boundPids(pids []int, max int) ([]int, bool) {
	seen := map[int]bool{}
	out := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
	}
	if len(out) > max {
		return out[:max], true
	}
	return out, false
}

// readNetTables reads the five tables for pid into nt. It returns true when the
// pid's /proc net directory could be read, so the caller stops trying other
// candidates. A missing per-protocol file is not fatal: the table is simply
// empty (a host without IPv6 has no tcp6). An unreadable tcp table means the pid
// is gone or /proc is restricted, and the caller tries the next candidate.
func readNetTables(pid int, nt *ports.NetTables) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "net")
	if _, err := os.Stat(filepath.Join(dir, "tcp")); err != nil {
		return false
	}
	if b, err := os.ReadFile(filepath.Join(dir, "tcp")); err == nil {
		nt.TCP = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, "tcp6")); err == nil {
		nt.TCP6 = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, "udp")); err == nil {
		nt.UDP = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, "udp6")); err == nil {
		nt.UDP6 = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, "unix")); err == nil {
		nt.Unix = b
	}
	return true
}

// socketInodesOf scans a process's /proc/<pid>/fd for socket:[<inode>] links and
// returns the inodes it holds, how many fds were examined, and whether the fd
// cap was hit. A pid that has exited (or whose /proc entry is unreadable) yields
// nothing rather than an error: it is simply no longer a candidate.
func socketInodesOf(pid int, maxFDs int) (inodes []uint64, scanned int, capped bool) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, false
	}
	for _, e := range entries {
		if scanned >= maxFDs {
			return inodes, scanned, true
		}
		scanned++
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if inode, ok := parseSocketLink(target); ok {
			inodes = append(inodes, inode)
		}
	}
	return inodes, scanned, false
}

// parseSocketLink parses a /proc/<pid>/fd symlink target of the form
// "socket:[<inode>]" and returns the inode. Any other target (a regular file, a
// pipe, an anon_inode) yields ok=false.
func parseSocketLink(target string) (uint64, bool) {
	if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	inner := target[len("socket:[") : len(target)-1]
	inode, err := strconv.ParseUint(inner, 10, 64)
	if err != nil {
		return 0, false
	}
	return inode, true
}
