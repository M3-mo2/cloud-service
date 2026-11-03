package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sandboxd/internal/model"
)

// Per-workspace cgroup ownership.
//
// Historically the workspace's RAM ceiling was PER RUN: nsjail created one
// cgroup per launch at /sys/fs/cgroup/NSJAIL.<pid> directly under the root, so
// a workspace with N concurrent runs could consume N x memory_mb against an
// advertised memory_mb. Measured: one workspace at memory_mb 128 with three
// runs produced three cgroups each with memory.max = 134217728 (384 MB against
// an advertised 128 MB).
//
// This file implements a per-WORKSPACE parent cgroup: ONE cgroup per workspace,
// owned by the engine, whose limits are the workspace's advertised limits. nsjail
// is pointed at it via `cgroupv2_mount`, so every run's cgroup is created INSIDE
// it and the parent enforces the SUM. Moving an existing cgroup into a parent is
// EPERM (measured: `mv` -> "Operation not permitted", os.rename errno=1), so the
// parent must exist BEFORE nsjail launches and nsjail must be told to use it.
//
// The parent name is derived from the workspace id (a ULID, so it is stable and
// non-guessable). Workspace parents are kept out of the way of the per-run
// NSJAIL.* directories by their own prefix.

// WorkspaceCgroupPrefix is the directory-name prefix of a workspace's parent
// cgroup under CgroupMount. The remainder is the workspace id (a ULID).
const WorkspaceCgroupPrefix = "ws-"

// workspaceCgroupControllers is the exact controller set delegated into a
// workspace parent's cgroup.subtree_control. A parent with delegation but no
// enabled child controllers silently enforces nothing, so the set is written
// explicitly and its failure is a loud error.
const workspaceCgroupControllers = "+memory +cpu +pids"

// WorkspaceCgroupName returns the stable, non-guessable parent-cgroup name for a
// workspace. The workspace id is a ULID, so the name is not guessable and does
// not change across restarts.
func WorkspaceCgroupName(wsID string) string { return WorkspaceCgroupPrefix + wsID }

// WorkspaceCgroupPath returns the absolute path of a workspace's parent cgroup.
func WorkspaceCgroupPath(wsID string) string {
	return workspaceCgroupPathAt(CgroupMount, wsID)
}

// workspaceCgroupPathAt returns the parent-cgroup path for a workspace under a
// given cgroup mount. It is the testable core of WorkspaceCgroupPath.
func workspaceCgroupPathAt(base, wsID string) string {
	return filepath.Join(base, WorkspaceCgroupName(wsID))
}

// EnsureWorkspaceCgroup creates (idempotently) a workspace's parent cgroup,
// delegates +memory +cpu +pids into cgroup.subtree_control, and writes the
// workspace's enforced limits onto it:
//
//   - memory.max     = memory_mb bytes (the advertised physical ceiling)
//   - memory.swap.max = 0 (no swap is given to a workspace, so the ceiling is a
//     real physical ceiling rather than a soft one the kernel can swap around)
//   - memory.oom.group = 1 (an OOM at the ceiling kills the whole workspace's
//     process group, not one arbitrary process inside it)
//   - pids.max       = max_runs * 64 (the workspace-wide process budget)
//   - cpu.max        = cpu_cores as a quota per 1000000us period
//
// It is idempotent and cheap, and is called before every run/exec/session so a
// pre-existing workspace (created before this mechanism existed) and a PATCH'd
// limit both converge. A delegation or limit write failure is returned, never
// swallowed: a parent that exists but enforces nothing is exactly the silent
// failure this mechanism exists to prevent.
func EnsureWorkspaceCgroup(ws *model.Workspace) error {
	return ensureWorkspaceCgroupAt(CgroupMount, ws)
}

// ensureWorkspaceCgroupAt is the testable core of EnsureWorkspaceCgroup: it
// provisions the parent under a given cgroup mount. It is exported to tests via
// a package-level call and takes the base so a unit test can use a temp dir.
func ensureWorkspaceCgroupAt(base string, ws *model.Workspace) error {
	if ws == nil || ws.ID == "" {
		return errors.New("workspace is required for cgroup provisioning")
	}
	path := workspaceCgroupPathAt(base, ws.ID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create workspace cgroup %s: %w", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte(workspaceCgroupControllers), 0o644); err != nil {
		return fmt.Errorf("delegate controllers into %s: %w", path, err)
	}

	memMax := int64(ws.Limits.MemoryMB) * 1024 * 1024
	if memMax <= 0 {
		memMax = 1
	}
	if err := writeCgroupInt(path, "memory.max", memMax); err != nil {
		return err
	}
	// No swap: the workspace ceiling must be a real physical ceiling. Without
	// this the kernel can swap a workload past memory.max, and an OOM may never
	// fire (measured: a 200 MB allocation in a 128 MB cgroup with host swap
	// available succeeded and never touched oom_kill).
	if err := writeCgroupRaw(path, "memory.swap.max", "0"); err != nil {
		return err
	}
	// An OOM at the ceiling kills the whole workspace as one group rather than a
	// random process inside it, so the outcome is a whole-run death the engine
	// can attribute, not a silently missing child.
	if err := writeCgroupRaw(path, "memory.oom.group", "1"); err != nil {
		return err
	}

	pids := ws.Limits.MaxRuns * 64
	if pids <= 0 {
		pids = 64
	}
	if err := writeCgroupInt(path, "pids.max", int64(pids)); err != nil {
		return err
	}

	if ws.Limits.CPUCores > 0 {
		milli := int(ws.Limits.CPUCores * 1000)
		if milli <= 0 {
			milli = 1
		}
		// cgroup v2 cpu.max = "<quota> <period>"; the quota is the microseconds
		// of CPU per period the whole workspace may use.
		if err := writeCgroupRaw(path, "cpu.max", fmt.Sprintf("%d 1000000", milli*1000)); err != nil {
			return err
		}
	}
	return nil
}

// writeCgroupInt writes an integer file in a cgroup directory.
func writeCgroupInt(dir, name string, v int64) error {
	return writeCgroupRaw(dir, name, strconv.FormatInt(v, 10))
}

// writeCgroupRaw writes a raw value to a cgroup interface file.
func writeCgroupRaw(dir, name, v string) error {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
		return fmt.Errorf("write %s=%s: %w", name, v, err)
	}
	return nil
}

// RemoveWorkspaceCgroup kills every process in a workspace's parent cgroup
// (including its per-run leaf cgroups) and removes it. It is called on workspace
// delete, AFTER the service has stopped every run/session, so the parent is
// normally already empty; a detached process that outlived the stop is killed
// here so the removal is not blocked. A cgroup that still has processes is never
// rmdir'd (the kernel refuses it anyway), so the members are killed first.
func RemoveWorkspaceCgroup(wsID string) {
	path := WorkspaceCgroupPath(wsID)
	if _, err := os.Stat(path); err != nil {
		return
	}
	// cgroup.kill signals every process in the cgroup AND its descendants
	// (including a process that escaped its process group with setsid()).
	_ = os.WriteFile(filepath.Join(path, cgroupKillFile), []byte("1"), 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cgroupTreeEmpty(path) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	removeCgroupTree(path)
}

// listWorkspaceCgroups returns every workspace parent cgroup under the mount.
func listWorkspaceCgroups() []string {
	entries, err := os.ReadDir(CgroupMount)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), WorkspaceCgroupPrefix) {
			out = append(out, filepath.Join(CgroupMount, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// cgroupTreeEmpty reports whether a cgroup and all of its descendants have no
// member processes.
func cgroupTreeEmpty(path string) bool {
	if len(cgroupMembers(path)) > 0 {
		return false
	}
	for _, d := range listSubCgroups(path) {
		if len(cgroupMembers(d)) > 0 {
			return false
		}
	}
	return true
}

// listSubCgroups returns every descendant cgroup directory under path (deepest
// last order is not guaranteed; callers sort as needed).
func listSubCgroups(path string) []string {
	var out []string
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == path || !d.IsDir() {
			return nil
		}
		if _, serr := os.Stat(filepath.Join(p, "cgroup.procs")); serr != nil {
			return nil
		}
		out = append(out, p)
		return nil
	})
	// Remove deepest-first so a parent is removed only after its children.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// removeCgroupTree removes a cgroup and every empty descendant, deepest first.
func removeCgroupTree(path string) bool {
	for _, d := range listSubCgroups(path) {
		_ = os.Remove(d)
	}
	return os.Remove(path) == nil
}

// KillCgroupTreeNested kills and removes a cgroup and every descendant. It is
// the workspace-parent analogue of KillCgroupTree (which only removes one
// directory and assumes it has no child cgroups).
func KillCgroupTreeNested(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(path, cgroupKillFile), []byte("1"), 0o644)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cgroupTreeEmpty(path) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	removeCgroupTree(path)
}

// readOOMKill reads the cumulative OOM count of a cgroup from `memory.events`
// (which INCLUDES descendants): the sum of `oom_kill` and `oom_group_kill`.
//
// It deliberately does NOT use `memory.events.local`: a workspace parent has no
// processes of its own (they live in the leaf cgroups), so with
// `memory.oom.group=1` the parent's LOCAL counter records `oom_group_kill` and
// leaves `oom_kill` at 0 (measured). Reading the descendant-inclusive file (and
// summing both fields) counts either kind of OOM the workspace hit.
//
// It returns -1 when the file cannot be read, so a caller can tell "no counter
// available" from "counter is zero" and a missing counter never fabricates an
// OOM.
func readOOMKill(dir string) int64 {
	if dir == "" {
		return -1
	}
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return -1
	}
	var total int64
	seen := false
	for _, line := range strings.Split(string(b), "\n") {
		for _, key := range []string{"oom_kill ", "oom_group_kill "} {
			if v, ok := strings.CutPrefix(line, key); ok {
				n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				if err == nil {
					total += n
					seen = true
				}
			}
		}
	}
	if !seen {
		return -1
	}
	return total
}
