package workspace

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFs(t *testing.T) *Fs {
	t.Helper()
	fs, err := NewFs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestWriteReadRoundTrip(t *testing.T) {
	fs := newFs(t)
	if err := fs.Write("/main.py", []byte("print('hi')\n"), false, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, info, truncated, err := fs.Read("/main.py", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("should not be truncated")
	}
	if string(data) != "print('hi')\n" {
		t.Errorf("data = %q", data)
	}
	if info.Path != "/main.py" || info.Size != int64(len("print('hi')\n")) {
		t.Errorf("info = %+v", info)
	}
	if info.SHA256 == "" {
		t.Error("missing sha256")
	}
}

func TestReadTruncation(t *testing.T) {
	fs := newFs(t)
	big := strings.Repeat("a", 100)
	if err := fs.Write("/big.txt", []byte(big), false, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, _, truncated, err := fs.Read("/big.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(data) != 10 {
		t.Fatalf("truncated=%v len=%d", truncated, len(data))
	}
}

func TestWriteRejectsOversize(t *testing.T) {
	fs := newFs(t)
	err := fs.Write("/x", []byte(strings.Repeat("a", 100)), false, WriteOptions{MaxBytes: 10})
	if !IsTooLarge(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestListAndMkdir(t *testing.T) {
	fs := newFs(t)
	if err := fs.Mkdir("/src/pkg"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write("/src/pkg/a.txt", []byte("a"), false, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.List("/src")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "pkg" || entries[0].Type != "dir" {
		t.Fatalf("entries = %+v", entries)
	}
	entries, err = fs.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "src" {
		t.Fatalf("root entries = %+v", entries)
	}
}

func TestListNotDirectory(t *testing.T) {
	fs := newFs(t)
	fs.Write("/f.txt", []byte("x"), false, WriteOptions{})
	if _, err := fs.List("/f.txt"); err != errNotDirectory {
		t.Fatalf("err = %v", err)
	}
}

func TestRemove(t *testing.T) {
	fs := newFs(t)
	fs.Mkdir("/dir")
	fs.Write("/dir/a", []byte("x"), false, WriteOptions{})
	if err := fs.Remove("/dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.List("/dir"); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	if err := fs.Remove("/dir"); !IsNotFound(err) {
		t.Fatalf("second remove err = %v", err)
	}
}

func TestRemoveRootRejected(t *testing.T) {
	fs := newFs(t)
	if err := fs.Remove("/"); !IsUnsafe(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestRename(t *testing.T) {
	fs := newFs(t)
	fs.Write("/a.txt", []byte("x"), false, WriteOptions{})
	if err := fs.Rename("/a.txt", "/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fs.Read("/a.txt", 10); !IsNotFound(err) {
		t.Fatalf("src should be gone: %v", err)
	}
	data, _, _, err := fs.Read("/b/c.txt", 10)
	if err != nil || string(data) != "x" {
		t.Fatalf("dst = %q err = %v", data, err)
	}
}

func TestRenameMissing(t *testing.T) {
	fs := newFs(t)
	if err := fs.Rename("/nope", "/x"); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestAppend(t *testing.T) {
	fs := newFs(t)
	fs.Write("/log", []byte("a"), false, WriteOptions{})
	fs.Write("/log", []byte("b"), true, WriteOptions{})
	data, _, _, _ := fs.Read("/log", 100)
	if string(data) != "ab" {
		t.Fatalf("data = %q", data)
	}
}

func TestStat(t *testing.T) {
	fs := newFs(t)
	fs.Write("/x.json", []byte(`{"a":1}`), false, WriteOptions{})
	info, err := fs.Stat("/x.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 7 || info.SHA256 == "" {
		t.Fatalf("info = %+v", info)
	}
	if info.Binary {
		t.Error("json should be text")
	}
}

func TestQuotaEnforcement(t *testing.T) {
	fs := newFs(t)
	fs.SetDiskLimit(100)
	if err := fs.Write("/a", []byte(strings.Repeat("x", 80)), false, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	err := fs.Write("/b", []byte(strings.Repeat("y", 40)), false, WriteOptions{})
	var qe *QuotaError
	if err == nil {
		t.Fatal("expected quota error")
	}
	if !asQuota(err, &qe) {
		t.Fatalf("err = %v", err)
	}
	if fs.Used() != 80 {
		t.Fatalf("used = %d", fs.Used())
	}
	// Deleting frees space, allowing the write.
	if err := fs.Remove("/a"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write("/b", []byte(strings.Repeat("y", 40)), false, WriteOptions{}); err != nil {
		t.Fatalf("write after free: %v", err)
	}
}

func TestQuotaRecheck(t *testing.T) {
	fs := newFs(t)
	fs.Write("/a", []byte("hello"), false, WriteOptions{})
	// Simulate drift.
	fs.used = 0
	if err := fs.Recheck(); err != nil {
		t.Fatal(err)
	}
	if fs.Used() != 5 {
		t.Fatalf("used = %d", fs.Used())
	}
}

// TestQuotaBoundaryExactlyAtLimit pins the budget arithmetic at the boundary:
// a write that lands EXACTLY on the limit succeeds, and a single byte over is
// refused. The workspace's own allowance is the budget (never the filesystem's
// reserved margin), so the boundary is exact.
func TestQuotaBoundaryExactlyAtLimit(t *testing.T) {
	fs := newFs(t)
	fs.SetDiskLimit(100)
	if err := fs.Write("/at", []byte(strings.Repeat("x", 100)), false, WriteOptions{}); err != nil {
		t.Fatalf("a write that lands exactly on the limit must succeed: %v", err)
	}
	if fs.Used() != 100 {
		t.Fatalf("used = %d, want 100", fs.Used())
	}
	// One block over is refused with a QuotaError.
	err := fs.Write("/over", []byte("y"), false, WriteOptions{})
	var qe *QuotaError
	if !asQuota(err, &qe) {
		t.Fatalf("one byte over the limit must be a QuotaError, got %v", err)
	}
	if qe.Used != 100 || qe.Limit != 100 {
		t.Fatalf("quota error used/limit = %d/%d, want 100/100", qe.Used, qe.Limit)
	}
	// The refused write left the workspace unchanged.
	if fs.Used() != 100 {
		t.Fatalf("a refused write must not change used (got %d)", fs.Used())
	}
}

// TestQuotaOneBlockOver is the append case: the workspace's own budget is the
// boundary, so appending past it is refused rather than spending a reserved
// margin.
func TestQuotaOneBlockOver(t *testing.T) {
	fs := newFs(t)
	fs.SetDiskLimit(64)
	if err := fs.Write("/a", []byte(strings.Repeat("x", 60)), false, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// 60 + 5 = 65 > 64: refused.
	var qe *QuotaError
	if err := fs.Write("/a", []byte("bbbbb"), true, WriteOptions{}); !asQuota(err, &qe) {
		t.Fatalf("append past the limit must be a QuotaError, got %v", err)
	}
	// 60 + 4 = 64: exactly at the limit, allowed.
	if err := fs.Write("/a", []byte("dddd"), true, WriteOptions{}); err != nil {
		t.Fatalf("append landing exactly on the limit must succeed: %v", err)
	}
	if fs.Used() != 64 {
		t.Fatalf("used = %d, want 64", fs.Used())
	}
}

func TestOversizeWriteQuotaOrder(t *testing.T) {
	fs := newFs(t)
	fs.SetDiskLimit(10)
	// Over maxBytes AND over quota: maxBytes checked first.
	err := fs.Write("/a", []byte(strings.Repeat("x", 20)), false, WriteOptions{MaxBytes: 5})
	if !IsTooLarge(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	fs := newFs(t)
	fs.Write("/a", []byte("12345"), false, WriteOptions{})
	fs.Mkdir("/d")
	fs.Write("/d/b", []byte("678"), false, WriteOptions{})
	used, err := fs.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if used != 8 {
		t.Fatalf("used = %d", used)
	}
}

func TestSymlinkNotFollowedInDiskUsage(t *testing.T) {
	root := t.TempDir()
	fs, _ := NewFs(root)
	fs.Write("/a", []byte("12345"), false, WriteOptions{})
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	used, _ := fs.DiskUsage()
	if used != 5 {
		t.Fatalf("used = %d (symlink should be ignored)", used)
	}
}

func TestDiskUsageBoundedStops(t *testing.T) {
	fs := newFs(t)
	fs.Write("/a", []byte("12345"), false, WriteOptions{})
	fs.Write("/b", []byte("678"), false, WriteOptions{})

	size, complete, err := fs.DiskUsageBounded(100)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || size != 8 {
		t.Fatalf("size = %d complete = %v, want 8/true", size, complete)
	}

	size, complete, err = fs.DiskUsageBounded(1)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("bounded walk should be incomplete with maxEntries=1")
	}
	if size > 8 {
		t.Fatalf("bounded size = %d, want a lower bound", size)
	}
}

func asQuota(err error, target **QuotaError) bool {
	qe, ok := err.(*QuotaError)
	if ok {
		*target = qe
	}
	return ok
}

func TestListSortedDirsFirstWithMetadata(t *testing.T) {
	fs := newFs(t)
	fs.Write("/b.txt", []byte("bb"), false, WriteOptions{})
	fs.Write("/a.txt", []byte("a"), false, WriteOptions{})
	fs.Mkdir("/zeta")
	fs.Mkdir("/alpha")
	entries, err := fs.List("/")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = e.Type + ":" + e.Name
	}
	want := []string{"dir:alpha", "dir:zeta", "file:a.txt", "file:b.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v want %v", got, want)
	}
	for _, e := range entries {
		if e.Mode == "" {
			t.Fatalf("entry %q has no mode", e.Name)
		}
	}
	// Directory size is reported as 0.
	if entries[0].Size != 0 {
		t.Fatalf("dir size = %d want 0", entries[0].Size)
	}
}

func TestListPageTruncation(t *testing.T) {
	fs := newFs(t)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		fs.Write("/"+n, []byte("x"), false, WriteOptions{})
	}
	page, err := fs.ListPage("/", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Count != 2 || page.Total != 5 || !page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	if page.Entries[0].Name != "a" || page.Entries[1].Name != "b" {
		t.Fatalf("entries = %+v", page.Entries)
	}
	page2, err := fs.ListPage("/", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page2.Entries[0].Name != "c" || page2.Count != 2 {
		t.Fatalf("page2 = %+v", page2.Entries)
	}
	last, err := fs.ListPage("/", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if last.Count != 1 || last.Truncated {
		t.Fatalf("last = %+v", last)
	}
}

func TestListSymlinkTarget(t *testing.T) {
	fs := newFs(t)
	fs.Write("/real.txt", []byte("x"), false, WriteOptions{})
	if err := os.Symlink("real.txt", filepath.Join(fs.Root(), "inside")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(fs.Root(), "outside")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	entries, err := fs.List("/")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e := byName["inside"]; e.Type != "symlink" || e.Target != "real.txt" || !e.TargetInside {
		t.Fatalf("inside entry = %+v", e)
	}
	if e := byName["outside"]; e.Type != "symlink" || e.TargetInside {
		t.Fatalf("outside entry = %+v", e)
	}
}

func TestReadRangeHonestHash(t *testing.T) {
	fs := newFs(t)
	content := []byte("0123456789abcdef")
	fs.Write("/f", content, false, WriteOptions{})

	full, err := fs.ReadRange("/f", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !full.WholeFile || full.Truncated {
		t.Fatalf("full = %+v", full)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(content)); full.SHA256 != got {
		t.Fatalf("full hash = %s want %s", full.SHA256, got)
	}

	part, err := fs.ReadRange("/f", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	if string(part.Data) != "456789" || part.Offset != 4 || part.Length != 6 {
		t.Fatalf("part = %q off=%d len=%d", part.Data, part.Offset, part.Length)
	}
	if part.WholeFile {
		t.Fatal("partial read must not be whole_file")
	}
	wantPart := fmt.Sprintf("%x", sha256.Sum256([]byte("456789")))
	if part.SHA256 != wantPart {
		t.Fatalf("partial hash = %s want %s", part.SHA256, wantPart)
	}
	if part.SHA256 == full.SHA256 {
		t.Fatal("partial hash must differ from full hash")
	}
}

func TestReadRangeBoundaries(t *testing.T) {
	fs := newFs(t)
	// Empty file.
	fs.Write("/empty", []byte{}, false, WriteOptions{})
	empty, err := fs.ReadRange("/empty", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Data) != 0 || !empty.WholeFile || empty.Truncated || empty.Total != 0 {
		t.Fatalf("empty = %+v", empty)
	}
	// One byte over the cap.
	fs.Write("/six", []byte("abcdef"), false, WriteOptions{})
	over, err := fs.ReadRange("/six", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(over.Data) != 5 || !over.Truncated || over.WholeFile {
		t.Fatalf("over = %+v", over)
	}
	// Exactly at the cap.
	exact, err := fs.ReadRange("/six", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(exact.Data) != "bcdef" || !exact.Truncated || exact.WholeFile {
		t.Fatalf("exact = %+v", exact)
	}
}

func TestReadLines(t *testing.T) {
	fs := newFs(t)
	fs.Write("/f.txt", []byte("one\ntwo\nthree\nfour\n"), false, WriteOptions{})
	res, err := fs.ReadLines("/f.txt", 2, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "two\nthree\n" || res.StartLine != 2 || res.EndLine != 3 || res.TotalLines != 4 {
		t.Fatalf("res = %+v data=%q", res, res.Data)
	}
	// Open-ended range to end of file.
	rest, err := fs.ReadLines("/f.txt", 3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest.Data) != "three\nfour\n" {
		t.Fatalf("rest = %q", rest.Data)
	}
	// No trailing newline counts as a line.
	fs.Write("/noeol", []byte("a\nb"), false, WriteOptions{})
	n, err := fs.ReadLines("/noeol", 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(n.Data) != "a\nb" || n.TotalLines != 2 {
		t.Fatalf("noeol = %+v data=%q", n, n.Data)
	}
	// Empty file has zero lines.
	fs.Write("/e", []byte{}, false, WriteOptions{})
	e, err := fs.ReadLines("/e", 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.TotalLines != 0 || len(e.Data) != 0 {
		t.Fatalf("empty lines = %+v", e)
	}
	// Invalid range.
	if _, err := fs.ReadLines("/f.txt", 3, 1, 0); !IsRangeError(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestListAndReadNotDirectory(t *testing.T) {
	fs := newFs(t)
	fs.Write("/f.txt", []byte("x"), false, WriteOptions{})
	if _, err := fs.List("/f.txt"); !IsNotDirectory(err) {
		t.Fatalf("list err = %v", err)
	}
	if _, err := fs.ReadRange("/missing", 0, 0); !IsNotFound(err) {
		t.Fatalf("read missing err = %v", err)
	}
}

func TestReadDirectoryNotDirectory(t *testing.T) {
	fs := newFs(t)
	fs.Mkdir("/d")
	if _, err := fs.ReadRange("/d", 0, 10); !IsNotDirectory(err) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := fs.Open("/d"); !IsNotDirectory(err) {
		t.Fatalf("open dir err = %v", err)
	}
}

func TestWriteOverDirectoryIsDirectory(t *testing.T) {
	fs := newFs(t)
	fs.Mkdir("/d")
	err := fs.Write("/d", []byte("x"), false, WriteOptions{})
	if !IsIsDirectory(err) {
		t.Fatalf("err = %v want is-directory", err)
	}
}

func TestStatKindsAndBinary(t *testing.T) {
	fs := newFs(t)
	fs.Write("/f.txt", []byte("hello"), false, WriteOptions{})
	fs.Write("/bin", []byte{0x00, 0x01, 0xff}, false, WriteOptions{})
	fs.Mkdir("/d")

	fi, err := fs.Stat("/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Type != "file" || fi.Size != 5 || fi.SHA256 == "" || fi.Binary || fi.Mode == "" {
		t.Fatalf("stat file = %+v", fi)
	}
	bd, err := fs.Stat("/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bd.Binary {
		t.Fatalf("binary not detected: %+v", bd)
	}
	di, err := fs.Stat("/d")
	if err != nil {
		t.Fatal(err)
	}
	if di.Type != "dir" || di.SHA256 != "" {
		t.Fatalf("stat dir = %+v", di)
	}
	if _, err := fs.Stat("/missing"); !IsNotFound(err) {
		t.Fatalf("stat missing err = %v", err)
	}
}
