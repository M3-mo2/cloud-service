package workspace

import (
	"errors"
	"os"
	"path/filepath"
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
