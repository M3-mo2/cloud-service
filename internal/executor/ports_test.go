package executor

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"sandboxd/internal/ports"
)

func TestParseSocketLink(t *testing.T) {
	cases := []struct {
		in    string
		inode uint64
		ok    bool
	}{
		{"socket:[2704413670]", 2704413670, true},
		{"socket:[123]", 123, true},
		{"/dev/null", 0, false},
		{"anon_inode:[eventfd]", 0, false},
		{"socket:[]", 0, false},
		{"pipe:[123]", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSocketLink(c.in)
		if ok != c.ok || got != c.inode {
			t.Fatalf("parseSocketLink(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.inode, c.ok)
		}
	}
}

func TestBoundPidsCapsAndDedupes(t *testing.T) {
	got, capped := boundPids([]int{3, 1, 3, 0, -1, 2, 4}, 3)
	if !capped {
		t.Fatalf("expected the cap to be hit")
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("got %v", got)
	}
	got, capped = boundPids([]int{5, 6}, 10)
	if capped || len(got) != 2 {
		t.Fatalf("got %v capped=%v", got, capped)
	}
}

// TestNetTablesForPidsFindsOwnListener is a real live test of the host-read and
// attribution path without nsjail: it opens a TCP listener in THIS process and
// asserts NetTablesForPids, scanning this process's own /proc/<pid>/fd, finds the
// socket and attributes its inode to this pid. It exercises the exact primitive
// the endpoint uses (read /proc/<pid>/net/tcp, scan /proc/<pid>/fd).
func TestNetTablesForPidsFindsOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a listener: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// The socket inode this listener holds (from our own fd table).
	inodes := socketInodesOfSelf(t)
	pid := os.Getpid()

	nt := NetTablesForPids([]int{pid}, ports.DefaultLimits())
	if len(nt.TCP) == 0 {
		t.Fatalf("no /proc/%d/net/tcp was read", pid)
	}
	// Our listener must appear in the TCP table AND be attributed to us.
	res := ports.Build(nt, map[int]ports.ProcessRef{pid: {PID: pid, Kind: "run"}})
	var found bool
	for _, e := range res.Ports {
		if e.Port == port && e.Protocol == ports.ProtoTCP {
			found = true
			if !e.Attributed || e.PID != pid {
				t.Fatalf("listener on %d must be attributed to pid %d: %+v", port, pid, e)
			}
			if e.Scope != ports.ScopeLoopback {
				t.Fatalf("listener on 127.0.0.1 must be loopback scope: %+v", e)
			}
			if e.State != ports.StateListen {
				t.Fatalf("TCP listener state = %q, want LISTEN", e.State)
			}
		}
	}
	if !found {
		t.Fatalf("listener on port %d not found in the parsed tables (inodes held: %v)", port, inodes)
	}
}

// TestNetTablesForPidsUDPOwnSocket is the UDP counterpart: a bound UDP socket is
// reported and is attributed to this process, and it is not called LISTEN.
func TestNetTablesForPidsUDPOwnSocket(t *testing.T) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pid := os.Getpid()

	nt := NetTablesForPids([]int{pid}, ports.DefaultLimits())
	res := ports.Build(nt, map[int]ports.ProcessRef{pid: {PID: pid, Kind: "run"}})
	var found bool
	for _, e := range res.Ports {
		if e.Port == port && e.Protocol == ports.ProtoUDP {
			found = true
			if e.State != ports.StateUDPNone {
				t.Fatalf("UDP socket must not be reported as LISTEN: %+v", e)
			}
			if !e.Attributed || e.PID != pid {
				t.Fatalf("UDP socket on %d must be attributed to pid %d: %+v", port, pid, e)
			}
		}
	}
	if !found {
		t.Fatalf("bound UDP socket on port %d not found", port)
	}
}

// TestNetTablesForPidsGonePIDIsEmpty asserts a pid that does not exist yields an
// empty, still-complete read rather than an error.
func TestNetTablesForPidsGonePIDIsEmpty(t *testing.T) {
	nt := NetTablesForPids([]int{1 << 30}, ports.DefaultLimits())
	if len(nt.TCP) != 0 {
		t.Fatalf("a dead pid must yield no tables")
	}
	// Attribution was still attempted and completed within bound.
	if !nt.Complete {
		t.Fatalf("a dead pid is not a cap hit; complete must stay true")
	}
}

// socketInodesOfSelf returns the socket inodes this process holds, used to prove
// the attribution primitive finds a known fd.
func socketInodesOfSelf(t *testing.T) []uint64 {
	t.Helper()
	dir := "/proc/self/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []uint64
	for _, e := range entries {
		target, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		if inode, ok := parseSocketLink(target); ok {
			out = append(out, inode)
		}
	}
	return out
}

// TestSampleProcessStillWorks is a guard that the ports change did not disturb
// the process inventory primitive.
func TestSampleProcessStillWorks(t *testing.T) {
	pid := os.Getpid()
	info, err := sampleProcess(pid)
	if err != nil {
		t.Fatalf("sampleProcess(self): %v", err)
	}
	if info.PID != pid {
		t.Fatalf("pid = %d, want %d", info.PID, pid)
	}
	if _, err := strconv.Atoi(strconv.Itoa(pid)); err != nil {
		t.Fatalf("unreachable")
	}
	_ = time.Now()
}
