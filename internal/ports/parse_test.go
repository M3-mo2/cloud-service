package ports

import (
	"reflect"
	"testing"
)

// tcpHeader is the exact header row the kernel prints for tcp/tcp6/udp/udp6.
const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestParseTableTCPListen(t *testing.T) {
	data := tcpHeader +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2704413670 2 0000000000000000 100 0 0 10 0\n" +
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2704413671 2 0000000000000000 100 0 0 10 0\n"
	got := ParseTable(ProtoTCP, []byte(data))
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
	}
	// 0100007F is little-endian 127.0.0.1; 1F90 is 8080.
	if got[0].Addr.String() != "127.0.0.1" || got[0].Port != 8080 || got[0].StateHex != "0A" || got[0].Inode != 2704413670 {
		t.Fatalf("row0 = %+v", got[0])
	}
	// 00000000 is the wildcard 0.0.0.0; 0050 is 80.
	if got[1].Addr.String() != "0.0.0.0" || got[1].Port != 80 {
		t.Fatalf("row1 = %+v", got[1])
	}
	if scopeOf(got[0].Addr) != ScopeLoopback || scopeOf(got[1].Addr) != ScopeWildcard {
		t.Fatalf("scopes = %s %s", scopeOf(got[0].Addr), scopeOf(got[1].Addr))
	}
}

func TestParseTableTCP6(t *testing.T) {
	// ::1 is 00000000000000000000000001000000.
	data := tcpHeader +
		"   0: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2690712047 2 0000000000000000 100 0 0 10 0\n" +
		"   1: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2690712048 2 0000000000000000 100 0 0 10 0\n"
	got := ParseTable(ProtoTCP6, []byte(data))
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].Addr.String() != "::1" || got[0].Port != 8080 {
		t.Fatalf("row0 = %+v", got[0])
	}
	if got[1].Addr.String() != "::" || got[1].Port != 22 {
		t.Fatalf("row1 = %+v", got[1])
	}
	if scopeOf(got[0].Addr) != ScopeLoopback || scopeOf(got[1].Addr) != ScopeWildcard {
		t.Fatalf("scopes = %s %s", scopeOf(got[0].Addr), scopeOf(got[1].Addr))
	}
}

func TestParseTableUDPHasNoListenState(t *testing.T) {
	// A bound UDP socket: remote 0.0.0.0:0, state 07.
	data := tcpHeader +
		"   0: 00000000:1F90 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 2704413670 2 0000000000000000 0\n"
	got := ParseTable(ProtoUDP, []byte(data))
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	rep := reportable(got[0])
	if !rep {
		t.Fatalf("a bound UDP socket must be reportable: %+v", got[0])
	}
	if st := stateOf(got[0]); st != StateUDPNone {
		t.Fatalf("UDP state = %q, want the empty (no-listen) state", st)
	}
}

func TestParseTableSkipsConnectedUDPFlow(t *testing.T) {
	// A connected UDP client flow: remote is set (a DNS query in flight), so it
	// is not a service a user "has open".
	data := tcpHeader +
		"   0: 0770090A:A9FD 0470090A:0202 01 00000000:00000000 00:00000000 00000000  2023        0 2344121685 2 0000000000000000 0\n"
	got := ParseTable(ProtoUDP, []byte(data))
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if reportable(got[0]) {
		t.Fatalf("a connected UDP flow (remote set) must NOT be reported: %+v", got[0])
	}
}

func TestParseTableMalformedLinesSkipped(t *testing.T) {
	data := tcpHeader +
		"   0: nothex:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2704413670 2 0\n" + // bad addr
		"   1: 0100007F:ZZZZ 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2704413671 2 0\n" + // bad port
		"   2: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 notanumber 2 0\n" + // bad inode
		"   3: short line\n" + // too few fields
		"   4: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2704413672 2 0\n" // valid
	got := ParseTable(ProtoTCP, []byte(data))
	if len(got) != 1 || got[0].Inode != 2704413672 {
		t.Fatalf("want only the valid row, got %+v", got)
	}
}

func TestParseTableHeaderOnlyAndEmpty(t *testing.T) {
	if got := ParseTable(ProtoTCP, []byte(tcpHeader)); len(got) != 0 {
		t.Fatalf("header-only table yielded %+v", got)
	}
	if got := ParseTable(ProtoTCP, nil); len(got) != 0 {
		t.Fatalf("empty table yielded %+v", got)
	}
	if got := ParseTable(ProtoTCP, []byte("")); len(got) != 0 {
		t.Fatalf("blank table yielded %+v", got)
	}
}

func TestParseUnixTable(t *testing.T) {
	data := "Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 2344062687 /run/app.sock\n" +
		"0000000000000000: 00000003 00000000 00000000 0002 01 2344062688\n"
	got := parseUnixTable([]byte(data))
	if len(got) != 2 {
		t.Fatalf("want 2 unix rows, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/run/app.sock" || got[0].Type != "stream" || got[0].State != "connected" {
		t.Fatalf("row0 = %+v", got[0])
	}
	if got[1].Path != "" || got[1].Type != "dgram" || got[1].State != "unconnected" {
		t.Fatalf("row1 = %+v", got[1])
	}
}

func TestBuildSortsAndAttributes(t *testing.T) {
	tables := NetTables{
		TCP: []byte(tcpHeader +
			"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 100 2 0\n" + // 8080 wildcard
			"   1: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 101 2 0\n"), // 80 loopback
		TCP6: []byte(tcpHeader +
			"   0: 00000000000000000000000001000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 102 2 0\n"), // ::1:80
		UDP: []byte(tcpHeader +
			"   0: 00000000:2000 00000000:0000 07 00000000:00000000 00:00000000 00000000 0 0 103 2 0\n"), // 8192
		UDP6: []byte(tcpHeader +
			"   0: 00000000000000000000000000000000:2001 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000 0 0 104 2 0\n"), // 8193
		Holders:  map[uint64][]int{100: {7}, 101: {7}, 102: {8}, 103: {9}, 104: {9}},
		Complete: true,
	}
	procs := map[int]ProcessRef{
		7: {PID: 7, Kind: "run", Argv: []string{"/usr/bin/python3", "-m", "http.server", "8080"}},
		8: {PID: 8, Kind: "session", Argv: []string{"/usr/bin/node", "server.js"}},
		9: {PID: 9, Kind: "detached", Argv: []string{"dnsmasq", "-k"}},
	}
	res := Build(tables, procs)
	if res.Count != 5 {
		t.Fatalf("want 5 endpoints, got %d: %+v", res.Count, res.Ports)
	}
	if !res.Complete {
		t.Fatalf("complete should be true")
	}
	// Sorted by port then protocol: 80 tcp, 80 tcp6, 8080 tcp, 8192 udp, 8193 udp6.
	wantOrder := []struct {
		port  int
		proto string
	}{{80, ProtoTCP}, {80, ProtoTCP6}, {8080, ProtoTCP}, {8192, ProtoUDP}, {8193, ProtoUDP6}}
	for i, w := range wantOrder {
		if res.Ports[i].Port != w.port || res.Ports[i].Protocol != w.proto {
			t.Fatalf("order[%d] = %d/%s, want %d/%s", i, res.Ports[i].Port, res.Ports[i].Protocol, w.port, w.proto)
		}
	}
	// The 8080 tcp endpoint is wildcard and attributed to pid 7 with a label.
	e := res.Ports[2]
	if e.Scope != ScopeWildcard || !e.Attributed || e.PID != 7 || e.Kind != "run" {
		t.Fatalf("8080 endpoint = %+v", e)
	}
	if e.Label != "python3 -m http.server 8080" {
		t.Fatalf("label = %q", e.Label)
	}
	// The UDP endpoint has no LISTEN state.
	u := res.Ports[3]
	if u.State != StateUDPNone || u.Protocol != ProtoUDP {
		t.Fatalf("udp endpoint = %+v", u)
	}
}

func TestBuildUnattributedIsReportedNotDropped(t *testing.T) {
	tables := NetTables{
		TCP:      []byte(tcpHeader + "   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 555 2 0\n"),
		Holders:  map[uint64][]int{}, // no live pid holds inode 555
		Complete: true,
	}
	res := Build(tables, map[int]ProcessRef{})
	if res.Count != 1 {
		t.Fatalf("want 1 endpoint, got %d", res.Count)
	}
	if res.Ports[0].Attributed {
		t.Fatalf("endpoint must be unattributed: %+v", res.Ports[0])
	}
	if res.Ports[0].Port != 8080 || res.Ports[0].Label != "" {
		t.Fatalf("unattributed endpoint = %+v", res.Ports[0])
	}
}

func TestBuildEmptyIsEmpty(t *testing.T) {
	res := Build(NetTables{Complete: true}, nil)
	if res.Count != 0 || len(res.Ports) != 0 || res.UnixCount != 0 {
		t.Fatalf("empty tables should yield an empty result, got %+v", res)
	}
}

func TestBuildIncompletePropagates(t *testing.T) {
	res := Build(NetTables{Complete: false, Notes: []string{"pid cap reached"}, PidsScanned: 64, FDsScanned: 512}, nil)
	if res.Complete {
		t.Fatalf("complete must be false when the read hit a bound")
	}
	if !reflect.DeepEqual(res.Notes, []string{"pid cap reached"}) {
		t.Fatalf("notes = %+v", res.Notes)
	}
	if res.PidsScanned != 64 || res.FDsScanned != 512 {
		t.Fatalf("work counters = %d %d", res.PidsScanned, res.FDsScanned)
	}
}

func TestBuildMultipleOwnersDeterministic(t *testing.T) {
	tables := NetTables{
		TCP:      []byte(tcpHeader + "   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 700 2 0\n"),
		Holders:  map[uint64][]int{700: {12, 5, 12}},
		Complete: true,
	}
	procs := map[int]ProcessRef{12: {PID: 12, Kind: "run"}, 5: {PID: 5, Kind: "session"}}
	res := Build(tables, procs)
	if len(res.Ports) != 1 {
		t.Fatalf("want 1 endpoint")
	}
	e := res.Ports[0]
	if e.PID != 5 {
		t.Fatalf("primary owner must be the lowest pid, got %d", e.PID)
	}
	if !reflect.DeepEqual(e.Owners, []int{5, 12}) {
		t.Fatalf("owners = %+v", e.Owners)
	}
}

func TestLabelForTruncatesAndBasenames(t *testing.T) {
	l := LabelFor([]string{"/usr/local/bin/node", "/app/server.js"}, maxLabelLen)
	if l != "node /app/server.js" {
		t.Fatalf("label = %q", l)
	}
	long := make([]string, 0, 100)
	long = append(long, "cmd")
	for i := 0; i < 100; i++ {
		long = append(long, "argument")
	}
	if got := LabelFor(long, maxLabelLen); len(got) != maxLabelLen {
		t.Fatalf("long label len = %d, want %d", len(got), maxLabelLen)
	}
	if got := LabelFor(nil, maxLabelLen); got != "" {
		t.Fatalf("empty argv label = %q", got)
	}
}
