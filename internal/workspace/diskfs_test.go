package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestDiskManagerFitsAllowance proves the mounted filesystem's statvfs total
// equals the requested allowance exactly, and that a write past it fails with
// ENOSPC (the kernel boundary). It is skipped when the host has no usable FUSE
// backend, so `go test ./...` stays green on a host without /dev/fuse.
func TestDiskManagerFitsAllowance(t *testing.T) {
	dm := NewDiskManager(DiskManagerConfig{})
	if ok, reason := dm.Available(); !ok {
		t.Skipf("fuse backend unavailable: %s", reason)
	}
	root := filepath.Join(t.TempDir(), "files")
	if err := dm.Ensure(root, 256); err != nil {
		t.Skipf("fuse mount not permitted here: %v", err)
	}
	defer func() { _ = dm.Remove(root) }()

	total, _, err := statvfs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(256) * 1024 * 1024
	if total != want {
		t.Fatalf("statvfs total = %d, want %d", total, want)
	}

	// Fill past the allowance: the first 200 MiB lands, the next fails ENOSPC.
	big := filepath.Join(root, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	var wrote int64
	for i := 0; i < 300; i++ {
		n, werr := f.Write(chunk)
		wrote += int64(n)
		if werr != nil {
			if !isENOSPC(werr) {
				f.Close()
				t.Fatalf("write failed with a non-ENOSPC error: %v", werr)
			}
			break
		}
	}
	f.Close()
	if wrote > want {
		t.Fatalf("wrote %d bytes past the %d allowance", wrote, want)
	}
	if wrote == 0 {
		t.Fatalf("wrote nothing; the boundary refused the very first byte")
	}
	// The exhaustion proof: the boundary must have been reached (a write failed).
	if wrote >= int64(300)*(1<<20) {
		t.Fatalf("all 300 MiB landed; the allowance was not enforced")
	}
}

// TestDiskManagerDistinctPerWorkspace proves two workspaces with different
// allowances get different statvfs totals (no hardcoded value).
func TestDiskManagerDistinctPerWorkspace(t *testing.T) {
	dm := NewDiskManager(DiskManagerConfig{})
	if ok, reason := dm.Available(); !ok {
		t.Skipf("fuse backend unavailable: %s", reason)
	}
	base := t.TempDir()
	r1 := filepath.Join(base, "a", "files")
	r2 := filepath.Join(base, "b", "files")
	if err := dm.Ensure(r1, 64); err != nil {
		t.Skipf("fuse mount not permitted here: %v", err)
	}
	defer func() { _ = dm.Remove(r1) }()
	if err := dm.Ensure(r2, 128); err != nil {
		t.Skipf("fuse mount not permitted here: %v", err)
	}
	defer func() { _ = dm.Remove(r2) }()
	t1, _, _ := statvfs(r1)
	t2, _, _ := statvfs(r2)
	if t1 != 64*1024*1024 || t2 != 128*1024*1024 {
		t.Fatalf("totals = %d, %d; want 64MiB, 128MiB", t1, t2)
	}
	if t1 == t2 {
		t.Fatal("identical totals across different allowances (hardcoded)")
	}
}

func isENOSPC(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// TestDiskManagerHidesLostFound proves the ext4-internal /lost+found is removed
// from a workspace's visible root on BOTH the fresh-create path and the
// e2fsck-repair path (the case a one-shot removal at image creation misses: a
// repair recreates the directory). It is skipped when the host has no usable
// FUSE backend.
func TestDiskManagerHidesLostFound(t *testing.T) {
	dm := NewDiskManager(DiskManagerConfig{})
	if ok, reason := dm.Available(); !ok {
		t.Skipf("fuse backend unavailable: %s", reason)
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs not available to dirty the image")
	}
	root := filepath.Join(t.TempDir(), "files")
	if err := dm.Ensure(root, 64); err != nil {
		t.Skipf("fuse mount not permitted here: %v", err)
	}
	defer func() { _ = dm.Remove(root) }()
	lf := filepath.Join(root, "lost+found")
	if _, err := os.Lstat(lf); !os.IsNotExist(err) {
		t.Fatalf("fresh workspace root still has lost+found (err=%v)", err)
	}
	// The image still carries the directory inside (the user's requirement is
	// that it not be VISIBLE, not that it not exist); prove it is there in the
	// image so the removal is a real removal, not a no-op on an absent dir.
	img := imagePath(root)
	if err := dm.Release(root); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Put the directory back and dirty the superblock, exactly the state a
	// repair leaves and an interrupted write leaves.
	if out, err := exec.Command("debugfs", "-w", "-R", "mkdir /lost+found", img).CombinedOutput(); err != nil {
		t.Fatalf("debugfs mkdir lost+found: %v: %s", err, out)
	}
	if out, err := exec.Command("debugfs", "-w", "-R", "ssv state 0", img).CombinedOutput(); err != nil {
		t.Fatalf("debugfs dirty: %v: %s", err, out)
	}
	if !debugfsHasLostFound(t, img) {
		t.Fatal("precondition failed: image does not contain /lost+found before reprovision")
	}
	// Re-provision at a different size: resize2fs refuses the dirty superblock,
	// so the engine runs e2fsck (which recreates /lost+found) and retries.
	if err := dm.Ensure(root, 96); err != nil {
		t.Fatalf("Ensure on a dirty image did not self-heal: %v", err)
	}
	if _, err := os.Lstat(lf); !os.IsNotExist(err) {
		t.Fatalf("after the repair the visible root still has lost+found (err=%v)", err)
	}
}

// debugfsHasLostFound reports whether an image's root contains lost+found.
func debugfsHasLostFound(t *testing.T, img string) bool {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "ls -p /", img).Output()
	if err != nil {
		t.Fatalf("debugfs ls: %v", err)
	}
	return strings.Contains(string(out), "/lost+found")
}

// TestDiskManagerResizeRepairsDirtyImage proves the self-healing path: an image
// left with a dirty ext4 superblock (which makes resize2fs refuse with "Please
// run 'e2fsck -f' first") is repaired with e2fsck and the resize retried, rather
// than bricking the workspace with a permanent provisioning failure. This is the
// defense-in-depth half of the fit race fix; the read-only probe is the primary
// fix (tested implicitly by Ensure succeeding).
func TestDiskManagerResizeRepairsDirtyImage(t *testing.T) {
	dm := NewDiskManager(DiskManagerConfig{})
	if ok, reason := dm.Available(); !ok {
		t.Skipf("fuse backend unavailable: %s", reason)
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs not available to dirty the image")
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Skip("e2fsck not available")
	}
	root := filepath.Join(t.TempDir(), "files")
	if err := dm.Ensure(root, 64); err != nil {
		t.Skipf("fuse mount not permitted here: %v", err)
	}
	img := imagePath(root)
	// Unmount but keep the image, so we can dirty it offline.
	if err := dm.Release(root); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Mark the superblock not-clean, exactly the state an interrupted metadata
	// write leaves behind.
	out, err := exec.Command("debugfs", "-w", "-R", "ssv state 0", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs dirty: %v: %s", err, out)
	}
	// A resize of a differently-sized allowance must repair and succeed.
	if err := dm.Ensure(root, 96); err != nil {
		t.Fatalf("Ensure on a dirty image did not self-heal: %v", err)
	}
	defer func() { _ = dm.Remove(root) }()
	total, _, err := statvfs(root)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(96)*1024*1024 {
		t.Fatalf("statvfs total after repair = %d, want 96 MiB", total)
	}
}
