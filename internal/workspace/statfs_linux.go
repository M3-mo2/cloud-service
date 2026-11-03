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
