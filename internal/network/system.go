// Package network implements per-workspace isolated networking (egress) and
// quick public tunnels for sandboxd.
//
// Egress model. Every allow_net workspace gets its own network namespace that
// is connected to the public internet ONLY through chained slirp4netns
// instances:
//
//	workspace ns --slirp(UNIX socket)--> relay ns --slirp(UNIX socket)--> host
//
// An engine-owned "relay" namespace is created once and connected to the host
// by a single slirp4netns instance. Each workspace namespace is then connected
// to the relay by a second slirp4netns instance run INSIDE the relay namespace.
// Because slirp4netns speaks to its parent over a UNIX socket and only wires up
// a tap interface inside the child namespace, this creates ZERO host
// interfaces, ZERO host routes and ZERO veth pairs. The relay carries a
// blackhole route for every private range, so the workspace can reach the
// public internet (and a public resolver) but never the host LAN, the host's
// own services, or another workspace.
//
// This is the measured-safe design (see docs/DECISIONS.md and
// docs/VERIFICATION.md): the host's own interfaces and routes are byte-identical
// before and after, and no veth/tap interface exists on the host. The
// abandoned alternative — host veth pairs, addresses and routes (or `ip netns
// add`) — took the target host off the network three times and is deleted from
// this tree.
//
// `ip netns add` is unusable on the target host (it mounts /sys and fails with
// "Operation not permitted"), so namespaces are created by holding a private
// namespace with `unshare -n` and bind-mounting its /proc/<pid>/ns/net into
// /run/netns; the holder then exits and the bind mount keeps the namespace
// alive. iptables and nft are not installed, so no NAT is used anywhere: the
// controlled egress path IS slirp4netns.
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
	run(name string, args ...string) error
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
//
// The child writes nothing; the pid used is the direct child of `unshare -n`,
// which exec's the sleep in the freshly unshared namespace. (Callers must NOT
// wrap this in sudo: sudo would return sudo's own pid, capturing the host
// namespace — a trap this package's tests guard against.)
func (m *Manager) createNetns(name string) (string, error) {
	if err := os.MkdirAll(m.cfg.NetnsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.NetnsDir, name)
	// Remove a stale mount/file first (best effort).
	_ = m.sys.run("umount", path)
	_ = os.Remove(path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", err
	}

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
// path entirely. It is used for read-only inspection (addr/route probes).
func (m *Manager) inNetns(name string, command string, args ...string) error {
	full := append([]string{"--net=" + m.netnsPath(name), command}, args...)
	return m.sys.run("nsenter", full...)
}

// inNetnsOutput runs a command inside a named namespace and returns stdout.
func (m *Manager) inNetnsOutput(name string, command string, args ...string) ([]byte, error) {
	full := append([]string{"--net=" + m.netnsPath(name), command}, args...)
	return m.sys.output("nsenter", full...)
}

// runInNetnsMount runs a command inside a named namespace AND a private mount
// namespace. The private mount namespace is what lets the engine bind a public
// resolver over /etc/resolv.conf for slirp and cloudflared WITHOUT touching the
// host's /etc/resolv.conf: `unshare -m` isolates the mount, then `nsenter
// --net` enters the target network namespace. This is the measured-safe
// replacement for the earlier approach, which leaked a resolver bind onto the
// host because `nsenter --net` alone shares the host mount namespace.
func (m *Manager) runInNetnsMount(netns, resolvConf string, command string, args ...string) *exec.Cmd {
	// The inner shell binds the resolver (when set), then execs nsenter so no
	// wrapper process survives; nsenter in turn execs the target command.
	script := ""
	if resolvConf != "" {
		script = "mount --bind " + shellQuote(resolvConf) + " /etc/resolv.conf && "
	}
	script += "exec nsenter --net=" + shellQuote(netns) + " " + shellJoin(append([]string{command}, args...))
	return exec.Command("unshare", "-m", "sh", "-c", script)
}

// shellQuote single-quotes a string for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin quotes each argument for /bin/sh.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}
