// Package network implements per-workspace isolated networking and published
// ports for sandboxd.
//
// Every workspace gets its own private network namespace with a point-to-point
// veth link to an engine-owned "hub" namespace. The workspace sees exactly one
// peer (the host end of its own link) and has no default route, so it can
// reach neither the host LAN, nor another workspace, nor any host service that
// binds 0.0.0.0: the forwarder and the optional egress proxy originate inside
// the hub namespace, so the host needs no route to the workspace subnet at all.
//
// The hub itself is reachable from the host over a dedicated host<->hub veth
// link, so the engine can install listeners and dial into a workspace without
// ever exposing the workspace subnet to the host network.
//
// This is a measured design (see docs/DECISIONS.md and docs/VERIFICATION.md):
// `ip netns add` is broken on the target host (it mounts /sys and fails with
// "Operation not permitted"), so namespaces are created by holding a private
// namespace with `unshare -n` and bind-mounting its /proc/<pid>/ns/net into
// /run/netns; the holder then exits and the bind mount keeps the namespace
// alive. iptables and nft are not installed, so no NAT is used anywhere.
package network

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// system abstracts the host commands the network layer runs, so the pure
// bookkeeping can be tested without root while the real implementation shells
// out to `ip`, `nsenter`, `unshare` and `mount`.
type system interface {
	// run executes a command and returns an error if it fails.
	run(name string, args ...string) error
	// output executes a command and returns its stdout.
	output(name string, args ...string) ([]byte, error)
}

// osSystem is the real system.
type osSystem struct{}

func (osSystem) run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (osSystem) output(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// toolAvailable reports whether a binary is on PATH (or an absolute path).
func toolAvailable(name string) bool {
	if strings.ContainsRune(name, '/') {
		fi, err := os.Stat(name)
		return err == nil && !fi.IsDir()
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// createNetns creates a persistent named network namespace at
// <netnsDir>/<name> without `ip netns add`.
//
// Measured on the target host: `ip netns add` fails because it also tries to
// mount a fresh /sys ("mount of /sys failed: Operation not permitted"), but
// holding a private namespace with `unshare -n` and bind-mounting that
// process's /proc/<pid>/ns/net into /run/netns works, and the namespace then
// outlives the holder because the bind mount holds a reference.
func (m *Manager) createNetns(name string) (string, error) {
	if err := os.MkdirAll(m.cfg.NetnsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.NetnsDir, name)
	// Remove a stale mount/file first (best effort; a live namespace with the
	// same name would otherwise collide).
	_ = os.Remove(path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", err
	}

	// Hold a private netns in a short-lived child.
	holder := exec.Command("unshare", "-n", "sleep", "100000")
	if err := holder.Start(); err != nil {
		return "", fmt.Errorf("unshare -n: %w", err)
	}
	pid := holder.Process.Pid
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = holder.Process.Wait()
	}()

	nsFile := fmt.Sprintf("/proc/%d/ns/net", pid)
	// Wait for the namespace to appear, then bind-mount it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(nsFile); err == nil {
			if err := m.sys.run("mount", "--bind", nsFile, path); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("bind-mount namespace %s: timed out", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return path, nil
}

// destroyNetns unmounts and removes a named namespace. The namespace itself
// persists until every process in it has exited (the kernel refcounts it), so
// a workload that is still running keeps it alive; callers stop work first.
func (m *Manager) destroyNetns(name string) {
	path := filepath.Join(m.cfg.NetnsDir, name)
	_ = m.sys.run("umount", path)
	_ = os.Remove(path)
}

// netnsPath returns the path of a named namespace.
func (m *Manager) netnsPath(name string) string {
	return filepath.Join(m.cfg.NetnsDir, name)
}

// inNetns runs a command inside a named namespace via nsenter. Using
// `nsenter --net=<path>` (not `ip netns exec`) avoids the broken /sys mount
// path entirely.
func (m *Manager) inNetns(name string, command string, args ...string) error {
	full := append([]string{"--net=" + m.netnsPath(name), command}, args...)
	return m.sys.run("nsenter", full...)
}
