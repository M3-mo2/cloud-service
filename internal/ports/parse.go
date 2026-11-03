// Package ports implements native per-workspace port/service discovery for
// sandboxd: it answers, for a workspace, which ports its processes are listening
// on and which process serves each one.
//
// It exists because the TENANT CANNOT ANSWER THIS ITSELF. Inside a workspace the
// curated /proc does not carry /proc/net (see docs/DECISIONS.md ADR 86), and
// neither `ss` nor `netstat` is installed in the profile. A user's app can bind
// a port and the user has no way to see it from inside, so the engine is the
// only thing that can tell them.
//
// The facts this package reads are the pure Linux /proc/net text tables for a
// pid that is already INSIDE the workspace's network namespace:
//
//	/proc/<pid>/net/tcp   /proc/<pid>/net/tcp6
//	/proc/<pid>/net/udp   /proc/<pid>/net/udp6
//	/proc/<pid>/net/unix
//
// Reading /proc/<pid>/net/* needs no namespace handling at all, because the pid
// is already inside the namespace; `ip netns exec <ns> ss` is deliberately NOT
// used (it fails on the target host: the workspace namespace is created by
// slirp, and `ip netns exec net:[...]` cannot open it). The cost of a table read
// is measured (~4.6 ms for a direct open), which is why this runs ON DEMAND and
// is deliberately kept OUT of the 3-second metrics stream (ADR 96).
//
// The parser is pure over fixture text (parse.go/ports.go), so it is unit-tested
// without a live /proc; the host reads live behind the Source interface.
package ports

import (
	"net/netip"
	"strconv"
	"strings"
)

// Protocol names reported for a port. They mirror the /proc/net file the entry
// was read from.
const (
	ProtoTCP  = "tcp"
	ProtoTCP6 = "tcp6"
	ProtoUDP  = "udp"
	ProtoUDP6 = "udp6"
)

// Scope describes where a listening socket is bound, which is what decides
// whether it is reachable from outside the workspace at all.
const (
	// ScopeLoopback: bound to 127.0.0.0/8 or ::1 -- reachable only from inside
	// the workspace.
	ScopeLoopback = "loopback"
	// ScopeWildcard: bound to 0.0.0.0 or :: -- reachable on every interface of
	// the workspace's own namespace.
	ScopeWildcard = "wildcard"
	// ScopeSpecific: bound to one specific address.
	ScopeSpecific = "specific"
)

// StateListen is the state reported for a TCP listening socket.
const StateListen = "LISTEN"

// StateUDPNone is the state reported for a UDP socket. UDP has NO listen state,
// so the engine reports that honestly (the empty string) rather than inventing
// one; a UDP socket is either bound or not.
const StateUDPNone = ""

// Socket is one parsed row of a /proc/net/{tcp,tcp6,udp,udp6} table.
type Socket struct {
	Proto      string
	Addr       netip.Addr
	Port       int
	Remote     netip.Addr
	RemotePort int
	StateHex   string
	Inode      uint64
}

// rawUnix is one parsed row of /proc/net/unix.
type rawUnix struct {
	Inode uint64
	Type  string
	State string
	Path  string
}

// ParseTable parses one /proc/net/{tcp,tcp6,udp,udp6} table. A malformed row
// is skipped, never guessed; a header-only file or an empty file yields no rows.
//
// The kernel prints each row as whitespace-separated fields:
//
//	sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
//
// so local_address is field 1, st is field 3 and inode is field 9. The parse is
// independent of any trailing per-protocol columns (udp appends ref/pointer/
// drops), because it only reads the leading fields it needs.
func ParseTable(proto string, data []byte) []Socket {
	var out []Socket
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		addrHex, portHex, ok := splitProcAddr(fields[1])
		if !ok {
			// The header row ("local_address") lands here and is skipped.
			continue
		}
		addr, ok := parseHexAddr(addrHex)
		if !ok {
			continue
		}
		port, ok := parseHexPort(portHex)
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		s := Socket{Proto: proto, Addr: addr, Port: port, StateHex: strings.ToUpper(fields[3]), Inode: inode}
		// The remote address is optional for our purposes; parse it when it is
		// well formed (it is what tells a bound UDP socket from a connected
		// client flow).
		if raddrHex, rportHex, rok := splitProcAddr(fields[2]); rok {
			if raddr, aok := parseHexAddr(raddrHex); aok {
				s.Remote = raddr
			}
			if rport, pok := parseHexPort(rportHex); pok {
				s.RemotePort = rport
			}
		}
		out = append(out, s)
	}
	return out
}

// splitProcAddr splits "ADDR:PORT" at the last colon (an IPv6 address contains
// no colon in its /proc/net spelling, but the split is written to be robust).
func splitProcAddr(s string) (addrHex, portHex string, ok bool) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// parseHexAddr decodes a /proc/net hex address. IPv4 is 8 hex chars and IPv6 is
// 32, and both are stored in the kernel's little-endian word order: for IPv4 the
// bytes are reversed relative to network order ("0100007F" is 127.0.0.1), and
// for IPv6 each 4-byte word is reversed.
func parseHexAddr(addrHex string) (netip.Addr, bool) {
	switch len(addrHex) {
	case 8:
		v, err := strconv.ParseUint(addrHex, 16, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}), true
	case 32:
		var b [16]byte
		for i := 0; i < 4; i++ {
			v, err := strconv.ParseUint(addrHex[i*8:(i+1)*8], 16, 32)
			if err != nil {
				return netip.Addr{}, false
			}
			b[i*4] = byte(v)
			b[i*4+1] = byte(v >> 8)
			b[i*4+2] = byte(v >> 16)
			b[i*4+3] = byte(v >> 24)
		}
		return netip.AddrFrom16(b), true
	default:
		return netip.Addr{}, false
	}
}

// parseHexPort decodes a /proc/net hex port. The port is plain big-endian hex
// ("1F90" is 8080), unlike the address.
func parseHexPort(portHex string) (int, bool) {
	v, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// scopeOf classifies a bind address: loopback, wildcard, or specific. It matters
// because a loopback-only listener is NOT reachable from outside the workspace
// even if the port number is published, while a wildcard binding is.
func scopeOf(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return ScopeLoopback
	case a.IsUnspecified():
		return ScopeWildcard
	default:
		return ScopeSpecific
	}
}

// parseUnixTable parses /proc/net/unix. Rows are:
//
//	Num RefCount Protocol Flags Type St Inode Path
//
// A socket with no path is abstract or unnamed; it is reported with an empty
// path rather than dropped.
func parseUnixTable(data []byte) []rawUnix {
	var out []rawUnix
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		inode, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			// The header row ("Inode") lands here and is skipped.
			continue
		}
		path := ""
		if len(fields) > 7 {
			path = fields[7]
		}
		u := rawUnix{
			Inode: inode,
			Type:  unixType(fields[4]),
			State: unixState(fields[5]),
			Path:  path,
		}
		out = append(out, u)
	}
	return out
}

// unixType maps the /proc/net/unix Type column to a stable label.
func unixType(t string) string {
	switch t {
	case "0001":
		return "stream"
	case "0002":
		return "dgram"
	case "0005":
		return "seqpacket"
	default:
		return "unknown"
	}
}

// unixState maps the /proc/net/unix St column to a stable label.
func unixState(s string) string {
	switch s {
	case "01":
		return "unconnected"
	case "02":
		return "connecting"
	case "03":
		return "connected"
	case "04":
		return "disconnecting"
	default:
		return "unknown"
	}
}
