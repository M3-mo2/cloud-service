package workspace

import "syscall"

// fuseSuperMagic is the statfs f_type of a FUSE filesystem (from the kernel's
// <linux/magic.h>). A path whose statfs carries this type is a live FUSE mount.
const fuseSuperMagic = 0x65735546

// statvfsFull returns the statfs block size (f_bsize) and total blocks
// (f_blocks) for path.
func statvfsFull(path string) (bsize, blocks int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bsize), int64(st.Blocks), nil
}

// statvfs returns the total and free bytes of the filesystem at path, mirroring
// os.statvfs()/statvfs(2) as the workspace sees it.
func statvfs(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return bsize * int64(st.Blocks), bsize * int64(st.Bavail), nil
}

// mountedFuse reports whether path is currently a FUSE mount, by statfs type.
// It deliberately does NOT shell out to findmnt: the type magic is the kernel's
// own answer and cannot be spoofed by a directory.
func mountedFuse(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return int64(st.Type) == fuseSuperMagic
}

// diskUsedFromStat computes the used bytes of a filesystem from a statfs
// result as `df` reports it: total minus the FREE blocks (f_bfree), NOT the
// AVAILABLE blocks (f_bavail). The distinction is the ext4 reserved-block
// margin (mkfs.ext4's -m 5 default): f_bavail excludes the ~5% reserved for
// root, so `total - f_bavail` overstates usage. On an empty 64 MiB workspace
// that is 7.6 MB (11.9% "full") while the tenant's own `df` says ~0; using
// f_bfree matches `df` exactly. The operator's rule is that the number the
// engine reports must equal the number the tenant sees.
func diskUsedFromStat(bsize, blocks, bfree int64) int64 {
	used := bsize*blocks - bsize*bfree
	if used < 0 {
		used = 0
	}
	return used
}

// MountedDiskUsed returns the used bytes (total minus free) of the filesystem
// mounted at root. It is CHEAP: one statfs call, no directory walk. It returns
// ok=false when root is not a live FUSE mount (so the derived summary falls back
// to 0 rather than reporting the host filesystem's usage as the workspace's).
//
// The used figure is `df`'s "used" (total - f_bfree), so it equals what the
// tenant sees inside the workspace. See diskUsedFromStat.
func MountedDiskUsed(root string) (used int64, ok bool) {
	if !mountedFuse(root) {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, false
	}
	return diskUsedFromStat(int64(st.Bsize), int64(st.Blocks), int64(st.Bfree)), true
}

// DiskUsed is the exported form of MountedDiskUsed for callers outside the
// package (the service's summary and the metrics sampler). It is the same one
// statfs call and the same `df`-consistent used figure.
func DiskUsed(root string) (used int64, ok bool) { return MountedDiskUsed(root) }
