package workspace

import "testing"

// TestDiskUsedFromStatUsesBfree locks the measured bug fix: used must be
// total - f_bfree (what `df` reports), NOT total - f_bavail (which excludes
// ext4's reserved-block margin and made an empty 64 MiB workspace read as 11.9%
// full). The numbers are the measured ones from the task spec.
func TestDiskUsedFromStatUsesBfree(t *testing.T) {
	const bsize = 4096
	// An empty 64 MiB ext4 workspace, as measured: df used = 90112 bytes.
	// blocks = 64MiB/4096 = 16384; the reserved margin shows up in bavail.
	blocks := int64(16384)
	// bfree such that bsize*(blocks-bfree) == 90112  => blocks-bfree = 22
	bfree := blocks - 22
	bavail := bfree - 1856 // a plausible reserved margin
	if got := diskUsedFromStat(bsize, blocks, bfree); got != 90112 {
		t.Fatalf("used from bfree = %d, want 90112", got)
	}
	// The OLD (buggy) formula over-reports on the same input.
	old := bsize*blocks - bsize*bavail
	if old == 90112 {
		t.Fatal("test is not exercising the bavail/bfree distinction")
	}
	if old <= 90112 {
		t.Fatalf("bavail formula unexpectedly <= bfree formula: %d", old)
	}
}

// TestDiskUsedFromStatNeverNegative proves a clamped image cannot report a
// negative used.
func TestDiskUsedFromStatNeverNegative(t *testing.T) {
	if got := diskUsedFromStat(4096, 1, 5); got != 0 {
		t.Fatalf("used = %d, want 0 when bfree > blocks", got)
	}
}
