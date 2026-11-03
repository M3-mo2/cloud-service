package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Synthetic per-workspace /proc.
//
// The namespace deliberately does NOT mount the real procfs (mount_proc: false):
// the real /proc leaks host process, network and kernel-identity information,
// which the whole identity layer exists to hide. But a workspace still needs a
// handful of /proc files that describe ITS OWN resources -- meminfo so `free -h`
// reports the workspace's memory, cpuinfo/stat so `top`/`vmstat` have a shape,
// uptime/loadavg, and a curated mount table so a bare `df`/`findmnt` do not
// error.
//
// This payload is generated per workspace from the workspace's OWN limits and
// bind-mounted READ-ONLY over /proc. It contains no host process, network or
// kernel-identity fact. Every value here agrees with the other channels:
//   - meminfo MemTotal      == the workspace's memory_mb (the cgroup ceiling)
//   - meminfo SwapTotal     == 0 (no swap is given to a workspace)
//   - cpuinfo/stat          == exactly cpu_cores entries
//   - version               == the same kernel string the shim's uname returns
//
// The mount table is emitted in BOTH the /proc/mounts shape (maj:min is only
// informational there) AND the /proc/self/mountinfo shape, from the SAME
// procMount list, so the two can never disagree. Coreutils `df` (the operator's
// acceptance command) reads /proc/self/mountinfo, not /proc/mounts: without
// mountinfo a bare `df -h` prints "cannot read table of mounted file systems"
// and shows no row (measured), while `df --output=size <path>` still works
// because it can statvfs the path -- which is exactly why the earlier gate
// passed while the user-facing command was broken. The mount list uses a neutral
// device token (/dev/workspace), never the host image path, so it reveals
// nothing about where the workspace lives.
//
// memMB is the workspace's memory ceiling, diskMB its per-file/disk allowance,
// cpus its cpu count and kernelRelease the identity string the shim answers.

// procMount is one entry of the curated mount table. It is rendered into both
// the /proc/mounts and /proc/self/mountinfo formats from a single list so the
// two views are always consistent.
type procMount struct {
	source  string // device token (neutral; never the host image path)
	target  string // mount point
	fstype  string
	options string // per-mount options, comma separated

	// mountinfo-only fields. root is the filesystem root within the mount; a
	// bind mount of a whole filesystem uses "/". major/minor are the device
	// numbers shown in the mountinfo "major:minor" column. They are fixed,
	// neutral values (0:0) -- mountinfo does not require them to exist, and a
	// real dev_t would leak host device identity.
	root string
}

// procMounts is the curated mount table, exactly the list /proc/mounts has
// always carried: the workspace's own filesystem, the synthetic root and the
// curated /proc. /proc/self/mountinfo is rendered from THIS list, so a bare `df`
// renders a complete row for the workspace (and a sane row for /) without
// naming anything about the host. Only mounts that exist in the jail appear, so
// `df` (which stats each mount point) never errors on a missing path.
func procMounts() []procMount {
	return []procMount{
		{source: "tmpfs", target: "/", fstype: "tmpfs", options: "rw,nosuid,nodev,relatime", root: "/"},
		{source: "proc", target: "/proc", fstype: "proc", options: "rw,nosuid,nodev,noexec,relatime", root: "/"},
		{source: procMountSource, target: "/workspace", fstype: "fuse.ext4", options: "rw,nosuid,nodev,relatime", root: "/"},
	}
}

// renderProcMounts renders the table in the /proc/mounts format:
// source target fstype options 0 0
func renderProcMounts(ms []procMount) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%s %s %s %s 0 0\n", m.source, m.target, m.fstype, m.options)
	}
	return b.String()
}

// renderProcMountinfo renders the table in the real procfs mountinfo format:
// mnt_id parent_id major:minor root mount_point mount_options [optional fields] - fstype source super_options
// The ids are stable, generated values (1..n, the synthetic root carries id 1
// and is its own parent; every other mount is a child of it). Coreutils `df`
// dedups rows by device number, so each mount is given a DISTINCT synthetic
// major:minor (0:1, 0:2, ...): real device numbers would leak host device
// identity, and sharing one would collapse the table to a single row. The
// synthetic root keeps 0:0. Coreutils `df` reads exactly this file, so its
// presence is what makes a bare `df -h` work.
func renderProcMountinfo(ms []procMount) string {
	var b strings.Builder
	for i, m := range ms {
		id := i + 1
		// The root mount is its own parent (the kernel's initial mount does
		// this); every other mount hangs off the root (id 1).
		parent := 1
		root := m.root
		if root == "" {
			root = "/"
		}
		major, minor := 0, i // distinct 0:0, 0:1, 0:2, ... so df shows every row
		// The super_options column repeats the mount options: the kernel emits
		// the mount's own flags there, and repeating them is a valid table.
		fmt.Fprintf(&b, "%d %d %d:%d %s %s %s - %s %s %s\n",
			id, parent, major, minor, root, m.target, m.options, m.fstype, m.source, m.options)
	}
	return b.String()
}

// writeSyntheticProc materializes the curated /proc directory at dir and
// returns it.
func writeSyntheticProc(dir string, cpus int, memMB, diskMB int, kernelRelease string) (string, error) {
	if cpus < 1 {
		cpus = 1
	}
	if memMB < 1 {
		memMB = 1
	}
	if diskMB < 1 {
		diskMB = 1
	}
	if kernelRelease == "" {
		kernelRelease = "0.0.0-sandbox"
	}
	if err := os.MkdirAll(filepath.Join(dir, "self"), 0o755); err != nil {
		return "", err
	}

	memKB := int64(memMB) * 1024
	meminfo := fmt.Sprintf(`MemTotal:       %8d kB
MemFree:        %8d kB
MemAvailable:   %8d kB
Buffers:               0 kB
Cached:                0 kB
SwapCached:            0 kB
Active:                0 kB
Inactive:              0 kB
SwapTotal:             0 kB
SwapFree:              0 kB
Dirty:                 0 kB
Writeback:             0 kB
AnonPages:             0 kB
Mapped:                0 kB
Shmem:                 0 kB
Slab:                  0 kB
`, memKB, memKB, memKB)
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0o644); err != nil {
		return "", err
	}

	var cpuinfo strings.Builder
	for i := 0; i < cpus; i++ {
		fmt.Fprintf(&cpuinfo, "processor\t: %d\n", i)
		cpuinfo.WriteString("vendor_id\t: GenuineWorkspace\n")
		cpuinfo.WriteString("cpu family\t: 6\n")
		cpuinfo.WriteString("model\t\t: 0\n")
		cpuinfo.WriteString("model name\t: Workspace CPU\n")
		cpuinfo.WriteString("stepping\t: 0\n")
		cpuinfo.WriteString("cpu MHz\t\t: 2000.000\n")
		cpuinfo.WriteString("cache size\t: 0 KB\n")
		cpuinfo.WriteString("flags\t\t: fpu vme de pse tsc msr pae mce cx8 apic sep\n\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(cpuinfo.String()), 0o644); err != nil {
		return "", err
	}

	// /proc/stat: one aggregate cpu line plus one per core, all counters zero.
	// top/vmstat parse this; the values describe an idle workspace of exactly
	// cpu_cores cpus.
	var stat strings.Builder
	stat.WriteString("cpu  0 0 0 100000 0 0 0 0 0 0\n")
	for i := 0; i < cpus; i++ {
		fmt.Fprintf(&stat, "cpu%d 0 0 0 100000 0 0 0 0 0 0\n", i)
	}
	stat.WriteString("intr 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")
	stat.WriteString("ctxt 0\nbtime 0\nprocesses 1\nprocs_running 1\nprocs_blocked 0\n")
	stat.WriteString("softirq 0 0 0 0 0 0 0 0 0 0 0\n")
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat.String()), 0o644); err != nil {
		return "", err
	}

	// uptime: a stable, workspace-local value, never the host's uptime (which
	// would reveal how long the machine has been up).
	if err := os.WriteFile(filepath.Join(dir, "uptime"), []byte("1.00 1.00\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "loadavg"), []byte("0.00 0.00 0.00 1/"+strconv.Itoa(cpus)+" 1\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("Linux version "+kernelRelease+" (sandbox)\n"), 0o644); err != nil {
		return "", err
	}
	vmstat := "nr_free_pages " + strconv.FormatInt(memKB/4, 10) + "\n" +
		"nr_zone_inactive_anon 0\nnr_zone_active_anon 0\nnr_zone_inactive_file 0\n" +
		"nr_zone_active_file 0\nnr_zone_unevictable 0\nnr_zone_write_pending 0\n" +
		"pgpgin 0\npgpgout 0\npswpin 0\npswpout 0\npgfault 0\npgmajfault 0\n"
	if err := os.WriteFile(filepath.Join(dir, "vmstat"), []byte(vmstat), 0o644); err != nil {
		return "", err
	}

	// The synthetic mount table. A bare `df` and `findmnt` read it, so they
	// report the workspace's own filesystem instead of erroring on a missing
	// mounts table. Both formats are rendered from the SAME list so they cannot
	// disagree: /proc/mounts (and its aliases self/mounts and the conventional
	// /etc/mtab symlink target) and /proc/self/mountinfo, which is the file
	// coreutils `df` actually reads. The device token is neutral: never the host
	// image path.
	ms := procMounts()
	mounts := renderProcMounts(ms)
	for _, name := range []string{"mounts", "self/mounts", "tab"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(mounts), 0o644); err != nil {
			return "", err
		}
	}
	// /proc/self/mountinfo is the file `df` builds its filesystem table from; a
	// workspace that lacks it gets "cannot read table of mounted file systems"
	// from a bare `df -h` and no row at all. It is written in the real procfs
	// mountinfo format from the same mount list.
	if err := os.WriteFile(filepath.Join(dir, "self", "mountinfo"), []byte(renderProcMountinfo(ms)), 0o644); err != nil {
		return "", err
	}
	// A curated filesystems list: the workspace's own mount-namespace view, not
	// the host kernel's support table.
	filesystems := "nodev\tsysfs\nnodev\tproc\nnodev\ttmpfs\nnodev\tfuse\n"
	if err := os.WriteFile(filepath.Join(dir, "filesystems"), []byte(filesystems), 0o644); err != nil {
		return "", err
	}
	// The workspace's own limits, never the host's. Max file size mirrors the
	// per-file rlimit_fsize (the disk allowance); Max resident set mirrors the
	// memory ceiling, so the two channels agree.
	limits := "Limit                     Soft Limit           Hard Limit           Units\n" +
		"Max cpu time              unlimited            unlimited            seconds\n" +
		"Max file size             " + fmt.Sprintf("%-20d %-20d bytes", int64(diskMB)*1024*1024, int64(diskMB)*1024*1024) + "\n" +
		"Max resident set          " + fmt.Sprintf("%-20d %-20d bytes", memKB*1024, memKB*1024) + "\n" +
		"Max processes             unlimited            unlimited            processes\n" +
		"Max open files            256                  256                  files\n"
	if err := os.WriteFile(filepath.Join(dir, "self", "limits"), []byte(limits), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

// procMountSource is the neutral device token shown for the workspace filesystem
// in the synthetic mounts list. It deliberately names nothing about the host.
const procMountSource = "/dev/workspace"
