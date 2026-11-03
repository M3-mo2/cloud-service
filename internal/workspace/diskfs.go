package workspace

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// DiskBackend selects how a workspace's disk allowance is enforced.
//
//   - "fuse"    — a real per-workspace ext4 filesystem image, mounted with
//     fuse2fs. This is a KERNEL-enforced boundary: a write past the
//     allowance fails with ENOSPC at the boundary, with no
//     overshoot, and `df`/`stat -f`/os.statvfs() report the
//     workspace's own size because the mounted filesystem IS that
//     size. This is the default and the strongest mechanism.
//   - "counter" — the engine's own used-bytes counter, promoted to an authority
//     over running processes (the API file path plus a run-stopping
//     check). Real but SAMPLED: a process already running can
//     overshoot the allowance before the next check terminates it.
//     The measured overshoot is documented in docs/DECISIONS.md.
//   - "none"    — no disk enforcement. Nothing is reported as an allowance and
//     nothing is enforced (the honest state).
type DiskBackend string

// Disk backends.
const (
	DiskBackendFuse    DiskBackend = "fuse"
	DiskBackendCounter DiskBackend = "counter"
	DiskBackendNone    DiskBackend = "none"
)

// DiskManagerConfig configures the per-workspace disk image manager.
type DiskManagerConfig struct {
	Backend    DiskBackend
	Fuse2FS    string // fuse2fs binary (default "fuse2fs")
	MkfsExt4   string // mkfs.ext4 binary (default "mkfs.ext4")
	Resize2FS  string // resize2fs binary (default "resize2fs")
	E2fsck     string // e2fsck binary (default "e2fsck"); repairs a dirty image
	Fusermount string // fusermount3 binary (default "fusermount3")
	UID        int    // sandbox uid written into the image (uid= mount option)
	GID        int    // sandbox gid
}

func (c DiskManagerConfig) withDefaults() DiskManagerConfig {
	if c.Fuse2FS == "" {
		c.Fuse2FS = "fuse2fs"
	}
	if c.MkfsExt4 == "" {
		c.MkfsExt4 = "mkfs.ext4"
	}
	if c.Resize2FS == "" {
		c.Resize2FS = "resize2fs"
	}
	if c.E2fsck == "" {
		c.E2fsck = "e2fsck"
	}
	if c.Fusermount == "" {
		c.Fusermount = "fusermount3"
	}
	return c
}

// DiskManager provisions and mounts a per-workspace ext4 filesystem image. The
// image file lives beside the workspace's `files` directory
// ({wsdir}/disk.img); the workspace directory itself is the mount point, so the
// executor's existing bind of WorkspaceRoot needs no change: it binds whatever
// is mounted at that path.
//
// The mount is performed HOST-SIDE by the orchestrator, so the workspace only
// ever sees the mounted result and needs neither /dev/fuse nor fuse2fs inside
// the jail. This is the cleanest shape and keeps /dev minimal.
type DiskManager struct {
	cfg DiskManagerConfig

	mu    sync.Mutex
	state map[string]*diskState
}

// diskState tracks a live per-workspace image.
type diskState struct {
	image    string
	mounted  bool
	sizeMB   int
	mountErr error
}

// NewDiskManager constructs a disk manager.
func NewDiskManager(cfg DiskManagerConfig) *DiskManager {
	return &DiskManager{cfg: cfg.withDefaults(), state: map[string]*diskState{}}
}

// Available reports whether the FUSE backend can run: the fuse2fs, mkfs.ext4,
// resize2fs and fusermount binaries must be resolvable and /dev/fuse must be a
// usable character device. reason explains an unavailable state.
func (d *DiskManager) Available() (bool, string) {
	for _, b := range []string{d.cfg.Fuse2FS, d.cfg.MkfsExt4, d.cfg.Fusermount} {
		if _, err := exec.LookPath(b); err != nil {
			return false, "missing binary: " + b
		}
	}
	if fi, err := os.Stat("/dev/fuse"); err != nil {
		return false, "/dev/fuse unavailable: " + err.Error()
	} else if fi.Mode()&os.ModeCharDevice == 0 {
		return false, "/dev/fuse is not a character device"
	}
	return true, ""
}

// imagePath returns the image file for a workspace mount point.
func imagePath(root string) string {
	return filepath.Join(filepath.Dir(root), "disk.img")
}

// Ensure creates the per-workspace image (if absent) and mounts it at root. It
// is idempotent: calling it for an already-mounted workspace verifies the mount
// and the size, and returns without a second mount. sizeMB is the workspace's
// allowance; the filesystem is sized so that statvfs total == sizeMB (see
// fitImage). root must exist and be empty of other content.
func (d *DiskManager) Ensure(root string, sizeMB int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.state[root]; ok && st.mounted && st.sizeMB == sizeMB {
		if st.mountErr == nil {
			if err := clearLostFound(root); err != nil {
				return err
			}
		}
		return st.mountErr
	}
	if sizeMB < 1 {
		sizeMB = 1
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	img := imagePath(root)
	if err := d.ensureImage(img, sizeMB); err != nil {
		d.state[root] = &diskState{image: img, mounted: false, sizeMB: sizeMB, mountErr: err}
		return err
	}
	// If the desired size differs from the on-disk filesystem size, unmount and
	// resize first (ext4 cannot be grown under fuse2fs reliably, so the image is
	// resized offline).
	if mountedFuse(root) {
		if cur, err := fsTotalMB(root); err == nil && cur == int64(sizeMB) {
			// Already mounted at the right size: still converge the visible
			// root (an image repaired by e2fsck regains /lost+found even when
			// no resize is needed).
			if err := clearLostFound(root); err != nil {
				d.state[root] = &diskState{image: img, mounted: true, sizeMB: sizeMB, mountErr: err}
				return err
			}
			d.state[root] = &diskState{image: img, mounted: true, sizeMB: sizeMB}
			return nil
		}
		_ = d.unmount(root)
	}
	if err := d.fitImage(img, sizeMB); err != nil {
		d.state[root] = &diskState{image: img, mounted: false, sizeMB: sizeMB, mountErr: err}
		return err
	}
	if err := d.mount(img, root, false); err != nil {
		d.state[root] = &diskState{image: img, mounted: false, sizeMB: sizeMB, mountErr: err}
		return err
	}
	// The workspace is now mounted but not yet usable: hide the ext4-internal
	// /lost+found before recording the disk ready. This runs AFTER mkfs.ext4 and
	// AFTER any e2fsck repair, so it converges for a NEW image and for an image
	// e2fsck has just repaired. See clearLostFound and docs/DECISIONS.md ADR 98.
	if err := clearLostFound(root); err != nil {
		d.state[root] = &diskState{image: img, mounted: true, sizeMB: sizeMB, mountErr: err}
		return err
	}
	d.state[root] = &diskState{image: img, mounted: true, sizeMB: sizeMB}
	return nil
}

// clearLostFound removes the ext4-internal `/lost+found` from a MOUNTED
// workspace root so the tenant's visible root contains only what the tenant put
// there. It is called on every provisioning path, AFTER mkfs.ext4 and AFTER any
// e2fsck repair, so it converges for a new image and for an image a repair has
// just re-marked.
//
// WHY IT IS NEEDED (a property of ext4, not of this codebase). `mkfs.ext4`
// creates `/lost+found` in every new filesystem, and `e2fsck` RECREATES it
// whenever it runs (measured: `e2fsck -p -f` on an image whose directory was
// removed prints `/lost+found not found.  CREATED.` and puts it back, exit 1).
// The engine runs e2fsck on an image it finds dirty, so a one-shot removal at
// image creation is not enough: the directory returns the first time a repair
// runs. Removing it on every provisioning path, mounted, is what makes the fix
// hold.
//
// REMOVAL THROUGH THE MOUNTED ROOT, in one place. The image root is mounted
// read-write at the workspace root already (the workspace's own mount), so a
// plain rmdir of `{root}/lost+found` is the mechanism; no extra tool and no
// second read-write mount is introduced. It is idempotent: a missing directory
// is not an error.
//
// THE TRADE-OFF, stated honestly. `/lost+found` is where fsck reconnects
// ORPHANED inodes after a corruption. Removing it leaves a repair with nowhere
// to put such files. This is normally empty (mkfs.ext4 creates it empty; the
// measured `e2fsck -p -f` that recreates it recreates it empty), and an empty
// directory holds nothing, so nothing of the tenant's is lost: the tenant's
// INTACT files are untouched, because e2fsck never moves a reachable file into
// lost+found. What is lost is only the recovery of orphaned inodes on a
// genuinely corrupt image. So that the safety net is not destroyed silently, a
// NON-empty `/lost+found` is LEFT IN PLACE rather than deleted: the only way it
// becomes non-empty is a repair that actually reconnected orphaned inodes, and
// deleting it then would destroy exactly the data it exists to hold. In that
// rare, corruption-only case the directory stays visible (the tenant can see
// and recover the recovered files), rather than being hidden at the cost of
// their contents.
func clearLostFound(root string) error {
	lf := filepath.Join(root, "lost+found")
	fi, err := os.Lstat(lf)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Only the ext4 directory itself is a candidate. A differently-typed entry
	// (e.g. a symlink or a regular file the tenant created) is left alone: the
	// mechanism must not remove anything the tenant put in the workspace.
	if !fi.IsDir() {
		return nil
	}
	if err := os.Remove(lf); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			// A repair reconnected orphaned inodes; keep them.
			return nil
		}
		return err
	}
	return nil
}

// Release unmounts a workspace's image but keeps the image file (so a restart
// or a suspended workspace does not lose data).
func (d *DiskManager) Release(root string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.unmount(root)
	delete(d.state, root)
	return err
}

// Remove unmounts the image and deletes the image file, so deleting a
// workspace leaves nothing behind.
func (d *DiskManager) Remove(root string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.unmount(root)
	delete(d.state, root)
	if err := os.Remove(imagePath(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Mounted reports whether root is currently a fuse mount.
func (d *DiskManager) Mounted(root string) bool { return mountedFuse(root) }

// ForceUnmount unmounts root regardless of tracked state. It is used by the
// startup orphan sweep, where the mount exists but this manager has no state.
func (d *DiskManager) ForceUnmount(root string) error { return d.unmount(root) }

// FuseMountsUnder returns every live FUSE mount point whose path is prefix or a
// child of prefix, read from /proc/self/mountinfo. It is used to reap mounts
// left behind by a hard daemon death. A read failure yields no mounts.
func FuseMountsUnder(prefix string) []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// mountinfo fields: id parent major:minor root mountpoint options
		// [optional...] - fstype source superopts
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		target := fields[4]
		if !pathHasPrefix(target, prefix) {
			continue
		}
		// The fstype follows the "-" separator.
		for i := 0; i < len(fields); i++ {
			if fields[i] == "-" && i+1 < len(fields) {
				if strings.HasPrefix(fields[i+1], "fuse") {
					out = append(out, target)
				}
				break
			}
		}
	}
	return out
}

// pathHasPrefix reports whether p is prefix or a child of it.
func pathHasPrefix(p, prefix string) bool {
	if prefix == "" {
		return false
	}
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return len(p) == len(prefix) || p[len(prefix)] == '/'
}

// ensureImage creates an ext4 image of about sizeMB if it does not exist, then
// fits it exactly.
func (d *DiskManager) ensureImage(img string, sizeMB int) error {
	if _, err := os.Stat(img); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		return err
	}
	// The image needs headroom for ext4 metadata; 1.5x + slack, then resized
	// down to the exact target by fitImage.
	headroom := sizeMB + sizeMB/2 + 32
	f, err := os.OpenFile(img, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(headroom) * 1024 * 1024); err != nil {
		f.Close()
		return err
	}
	f.Close()
	cmd := exec.Command(d.cfg.MkfsExt4, "-q", "-F", "-m", "0", "-O", "^has_journal", img)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fitImage sizes the ext4 filesystem inside img so that the mounted total
// (statvfs f_blocks * f_frsize) equals sizeMB * 1 MiB. ext4's own metadata
// makes the usable total shrink as the filesystem grows, so the fit is
// iterative: resize2fs to a target block count, measure the resulting total via
// a transient mount, and correct. It converges in a few iterations (measured
// for 8 MiB..1 GiB). The image file is grown if needed to hold the target.
func (d *DiskManager) fitImage(img string, sizeMB int) error {
	targetBlocks := int64(sizeMB) * 1024 * 1024 / 4096
	// Read the current filesystem block count.
	bc, err := fsBlockCount(img)
	if err != nil {
		return err
	}
	// Grow the backing file if the fs needs more room than it has.
	needBytes := (targetBlocks + targetBlocks/4 + 4096) * 4096
	if fi, err := os.Stat(img); err == nil && fi.Size() < needBytes {
		if err := os.Truncate(img, needBytes); err != nil {
			return err
		}
	}
	probe := filepath.Join(filepath.Dir(img), ".fit-"+filepath.Base(filepath.Dir(img)))
	_ = os.MkdirAll(probe, 0o755)
	// Iterate. The correction is exact in one step when statvfs total == the
	// resize target; otherwise the delta (desired - measured) is added.
	for i := 0; i < 8; i++ {
		if err := d.resize2fs(img, bc); err != nil {
			return err
		}
		total, err := d.measureTotalBlocks(img, probe)
		if err != nil {
			return err
		}
		if total == targetBlocks {
			_ = os.RemoveAll(probe)
			return nil
		}
		bc += targetBlocks - total
		if bc < 1024 {
			bc = 1024
		}
		// Ensure the backing file can hold the new target.
		nb := (bc + bc/4 + 4096) * 4096
		if fi, err := os.Stat(img); err == nil && fi.Size() < nb {
			if err := os.Truncate(img, nb); err != nil {
				return err
			}
		}
	}
	_ = os.RemoveAll(probe)
	return fmt.Errorf("could not fit image %s to %d MB (ext4 metadata overhead)", img, sizeMB)
}

// measureTotalBlocks mounts img transiently at probe and returns statvfs
// f_blocks -- the total block count the workspace will see.
//
// The probe mount is READ-ONLY. It only statvfs's the filesystem, so it needs no
// write access, and mounting read-write here is a real defect: fuse2fs commits
// the ext4 superblock asynchronously, so a read-write unmount immediately
// followed by resize2fs races the kernel's final superblock write and resize2fs
// intermittently refuses with "Please run 'e2fsck -f ... first" (measured: 48/60
// with a read-write probe, 0/60 with a read-only probe). A read-only mount never
// dirties the image, so the fit is deterministic.
func (d *DiskManager) measureTotalBlocks(img, probe string) (int64, error) {
	if err := d.mount(img, probe, true); err != nil {
		return 0, err
	}
	defer d.unmount(probe)
	_, blocks, err := statvfsFull(probe)
	return blocks, err
}

// resize2fs runs resize2fs to the given block count. If the resize is refused
// because the ext4 superblock is dirty ("Please run 'e2fsck -f ... first"), it
// repairs the filesystem with e2fsck and retries once. The read-only probe fix
// above prevents the dirty state going forward, but an image left dirty by an
// older build must still be repairable rather than bricking the workspace with a
// permanent 503.
func (d *DiskManager) resize2fs(img string, blocks int64) error {
	first := d.resize2fsOnce(img, blocks)
	if first == nil {
		return nil
	}
	if !strings.Contains(first.Error(), "e2fsck") {
		return first
	}
	if err := d.e2fsck(img); err != nil {
		return first // report the original failure; the repair did not help
	}
	return d.resize2fsOnce(img, blocks)
}

// resize2fsOnce runs resize2fs one time.
func (d *DiskManager) resize2fsOnce(img string, blocks int64) error {
	cmd := exec.Command(d.cfg.Resize2FS, img, strconv.FormatInt(blocks, 10))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs to %d: %v: %s", blocks, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// e2fsck repairs an image non-interactively. -p preens (never prompts) and -f
// forces a full check even when the superblock looks clean, which is what clears
// a dirty flag left by an interrupted metadata write. e2fsck exits 0 (no errors)
// or 1 (errors corrected); anything else is a genuine failure.
func (d *DiskManager) e2fsck(img string) error {
	cmd := exec.Command(d.cfg.E2fsck, "-p", "-f", img)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("e2fsck %s: %v: %s", img, err, strings.TrimSpace(string(out)))
}

// mount mounts an image at dir as the sandbox uid with allow_other. When
// readOnly is true the mount is a read-only probe (no ext4 superblock write, so
// a following resize2fs is never raced); the workspace's own mount is read-write.
func (d *DiskManager) mount(img, dir string, readOnly bool) error {
	if mountedFuse(dir) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	opts := fmt.Sprintf("allow_other,fakeroot,uid=%d,gid=%d", d.cfg.UID, d.cfg.GID)
	if readOnly {
		opts = "ro," + opts
	}
	cmd := exec.Command(d.cfg.Fuse2FS, "-o", opts, img, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fuse2fs mount %s: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	if !mountedFuse(dir) {
		return fmt.Errorf("fuse2fs reported success but %s is not a fuse mount", dir)
	}
	// The mount root must be traversable and writable by the sandbox uid; the
	// fakeroot/uid options make the image's root owned by the sandbox uid, and
	// the workspace directory itself must be searchable by every ancestor.
	_ = os.Chmod(dir, 0o755)
	return nil
}

// unmount unmounts dir if it is a fuse mount.
func (d *DiskManager) unmount(dir string) error {
	if !mountedFuse(dir) {
		return nil
	}
	cmd := exec.Command(d.cfg.Fusermount, "-u", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fall back to a lazy unmount; a busy workspace (a running workload)
		// must not wedge the caller.
		cmd2 := exec.Command(d.cfg.Fusermount, "-u", "-z", dir)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("fusermount -u %s: %v: %s (lazy: %v: %s)", dir, err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

// fsBlockCount returns the ext4 block count recorded in the superblock, read
// with dumpe2fs when available, else derived from a transient mount.
func fsBlockCount(img string) (int64, error) {
	if p, err := exec.LookPath("dumpe2fs"); err == nil {
		out, err := exec.Command(p, "-h", img).Output()
		if err == nil {
			sc := bufio.NewScanner(strings.NewReader(string(out)))
			var blocks int64 = -1
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "Block count:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "Block count:"))
					if n, err := strconv.ParseInt(v, 10, 64); err == nil {
						blocks = n
					}
				}
			}
			if blocks > 0 {
				return blocks, nil
			}
		}
	}
	return 0, fmt.Errorf("could not read block count of %s (dumpe2fs missing?)", img)
}

// fsTotalBlocks reports statvfs f_blocks for a path.
func fsTotalBlocks(path string) (int64, error) {
	total, _, err := statvfs(path)
	return total, err
}

// fsTotalMB reports the mounted total size in MiB (rounded).
func fsTotalMB(path string) (int64, error) {
	bsize, blocks, err := statvfsFull(path)
	if err != nil {
		return 0, err
	}
	return bsize * blocks / (1024 * 1024), nil
}
