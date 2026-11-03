package decoy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// ServeSubcommand is the hidden argv[0] subcommand the manager uses to start a
// workspace's decoy stub inside its namespace. It is deliberately not a normal
// CLI verb: an operator never invokes it directly.
const ServeSubcommand = "__decoy-serve"

// Spec is the full, JSON-serializable description one decoy helper needs. It is
// written to a file by the manager (which owns the CA private key) and read by
// the helper re-exec of the same binary inside the workspace netns, so no
// secret crosses the environment of the sandboxed workload.
type Spec struct {
	WorkspaceID string   `json:"workspace_id"`
	Config      Config   `json:"config"`
	CACertPEM   []byte   `json:"ca_cert_pem"`
	CAKeyPEM    []byte   `json:"ca_key_pem"`
	Upstream    []string `json:"upstream"`
	// ReadyPath is created once both stub listeners are bound, so the manager
	// installs the nft redirect only after there is something to receive it.
	ReadyPath string `json:"ready_path"`
	// DNSPort / HTTPPort / HTTPSPort are the ports the helper binds on
	// 127.0.0.1; the in-netns nft rules redirect 53/80/443/853 to them.
	DNSPort   int `json:"dns_port"`
	HTTPPort  int `json:"http_port"`
	HTTPSPort int `json:"https_port"`
}

// ServeFromSpecFile reads the spec and serves the DNS, HTTP and TLS stubs until
// the process is signalled. It is the entry point of the hidden
// `sandboxd __decoy-serve <spec>` subcommand, run via `nsenter --net=<ws>` so
// the stubs live in the workspace's own namespace.
func ServeFromSpecFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	return Serve(spec)
}

// Serve runs the stubs described by spec until SIGTERM/SIGINT or context end.
// The listeners are bound SYNCHRONOUSLY before the ready file is written, so a
// workspace started right after never sees a port with no listener.
func Serve(spec Spec) error {
	ca, err := LoadCA(spec.CACertPEM, spec.CAKeyPEM)
	if err != nil {
		return err
	}
	cfg := spec.Config
	if spec.DNSPort > 0 {
		cfg.ResolverPort = spec.DNSPort
	}
	if spec.HTTPPort > 0 {
		cfg.HTTPPort = spec.HTTPPort
	}
	if spec.HTTPSPort > 0 {
		cfg.HTTPSPort = spec.HTTPSPort
	}
	res := NewResolver(cfg, spec.WorkspaceID, spec.Upstream)
	resp := NewResponder(cfg, ca, spec.WorkspaceID, res)

	// Bind everything up front.
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.ResolverPort})
	if err != nil {
		return fmt.Errorf("decoy dns udp bind: %w", err)
	}
	defer udpConn.Close()
	tcpConn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.ResolverPort})
	if err != nil {
		return fmt.Errorf("decoy dns tcp bind: %w", err)
	}
	defer tcpConn.Close()
	httpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.HTTPPort)))
	if err != nil {
		return fmt.Errorf("decoy http bind: %w", err)
	}
	defer httpLn.Close()
	tlsLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.HTTPSPort)))
	if err != nil {
		return fmt.Errorf("decoy https bind: %w", err)
	}
	defer tlsLn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		cancel()
	}()

	// Signal readiness now that every listener is bound.
	if spec.ReadyPath != "" {
		_ = os.WriteFile(spec.ReadyPath, []byte("ready\n"), 0o644)
	}

	errCh := make(chan error, 4)
	go func() { errCh <- res.serveBound(ctx, udpConn, tcpConn) }()
	go func() { errCh <- resp.serveBound(ctx, httpLn, tlsLn) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		cancel()
		if err != nil && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("decoy stub exited: %w", err)
		}
		return nil
	}
}

// SpecFilePath returns the path of the per-workspace spec file under dir.
func SpecFilePath(dir, workspaceID string) string {
	return dir + "/decoy-spec-" + shortHash(workspaceID) + ".json"
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// waitReady polls for the helper's ready file.
func waitReady(path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
