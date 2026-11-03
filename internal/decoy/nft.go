package decoy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// NftBinary is the path to the nft tool. It is configurable for a host that
// installs it elsewhere; the default is the Debian/Ubuntu location.
var NftBinary = "/usr/sbin/nft"

// RulesSpec describes the in-namespace egress rules that force the workspace's
// DNS through the stub and cover the well-known DoH endpoints, so the decoy
// cannot be bypassed. It is pure data so the rendered program is deterministic
// and unit-testable.
type RulesSpec struct {
	// Table is the nft table name (owned by the engine, deleted on teardown).
	Table string `json:"table"`
	// ProfileIP is the decoy address; its 127.0.0.1 loopback alias is added for
	// SNI-less clients and its 80/443 are redirected to the stubs.
	ProfileIP string `json:"profile_ip"`
	// DNSPort / HTTPPort / HTTPSPort are the stub listener ports.
	DNSPort   int `json:"dns_port"`
	HTTPPort  int `json:"http_port"`
	HTTPSPort int `json:"https_port"`
	// Mark is the SO_MARK value the resolver's own upstream queries carry.
	Mark uint32 `json:"mark"`
	// DoHIPs are resolver addresses whose DoH/DoT ports are covered.
	DoHIPs []string `json:"doh_ips"`
}

// DefaultRulesSpec builds the rules for a config and workspace.
func DefaultRulesSpec(cfg Config, workspaceID string) RulesSpec {
	p := cfg.Derive(workspaceID)
	return RulesSpec{
		Table:     "sbxd_decoy",
		ProfileIP: p.IP.String(),
		DNSPort:   cfg.ResolverPort,
		HTTPPort:  cfg.HTTPPort,
		HTTPSPort: cfg.HTTPSPort,
		Mark:      MarkValue,
		DoHIPs:    wellKnownDoHIPList(),
	}
}

// NftProgram renders the nft program for add (delete=false) or delete
// (delete=true). Only the named table is touched, so teardown never removes a
// rule this engine did not create.
//
// The design, all inside the workspace netns and none of it on the host:
//   - a nat/output chain whose FIRST rule accepts the resolver's marked
//     upstream sockets, so its forwarded queries are not re-captured;
//   - port 53 (UDP+TCP) is redirected to the stub resolver on 127.0.0.1;
//   - the well-known DoH/DoT resolver addresses have 443/853 redirected to the
//     stub TLS responder, so a client cannot use DoH/DoT to bypass the stub;
//   - the decoy profile address has 80/443 redirected to the stubs (for a
//     client that somehow got the decoy address through a channel we did not
//     model), and a redirect to the stub ports is placed before the generic
//     echo-service 443 redirect so DNS-modelable and DNSless clients both land
//     on the same responder.
func NftProgram(spec RulesSpec, del bool) string {
	var b strings.Builder
	if del {
		fmt.Fprintf(&b, "delete table inet %s\n", spec.Table)
		return b.String()
	}
	t := spec.Table
	fmt.Fprintf(&b, "add table inet %s\n", t)
	fmt.Fprintf(&b, "add chain inet %s out { type nat hook output priority dstnat; policy accept; }\n", t)
	// Escape hatch for the stub resolver's own marked upstream queries.
	fmt.Fprintf(&b, "add rule inet %s out meta mark 0x%x accept\n", t, spec.Mark)
	// DNS: every port-53 query goes to the stub, whichever resolver it names.
	fmt.Fprintf(&b, "add rule inet %s out udp dport 53 dnat ip to 127.0.0.1:%d\n", t, spec.DNSPort)
	fmt.Fprintf(&b, "add rule inet %s out tcp dport 53 dnat ip to 127.0.0.1:%d\n", t, spec.DNSPort)
	// DoH/DoT endpoints.
	for _, ip := range spec.DoHIPs {
		fmt.Fprintf(&b, "add rule inet %s out ip daddr %s tcp dport { 443, 853 } dnat ip to 127.0.0.1:%d\n", t, ip, spec.HTTPSPort)
	}
	// The decoy address itself.
	if spec.ProfileIP != "" && spec.ProfileIP != "0.0.0.0" {
		fmt.Fprintf(&b, "add rule inet %s out ip daddr %s tcp dport 443 dnat ip to 127.0.0.1:%d\n", t, spec.ProfileIP, spec.HTTPSPort)
		fmt.Fprintf(&b, "add rule inet %s out ip daddr %s tcp dport 80 dnat ip to 127.0.0.1:%d\n", t, spec.ProfileIP, spec.HTTPPort)
	}
	return b.String()
}

// ApplyRules runs nft with the rendered program inside the CURRENT network
// namespace (the caller must already be in the workspace netns, e.g. via
// `nsenter --net=`). It uses `nft -f -` so the whole program is one atomic
// transaction. delete removes only the engine-owned table.
func ApplyRules(spec RulesSpec, del bool) error {
	prog := NftProgram(spec, del)
	cmd := exec.Command(NftBinary, "-f", "-")
	cmd.Stdin = strings.NewReader(prog)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s: %w: %s", map[bool]string{true: "delete", false: "add"}[del], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RulesSpecFromFile loads a rules spec from a JSON file (used by the helper).
func RulesSpecFromFile(path string) (RulesSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RulesSpec{}, err
	}
	var s RulesSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		return RulesSpec{}, err
	}
	return s, nil
}
