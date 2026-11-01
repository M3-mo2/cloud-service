package network

import (
	"testing"
)

func TestAllocatorReserveRelease(t *testing.T) {
	a := newAllocator(mustPrefix("10.201.0.0/16").Addr(), 1<<8)
	one, err := a.reserve("ws_1")
	if err != nil {
		t.Fatal(err)
	}
	if one.CIDR.String() != "10.201.0.0/24" {
		t.Fatalf("first /24 wrong: %s", one.CIDR)
	}
	if one.GuestAddr.String() != "10.201.0.100" || one.GatewayAddr.String() != "10.201.0.2" {
		t.Fatalf("addresses wrong: guest=%s gw=%s", one.GuestAddr, one.GatewayAddr)
	}
	two, err := a.reserve("ws_2")
	if err != nil {
		t.Fatal(err)
	}
	if two.CIDR.String() != "10.201.1.0/24" {
		t.Fatalf("second /24 wrong: %s", two.CIDR)
	}
	// Idempotent.
	again, err := a.reserve("ws_1")
	if err != nil || again.CIDR != one.CIDR {
		t.Fatalf("reserve must be idempotent: %v %v", again, err)
	}
	// Release and reuse.
	a.release("ws_1")
	if _, ok := a.byWS["ws_1"]; ok {
		t.Fatal("released workspace still indexed")
	}
	if _, ok := a.byIndex[0]; ok {
		t.Fatal("released index still indexed")
	}
}

func TestMatchTunnelURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"INF |  https://random-words-here.trycloudflare.com  |", "https://random-words-here.trycloudflare.com"},
		{"https://api.trycloudflare.com", ""},
		{"no url here", ""},
		{"INF Requesting new quick Tunnel on trycloudflare.com...\nINF https://calm-piano-abc.trycloudflare.com", "https://calm-piano-abc.trycloudflare.com"},
		// api.trycloudflare.com plus the real URL: only the real one matches.
		{"api.trycloudflare.com https://real-one.trycloudflare.com", "https://real-one.trycloudflare.com"},
	}
	for _, c := range cases {
		if got := matchTunnelURL(c.in); got != c.want {
			t.Errorf("matchTunnelURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsQuickTunnelArgs(t *testing.T) {
	if !isQuickTunnelArgs([]string{"cloudflared", "tunnel", "--no-autoupdate", "--url", "http://localhost:5000"}) {
		t.Error("quick tunnel args must be recognised")
	}
	if !isQuickTunnelArgs([]string{"cloudflared", "tunnel", "--url=http://localhost:5000"}) {
		t.Error("--url= form must be recognised")
	}
	// A named/token tunnel is not a quick tunnel and must not be reaped.
	if isQuickTunnelArgs([]string{"cloudflared", "tunnel", "--token", "abc", "run", "my-tunnel"}) {
		t.Error("a named tunnel must not be treated as a quick tunnel")
	}
	if isQuickTunnelArgs([]string{"cloudflared", "tunnel", "run", "my-tunnel"}) {
		t.Error("a run tunnel must not be treated as a quick tunnel")
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

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/run/netns/sbxdrelay"); got != "'/run/netns/sbxdrelay'" {
		t.Fatalf("shellQuote = %q", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote escaping = %q", got)
	}
}
