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
// uptime/loadavg, and a curated `mounts` so a bare `df`/`findmnt` do not error.
//
// This payload is generated per workspace from the workspace's OWN limits and
// bind-mounted READ-ONLY over /proc. It contains no host process, network or
// kernel-identity fact. Every value here agrees with the other channels:
//   - meminfo MemTotal      == the workspace's memory_mb (the cgroup ceiling)
//   - meminfo SwapTotal     == 0 (no swap is given to a workspace)
//   - cpuinfo/stat          == exactly cpu_cores entries
//   - version               == the same kernel string the shim's uname returns
//
// The mount list uses a neutral device token (/dev/workspace), never the host
// image path, so it reveals nothing about where the workspace lives.
//
// memMB is the workspace's memory ceiling, diskMB its per-file/disk allowance,
// cpus its cpu count and kernelRelease the identity string the shim answers.

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

	// The synthetic mount list. A bare `df` and `findmnt` read it, so they
	// report the workspace's own filesystem instead of erroring on a missing
	// mounts table. The device token is neutral: never the host image path.
	mounts := procMountSource + " /workspace fuse.ext4 rw,nosuid,nodev,relatime 0 0\n" +
		"tmpfs / tmpfs rw,nosuid,nodev,relatime 0 0\n" +
		"proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0\n"
	for _, name := range []string{"mounts", "self/mounts", "tab"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(mounts), 0o644); err != nil {
			return "", err
		}
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
