package network

import (
	"testing"
)

func TestParseHostRange(t *testing.T) {
	lo, hi, err := parseHostRange("32000-32999")
	if err != nil || lo != 32000 || hi != 32999 {
		t.Fatalf("got %d-%d err=%v", lo, hi, err)
	}
	for _, bad := range []string{"", "32000", "abc-10", "10-abc", "0-10", "10-70000", "100-50"} {
		if _, _, err := parseHostRange(bad); err == nil {
			t.Errorf("parseHostRange(%q) should fail", bad)
		}
	}
}

func TestAllocatorReserveRelease(t *testing.T) {
	a := newAllocator(mustPrefix("10.200.0.0/16").Addr(), 1<<14)
	one, err := a.reserve("ws_1")
	if err != nil {
		t.Fatal(err)
	}
	if one.HubAddr.String() != "10.200.0.1" || one.GuestAddr.String() != "10.200.0.2" {
		t.Fatalf("first /30 wrong: hub=%s guest=%s", one.HubAddr, one.GuestAddr)
	}
	two, err := a.reserve("ws_2")
	if err != nil {
		t.Fatal(err)
	}
	if two.HubAddr.String() != "10.200.0.5" || two.GuestAddr.String() != "10.200.0.6" {
		t.Fatalf("second /30 wrong: hub=%s guest=%s", two.HubAddr, two.GuestAddr)
	}
	// Idempotent.
	again, err := a.reserve("ws_1")
	if err != nil || again.HubAddr != one.HubAddr {
		t.Fatalf("reserve must be idempotent: %v %v", again, err)
	}
	// Release and reuse.
	a.release("ws_1")
	if _, ok := a.byAddr[one.HubAddr]; ok {
		t.Fatal("released address still indexed")
	}
	three, err := a.reserve("ws_3")
	if err != nil {
		t.Fatal(err)
	}
	if three.HubAddr == two.HubAddr {
		t.Fatal("collision after release")
	}
}

func TestShortIDStable(t *testing.T) {
	id := "ws_01HZZZZZZZZZZZZZZZZZZZZZZZ"
	if shortID(id) != shortID(id) {
		t.Fatal("shortID must be stable")
	}
	if len(shortID(id)) != 8 {
		t.Fatalf("shortID length = %d", len(shortID(id)))
	}
	if shortID("a") == shortID("b") {
		t.Fatal("shortID collision on distinct ids")
	}
}

func TestWriteHTTPError(t *testing.T) {
	var buf writer
	writeHTTPError(&buf, 502, "port_unreachable", "not reachable")
	got := buf.String()
	for _, want := range []string{"HTTP/1.1 502 Bad Gateway", `"code":"port_unreachable"`, `"message":"not reachable"`} {
		if !contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

type writer struct{ b []byte }

func (w *writer) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }
func (w *writer) String() string              { return string(w.b) }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
