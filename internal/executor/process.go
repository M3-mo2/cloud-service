package executor

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file implements the native process inventory and stop primitives on top
// of the per-jail cgroup v2 hierarchy nsjail creates. It exists because a
// workload started inside a session is invisible to and unreachable from the
// API: it cannot be listed, counted or stopped. The cgroup is the only handle
// that reliably contains a process that called setsid()/setpgid() and left its
// process group, so enumeration and kill are cgroup-scoped.
//
// Measured on the host: each run/session gets its own cgroup directory
// /sys/fs/cgroup/NSJAIL.<n> (where <n> is the host pid nsjail assigned the
// workload as PID 1 inside its own pid namespace). The directory is created by
// nsjail for the life of the run and removed by nsjail on a clean exit; it is
// left behind if nsjail is SIGKILLed.

// Environment tags the executor injects into every sandbox so a process can be
// attributed back to its workspace and (for a run/session) its source. They are
// inherited by descendants, so a detached process started from a session still
// carries its session id.
const (
	TagEnvWorkspace = "SANDBOXD_WORKSPACE"
	TagEnvKind      = "SANDBOXD_KIND"
	TagEnvSource    = "SANDBOXD_SOURCE"

	KindRun     = "run"
	KindSession = "session"
	KindExec    = "exec"
)

// ProcessInfo is a single live process discovered inside a workspace's cgroup
// tree. Kind is "run", "session" or "detached"; SourceID carries the run/session
// id it belongs to (empty for "detached"). StartTicks is the raw kernel start
// time used to build a stable id.
type ProcessInfo struct {
	PID         int
	PPID        int
	Argv        []string
	Kind        string
	SourceID    string
	WorkspaceID string
	StartTicks  uint64
	StartedAt   time.Time
}

// cgroupKillFile is the cgroup v2 "kill everything in this group" interface.
// Writing "1" sends SIGKILL to every process in the cgroup, including a process
// that escaped its process group with setsid().
const cgroupKillFile = "cgroup.kill"

// NsjailCgroupPrefix is the directory-name prefix nsjail uses for a jail's
// cgroup under CgroupMount.
const NsjailCgroupPrefix = "NSJAIL."

// nsjailCgroupLeader returns the host pid of the process nsjail placed at PID 1
// of the jail, i.e. the first child of the nsjail process. nsjail creates the
// cgroup directory named after it (NSJAIL.<that pid>). It returns 0 when the
// child cannot be read (the process already exited, or /proc is restricted).
func nsjailCgroupLeader(nsjailPID int) int {
	children, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(nsjailPID), "task", strconv.Itoa(nsjailPID), "children"))
	if err != nil {
		return 0
	}
	best := 0
	for _, f := range strings.Fields(string(children)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		if best == 0 || pid < best {
			best = pid
		}
	}
	return best
}

// CgroupDirOfPID returns the absolute NSJAIL cgroup directory a host pid
// belongs to, or "" when the pid is not in an nsjail cgroup (or /proc is not
// readable). The pid's own cgroup membership is authoritative and survives
// setsid()/setpgid(), unlike its process group.
func CgroupDirOfPID(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		// Format: "0::/NSJAIL.<n>" on a unified (v2) host.
		rest, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		base := filepath.Base(rest)
		if !strings.HasPrefix(base, NsjailCgroupPrefix) {
			return ""
		}
		return filepath.Join(CgroupMount, base)
	}
	return ""
}

// CgroupForNsjailPID resolves the NSJAIL cgroup directory nsjail creates for a
// jail it launched. nsjail forks the jailed workload (PID 1 of its pid
// namespace) just after it starts and names the cgroup after that pid, so a
// brief bounded wait is needed before the directory is visible. It returns ""
// when no cgroup appears within the window (the jail already exited, /proc is
// restricted, or the direct child is not nsjail at all as in a test stub).
func CgroupForNsjailPID(nsjailPID int) string {
	// Fast path: if the direct child is not nsjail (a test stub that exec'd the
	// target, or a reaped jail), there is no NSJAIL cgroup to find and waiting
	// would only delay callers. nsjail's own comm is "nsjail".
	if !directChildIsNsjail(nsjailPID) {
		return ""
	}
	deadline := time.Now().Add(cgroupResolveWindow)
	for {
		if leader := nsjailCgroupLeader(nsjailPID); leader != 0 {
			if dir := CgroupDirOfPID(leader); dir != "" {
				return dir
			}
			if _, err := os.Stat(filepath.Join(CgroupMount, NsjailCgroupPrefix+strconv.Itoa(leader))); err == nil {
				return filepath.Join(CgroupMount, NsjailCgroupPrefix+strconv.Itoa(leader))
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// directChildIsNsjail reports whether a pid names the nsjail supervisor, i.e.
// its /proc/<pid>/comm is exactly "nsjail". A test stub that exec'es the target
// directly has the target's comm (or a stub's own) and is deliberately not
// resolved as a jail, so an unprivileged test does not wait for a cgroup that
// will never appear.
func directChildIsNsjail(pid int) bool {
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == "nsjail"
}

// cgroupResolveWindow bounds how long the cgroup directory is waited for after a
// jail starts. nsjail creates it within a few milliseconds; the window only has
// to cover the fork race, and a non-nsjail child never produces one.
const cgroupResolveWindow = 500 * time.Millisecond

// cgroupMembers returns the host pids that are direct members of a cgroup
// directory.
func cgroupMembers(dir string) []int {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	return parsePIDs(string(b))
}

func parsePIDs(s string) []int {
	var out []int
	for _, f := range strings.Fields(s) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// killCgroup terminates every process in a cgroup. It prefers cgroup.kill (the
// cgroup v2 primitive that signals every member, including a process that
// escaped its process group); when that file is unavailable it falls back to
// SIGKILLing each member individually. It returns true when the cgroup existed
// and a kill was attempted.
func killCgroup(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, cgroupKillFile), []byte("1"), 0o644); err == nil {
		return true
	}
	members := cgroupMembers(dir)
	if len(members) == 0 {
		return true
	}
	for _, pid := range members {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return true
}

// removeCgroup removes an (empty) cgroup directory. cgroup v2 refuses to remove
// a directory that still has processes, so this only succeeds once the members
// are gone; the caller retries after a kill.
func removeCgroup(dir string) bool {
	if dir == "" {
		return false
	}
	return os.Remove(dir) == nil
}

// waitCgroupGone waits until a cgroup directory no longer exists (or has no
// members), removing it when empty, bounded by timeout. It returns true when the
// directory is gone.
func waitCgroupGone(dir string, timeout time.Duration) bool {
	if dir == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(dir); err != nil {
			return true
		}
		if len(cgroupMembers(dir)) == 0 {
			if removeCgroup(dir) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// listNsjailCgroups returns every nsjail cgroup directory under the mount.
func listNsjailCgroups() []string {
	entries, err := os.ReadDir(CgroupMount)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), NsjailCgroupPrefix) {
			out = append(out, filepath.Join(CgroupMount, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// ListJailProcesses samples every process that is currently a member of an
// nsjail cgroup on the host. It is the discovery primitive for the workspace
// inventory: the caller filters by the injected workspace tag. A process whose
// /proc entry disappeared or is unreadable is skipped, never reported as a
// phantom.
func ListJailProcesses() []ProcessInfo {
	var out []ProcessInfo
	seen := map[int]bool{}
	for _, dir := range listNsjailCgroups() {
		for _, pid := range cgroupMembers(dir) {
			if seen[pid] {
				continue
			}
			info, err := sampleProcess(pid)
			if err != nil {
				continue
			}
			// Only report a process whose own cgroup membership is an NSJAIL
			// cgroup: a member of the directory by construction is, but this
			// guards against a stale or foreign listing.
			if CgroupDirOfPID(pid) == "" {
				continue
			}
			seen[pid] = true
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// KillCgroupTree kills every process in a cgroup directory (via cgroup.kill when
// available), waits for the members to drain, and removes the directory. It is
// the cgroup-scoped "stop this process tree completely" primitive: it catches a
// process that escaped its process group with setsid().
func KillCgroupTree(dir string) {
	if dir == "" {
		return
	}
	if _, err := os.Stat(dir); err != nil {
		return
	}
	killCgroup(dir)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(cgroupMembers(dir)) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	removeCgroup(dir)
}

// ReapCgroup is an alias for KillCgroupTree used at startup, where an orphaned
// workload (and its cgroup) left by a hard daemon death is reaped.
func ReapCgroup(dir string) { KillCgroupTree(dir) }

// ReapOrphans kills and removes every nsjail cgroup currently on the host. A
// run or session never survives a service restart, so anything still present at
// startup is an orphan from a hard (SIGKILL) daemon death; a graceful shutdown
// already leaves nothing. It returns the number of cgroups reaped.
func ReapOrphans() int {
	dirs := listNsjailCgroups()
	for _, dir := range dirs {
		KillCgroupTree(dir)
	}
	return len(dirs)
}

// sampleProcess reads the details of a host pid needed for the inventory: argv,
// ppid, cgroup identity and the injected tags. A pid that disappears between
// enumeration and read, or whose /proc entry is not readable, yields an error and
// is skipped by the caller (never reported as a phantom).
func sampleProcess(pid int) (ProcessInfo, error) {
	statB, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcessInfo{}, err
	}
	ppid, startTicks, err := parseStat(string(statB))
	if err != nil {
		return ProcessInfo{}, err
	}

	env := readEnviron(pid)
	info := ProcessInfo{
		PID:         pid,
		PPID:        ppid,
		Argv:        readArgv(pid),
		StartTicks:  startTicks,
		StartedAt:   startTimeFromTicks(startTicks),
		WorkspaceID: env[TagEnvWorkspace],
	}
	switch env[TagEnvKind] {
	case KindRun:
		info.Kind = KindRun
		info.SourceID = env[TagEnvSource]
	case KindSession:
		info.Kind = KindSession
		info.SourceID = env[TagEnvSource]
	default:
		info.Kind = "detached"
	}
	return info, nil
}

// parseStat extracts ppid (field 4) and starttime (field 22) from a /proc stat
// line. The comm field (2) can contain spaces and parentheses, so parsing starts
// after the last ')'.
func parseStat(stat string) (ppid int, startTicks uint64, err error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+2 > len(stat) {
		return 0, 0, errors.New("malformed stat")
	}
	fields := strings.Fields(stat[end+2:])
	// After "pid (comm)" the remaining fields are: state(3) ppid(4) ... starttime(22).
	// fields[0] is state, fields[1] is ppid, fields[19] is starttime.
	if len(fields) < 20 {
		return 0, 0, errors.New("short stat")
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	startTicks, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return ppid, startTicks, nil
}

// readArgv returns a process's argv, or a single-element fallback of the comm
// when the command line is empty (a kernel thread) or unreadable.
func readArgv(pid int) []string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err == nil && len(b) > 0 {
		parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(parts) > 0 && parts[0] != "" {
			return parts
		}
	}
	if comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
		if c := strings.TrimSpace(string(comm)); c != "" {
			return []string{c}
		}
	}
	return []string{}
}

// readEnviron returns the process's environment as a map. An unreadable environ
// (a foreign uid, a restrictive /proc) yields an empty map: the process is then
// reported as detached rather than hidden.
func readEnviron(pid int) map[string]string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil || len(b) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string)
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// clockTicks is the kernel's USER_HZ, the unit of the starttime field. Linux
// fixes it at 100 for the /proc interface.
const clockTicks = 100

// bootTime returns the host boot time in epoch seconds from /proc/stat, or the
// zero time when it cannot be read.
func bootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(secs, 0).UTC()
			}
		}
	}
	return time.Time{}
}

// startTimeFromTicks converts a process starttime (in clock ticks since boot)
// into an absolute wall-clock time. It returns the zero time when the boot time
// cannot be read.
func startTimeFromTicks(ticks uint64) time.Time {
	bt := bootTime()
	if bt.IsZero() {
		return time.Time{}
	}
	return bt.Add(time.Duration(ticks) * time.Second / clockTicks)
}
