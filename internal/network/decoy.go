package network

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/decoy"
)

// This file wires the egress-decoy layer into the per-workspace network
// lifecycle. The decoy layer is a decoy, not a security boundary (see
// docs/EGRESS-DECOY.md and ADR 84): an in-namespace stub resolver and stub
// HTTP/TLS responder answer a workspace's attempts to learn its own public IP
// with a plausible, consistent invented identity, while ordinary internet
// access (pypi, npm, github) is untouched.
//
// Everything here stays inside the workspace's OWN network namespace:
//
//   - the stubs are started through `nsenter --net=<ws>` so they bind only in
//     the workspace namespace;
//   - the egress rules are installed with `nsenter --net=<ws> nft -f -` and are
//     confined to an engine-owned table (deleted on teardown);
//   - the workspace's /etc/resolv.conf (a bind-mounted file) is pointed at the
//     stub resolver at 127.0.0.1.
//
// No host interface, route, address or nft rule is ever touched.

// decoyManager owns the decoy CA and the per-workspace helper processes.
type decoyManager struct {
	cfg config.DecoyConfig

	ca     *decoy.CA
	caOnce sync.Once
	caErr  error
	caDir  string

	exePath  string
	resolver []string

	mu      sync.Mutex
	helpers map[string]*os.Process // wsID -> helper process
	specs   map[string]string      // wsID -> spec file path
}

// newDecoyManager builds the manager; the CA is generated lazily on first use.
func newDecoyManager(cfg config.DecoyConfig, resolver []string, caDir string) *decoyManager {
	exe, _ := os.Executable()
	return &decoyManager{
		cfg:      cfg,
		caDir:    caDir,
		exePath:  exe,
		resolver: resolver,
		helpers:  map[string]*os.Process{},
		specs:    map[string]string{},
	}
}

// Enabled reports whether the decoy layer is configured on.
func (d *decoyManager) Enabled() bool { return d != nil && d.cfg.Enabled }

// CA materializes (or reloads) the decoy certificate authority. It is stable
// across restarts: the materialized PEM files are reused, so a workspace's
// trust in the CA survives a service restart. It returns nil when the layer is
// disabled.
func (d *decoyManager) CA() (*decoy.CA, error) {
	if !d.Enabled() {
		return nil, nil
	}
	d.caOnce.Do(func() {
		if err := os.MkdirAll(d.caDir, 0o755); err != nil {
			d.caErr = err
			return
		}
		certPath := filepath.Join(d.caDir, "decoy-ca.pem")
		keyPath := filepath.Join(d.caDir, "decoy-ca.key")
		if cert, kerr := os.ReadFile(certPath); kerr == nil {
			if key, kerr2 := os.ReadFile(keyPath); kerr2 == nil {
				if ca, lerr := decoy.LoadCA(cert, key); lerr == nil {
					d.ca = ca
					return
				}
			}
		}
		ca, gerr := decoy.NewCA(d.cfg.Org)
		if gerr != nil {
			d.caErr = gerr
			return
		}
		if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
			d.caErr = err
			return
		}
		// The CA private key is engine material; it never reaches a workspace
		// (only the certificate does, via the trust store), so 0600 is correct.
		if err := os.WriteFile(keyPath, ca.KeyPEM, 0o600); err != nil {
			d.caErr = err
			return
		}
		d.ca = ca
	})
	return d.ca, d.caErr
}

// decoyConfig converts the service config to the decoy package config.
func (d *decoyManager) decoyConfig() (decoy.Config, error) {
	prefix, err := parsePrefix(d.cfg.AddressRange)
	if err != nil {
		return decoy.Config{}, fmt.Errorf("decoy address_range %q: %w", d.cfg.AddressRange, err)
	}
	return decoy.Config{
		Enabled:       true,
		AddressRange:  prefix,
		Org:           d.cfg.Org,
		ASN:           d.cfg.ASN,
		City:          d.cfg.City,
		Country:       d.cfg.Country,
		Region:        d.cfg.Region,
		RDNS:          d.cfg.RDNS,
		ResolverPort:  d.cfg.ResolverPort,
		HTTPPort:      d.cfg.HTTPPort,
		HTTPSPort:     d.cfg.HTTPSPort,
		ExtraHosts:    d.cfg.ExtraHosts,
		ExtraPatterns: d.cfg.ExtraPatterns,
	}, nil
}

// StartFor starts (or restarts) the decoy stubs for a workspace inside its
// namespace and installs the in-namespace egress rules. It is idempotent per
// workspace: a second call with a live helper is a no-op. Any error is returned
// so the caller can refuse the run rather than silently serve the truth.
func (d *decoyManager) StartFor(wsID, netnsPath string) error {
	if !d.Enabled() {
		return nil
	}
	ca, err := d.CA()
	if err != nil {
		return fmt.Errorf("decoy CA: %w", err)
	}
	dcfg, err := d.decoyConfig()
	if err != nil {
		return err
	}

	d.mu.Lock()
	if p := d.helpers[wsID]; p != nil && decoyProcessAlive(p.Pid) {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	specPath := decoy.SpecFilePath(d.caDir, wsID)
	readyPath := specPath + ".ready"
	_ = os.Remove(readyPath)
	if specPath, err = d.writeSpec(wsID, dcfg, ca, readyPath); err != nil {
		return err
	}

	// Start the helper INSIDE the workspace namespace. --no-fork makes the
	// tracked pid the helper itself. The helper is not a sandbox workload: it
	// runs before/independently of nsjail, so it is not visible in the jail's
	// process view (the jail has its own pid namespace).
	cmd := exec.Command("nsenter", "--net="+netnsPath, "--no-fork", d.exePath, decoy.ServeSubcommand, specPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start decoy helper: %w", err)
	}
	proc := cmd.Process
	go func() { _ = cmd.Wait() }()

	// Wait for the helper's stubs to bind before installing the redirect, so a
	// run started immediately after cannot see a port with no listener.
	if err := waitForFile(readyPath, 5*time.Second); err != nil {
		_ = proc.Kill()
		return fmt.Errorf("decoy helper did not become ready: %w", err)
	}

	// Install the in-namespace rules.
	spec := decoy.DefaultRulesSpec(dcfg, wsID)
	if err := d.applyRules(netnsPath, spec, false); err != nil {
		_ = killGroup(proc.Pid)
		return fmt.Errorf("install decoy egress rules: %w", err)
	}

	d.mu.Lock()
	d.helpers[wsID] = proc
	d.specs[wsID] = specPath
	d.mu.Unlock()
	return nil
}

// StopFor stops a workspace's helper and removes its rules. It is best-effort
// and stops only the process this manager started.
func (d *decoyManager) StopFor(wsID, netnsPath string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	p := d.helpers[wsID]
	spec := d.specs[wsID]
	delete(d.helpers, wsID)
	delete(d.specs, wsID)
	d.mu.Unlock()
	if p != nil {
		_ = killGroup(p.Pid)
	}
	if netnsPath != "" {
		if dcfg, err := d.decoyConfig(); err == nil {
			s := decoy.DefaultRulesSpec(dcfg, wsID)
			_ = d.applyRules(netnsPath, s, true)
		}
	}
	if spec != "" {
		_ = os.Remove(spec)
		_ = os.Remove(spec + ".ready")
	}
}

// applyRules runs nft inside the workspace namespace (never the host).
func (d *decoyManager) applyRules(netnsPath string, spec decoy.RulesSpec, del bool) error {
	prog := decoy.NftProgram(spec, del)
	args := []string{"--net=" + netnsPath}
	if _, err := os.Stat(decoy.NftBinary); err == nil {
		args = append(args, decoy.NftBinary, "-f", "-")
	} else {
		return fmt.Errorf("nft not found at %s", decoy.NftBinary)
	}
	cmd := exec.Command("nsenter", args...)
	cmd.Stdin = strings.NewReader(prog)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft in %s: %w: %s", netnsPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeSpec writes the helper spec file (holding the CA key) under the engine
// tmp dir (never inside a workspace). The file is 0600.
func (d *decoyManager) writeSpec(wsID string, dcfg decoy.Config, ca *decoy.CA, readyPath string) (string, error) {
	spec := decoy.Spec{
		WorkspaceID: wsID,
		Config:      dcfg,
		CACertPEM:   ca.CertPEM,
		CAKeyPEM:    ca.KeyPEM,
		Upstream:    d.resolver,
		ReadyPath:   readyPath,
		DNSPort:     dcfg.ResolverPort,
		HTTPPort:    dcfg.HTTPPort,
		HTTPSPort:   dcfg.HTTPSPort,
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	path := decoy.SpecFilePath(d.caDir, wsID)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ResolverAddress returns the address an allow_net workspace's resolv.conf
// should name when the decoy layer is on (the stub at 127.0.0.1).
func (d *decoyManager) ResolverAddress() string {
	if d.Enabled() {
		return "127.0.0.1"
	}
	return ""
}

// ReapOrphans kills every decoy helper this engine started (matched by argv),
// so nothing survives a hard daemon death. It walks /proc and signals the exact
// pid; it never uses pkill/killall.
func (d *decoyManager) ReapOrphans() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	for wsID, p := range d.helpers {
		_ = killGroup(p.Pid)
		delete(d.helpers, wsID)
	}
	d.mu.Unlock()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		// The helper is the sandboxd binary itself; identify it by argv.
		if name != filepath.Base(d.exePath) && name != "sandboxd" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if !argsContain(args, decoy.ServeSubcommand) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		reaped++
	}
	return reaped
}

// decoyProcessAlive reports whether a pid is still alive (signal 0).
func decoyProcessAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func argsContain(args []string, needle string) bool {
	for _, a := range args {
		if a == needle {
			return true
		}
	}
	return false
}

// killGroup sends SIGTERM to a helper's process group, then SIGKILL bounded.
func killGroup(pid int) bool {
	if pid <= 0 {
		return false
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !decoyProcessAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

func waitForFile(path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() >= 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", filepath.Base(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func parsePrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return netip.Prefix{}, err
	}
	return p, nil
}
