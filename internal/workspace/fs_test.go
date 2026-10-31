package workspace

import (
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
