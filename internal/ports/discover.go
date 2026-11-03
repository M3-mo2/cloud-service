package ports

import (
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strings"
)

// Limits bounds the host-side scan. They are applied by the caller (the
// executor) and reported back through NetTables so the result can say honestly
// whether a bound was hit.
type Limits struct {
	// MaxPids caps how many of the workspace's processes are examined. It is
	// bounded by the registry size, plus this cap.
	MaxPids int
	// MaxFDsPerPID caps how many file descriptors are examined in one process.
	MaxFDsPerPID int
}

// DefaultLimits returns the shipped defaults: at most 64 processes and 512 file
// descriptors each, well above a normal workspace and far below anything that
// could make one request expensive.
func DefaultLimits() Limits { return Limits{MaxPids: 64, MaxFDsPerPID: 512} }

// NetTables is the raw /proc/<pid>/net text read for a pid inside a workspace
// namespace, together with the socket inode -> owning-pid attribution the
// executor resolved over the candidate pids. The text is kept raw so the parser
// (this package) is the single place the format is understood and unit-tested.
type NetTables struct {
	TCP  []byte
	TCP6 []byte
	UDP  []byte
	UDP6 []byte
	Unix []byte
	// Holders maps a socket inode to the host pids (a subset of the candidates)
	// that hold it, from scanning /proc/<pid>/fd. An inode absent from the map
	// has no resolvable owner.
	Holders map[uint64][]int
	// Complete is false when a configured bound was hit, so the caller reports a
	// partial result rather than one that looks total.
	Complete bool
	// Notes explain an incomplete result (which bound was hit).
	Notes []string
	// PidsScanned and FDsScanned report the bounded work actually done.
	PidsScanned int
	FDsScanned  int
}

// ProcessRef is the owner metadata the report carries for an attributed
// endpoint.
type ProcessRef struct {
	PID  int
	Kind string
	Argv []string
}

// Endpoint is one reported listening endpoint. It is JSON-ready: the service
// returns this shape directly.
type Endpoint struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Scope    string `json:"scope"`
	// State is LISTEN for a TCP listener and "" for UDP (UDP has no listen
	// state; the engine says so rather than inventing one).
	State string `json:"state"`
	// PID, Kind and Label name the owning process when it can be resolved.
	// Attributed is false when the socket's inode is held by no live process in
	// the registry: the entry is still reported, never dropped and never guessed.
	Attributed bool     `json:"attributed"`
	PID        int      `json:"pid,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Label      string   `json:"label,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	// Owners lists every candidate pid holding the socket, when there is more
	// than one; the primary fields above name the first by pid. It is empty for
	// an unattributed endpoint.
	Owners []int `json:"owners,omitempty"`
	// Inode is the kernel socket inode, carried so a client can correlate an
	// endpoint with its own diagnostics. It is not a stable across time.
	Inode uint64 `json:"inode,omitempty"`
	// Proto6 marks the v6 variants of tcp/udp without re-encoding Protocol.
	Proto6 bool `json:"ipv6,omitempty"`
}

// UnixSocket is one /proc/net/unix row. It is reported in a SEPARATE section,
// never mixed into the port list, because a unix socket is not a reachable port.
type UnixSocket struct {
	Path       string `json:"path,omitempty"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Attributed bool   `json:"attributed"`
	PID        int    `json:"pid,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Label      string `json:"label,omitempty"`
	Inode      uint64 `json:"inode,omitempty"`
}

// Result is the parsed, sorted, attributed answer for one workspace.
type Result struct {
	Ports     []Endpoint
	Count     int
	Unix      []UnixSocket
	UnixCount int
	// Complete is false when a read boundary was hit, so a caller knows the list
	// is a partial view rather than a total one.
	Complete bool
	Notes    []string
	// PidsScanned and FDsScanned are the bounded work the read performed.
	PidsScanned int
	FDsScanned  int
}

// maxLabelLen bounds the human label so a pathological argv cannot bloat a
// response.
const maxLabelLen = 120

// Build turns the raw /proc/net tables and the inode->process attribution into a
// deterministic, sorted result. It is pure: no host access, so it is fully
// unit-testable from fixture text.
//
// procs maps a host pid to the process metadata from the workspace registry, so
// an attributed endpoint can name its owner. A socket whose inode is held by no
// pid in procs (or that the executor could not resolve) is reported
// unattributed, never dropped.
func Build(t NetTables, procs map[int]ProcessRef) Result {
	var sockets []Socket
	sockets = append(sockets, ParseTable(ProtoTCP, t.TCP)...)
	sockets = append(sockets, ParseTable(ProtoTCP6, t.TCP6)...)
	sockets = append(sockets, ParseTable(ProtoUDP, t.UDP)...)
	sockets = append(sockets, ParseTable(ProtoUDP6, t.UDP6)...)

	endpoints := make([]Endpoint, 0, len(sockets))
	for _, s := range sockets {
		if !reportable(s) {
			continue
		}
		ep := Endpoint{
			Port:     s.Port,
			Protocol: s.Proto,
			Address:  s.Addr.String(),
			Scope:    scopeOf(s.Addr),
			State:    stateOf(s),
			Inode:    s.Inode,
			Proto6:   s.Proto == ProtoTCP6 || s.Proto == ProtoUDP6,
		}
		// Attribution: every candidate pid that holds this inode, sorted by pid.
		var owners []int
		for _, pid := range t.Holders[s.Inode] {
			if _, known := procs[pid]; known {
				owners = append(owners, pid)
			}
		}
		sort.Ints(owners)
		owners = dedupeInts(owners)
		if len(owners) > 0 {
			ep.Attributed = true
			ep.Owners = owners
			primary := owners[0]
			ep.PID = primary
			if ref, ok := procs[primary]; ok {
				ep.Kind = ref.Kind
				ep.Argv = ref.Argv
				ep.Label = LabelFor(ref.Argv, maxLabelLen)
			}
		}
		endpoints = append(endpoints, ep)
	}

	// Sort deterministically: by port, then protocol. protocol "tcp" < "tcp6" <
	// "udp" < "udp6" lexically, which is a stable, documented order.
	sort.SliceStable(endpoints, func(i, j int) bool {
		if endpoints[i].Port != endpoints[j].Port {
			return endpoints[i].Port < endpoints[j].Port
		}
		if endpoints[i].Protocol != endpoints[j].Protocol {
			return endpoints[i].Protocol < endpoints[j].Protocol
		}
		return endpoints[i].Address < endpoints[j].Address
	})

	// Unix sockets are a SEPARATE section.
	var unix []UnixSocket
	for _, u := range parseUnixTable(t.Unix) {
		us := UnixSocket{Path: u.Path, Type: u.Type, State: u.State, Inode: u.Inode}
		var owners []int
		for _, pid := range t.Holders[u.Inode] {
			if _, known := procs[pid]; known {
				owners = append(owners, pid)
			}
		}
		sort.Ints(owners)
		owners = dedupeInts(owners)
		if len(owners) > 0 {
			us.Attributed = true
			us.PID = owners[0]
			if ref, ok := procs[us.PID]; ok {
				us.Kind = ref.Kind
				us.Label = LabelFor(ref.Argv, maxLabelLen)
			}
		}
		unix = append(unix, us)
	}
	sort.SliceStable(unix, func(i, j int) bool {
		if unix[i].Path != unix[j].Path {
			return unix[i].Path < unix[j].Path
		}
		return unix[i].Inode < unix[j].Inode
	})

	return Result{
		Ports:       endpoints,
		Count:       len(endpoints),
		Unix:        unix,
		UnixCount:   len(unix),
		Complete:    t.Complete,
		Notes:       append([]string(nil), t.Notes...),
		PidsScanned: t.PidsScanned,
		FDsScanned:  t.FDsScanned,
	}
}

// reportable decides which parsed rows are surfaced. Only sockets that can carry
// a service are reported:
//   - TCP sockets in the LISTEN state (0A);
//   - UDP sockets that are BOUND, i.e. whose remote address is unspecified and
//     whose local port is non-zero (a UDP socket is either bound or connected;
//     there is no LISTEN).
//
// A TCP socket in any other state (ESTABLISHED, TIME_WAIT, ...) is not a
// listener and is not a port a user "has open".
func reportable(s Socket) bool {
	switch s.Proto {
	case ProtoTCP, ProtoTCP6:
		return s.StateHex == "0A"
	case ProtoUDP, ProtoUDP6:
		return s.Port != 0 && s.RemotePort == 0
	default:
		return false
	}
}

// stateOf reports the state of a reportable socket. TCP is always LISTEN here;
// UDP has no listen state, so the engine reports "" honestly.
func stateOf(s Socket) string {
	switch s.Proto {
	case ProtoTCP, ProtoTCP6:
		return StateListen
	default:
		return StateUDPNone
	}
}

// LabelFor builds a short human label from argv: the basename of argv[0] plus
// the remaining arguments, whitespace-joined and truncated to max characters. It
// never returns an empty string for a non-empty argv.
func LabelFor(argv []string, max int) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(argv))
	for i, a := range argv {
		if i == 0 {
			parts = append(parts, path.Base(a))
			continue
		}
		parts = append(parts, a)
	}
	label := strings.Join(parts, " ")
	if max > 0 && len(label) > max {
		label = label[:max]
	}
	return label
}

// dedupeInts removes adjacent duplicates from a sorted slice.
func dedupeInts(in []int) []int {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// FormatScope is a small helper for docs/tests.
func FormatScope(a string) string {
	addr, err := netip.ParseAddr(a)
	if err != nil {
		return ScopeSpecific
	}
	return scopeOf(addr)
}

// Describe is a debugging helper used by tests and logs.
func (e Endpoint) Describe() string {
	return fmt.Sprintf("%s/%d %s %s", e.Protocol, e.Port, e.Scope, e.State)
}
