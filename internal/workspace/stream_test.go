package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestFs(t *testing.T) *Fs {
	t.Helper()
	dir := t.TempDir()
	fs, err := NewFs(dir)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	return fs
}

func TestCreateStreamWritesAndStripsPrivilegedBits(t *testing.T) {
	fs := newTestFs(t)
	w, err := fs.CreateStream("a/b.txt", 0o4755, false) // setuid must be stripped
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(fs.Root(), "a", "b.txt"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("privileged bit survived: %o", fi.Mode())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("perm=%o want 755", fi.Mode().Perm())
	}
}

func TestCreateStreamRefusesSymlinkAtDestination(t *testing.T) {
	fs := newTestFs(t)
	// An existing symlink inside the workspace pointing OUTSIDE it.
	outside := t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fs.Root(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Writing to "link" must NOT follow it (O_NOFOLLOW), so the outside file is
	// never written through.
	w, err := fs.CreateStream("link", 0o644, true)
	if err == nil {
		_, _ = w.Write([]byte("pwned"))
		_ = w.Close()
		t.Fatal("writing through a symlink should be refused")
	}
	if !IsUnsafe(err) && !errors.Is(err, ErrExists) {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "original" {
		t.Fatalf("outside file was modified: %q", data)
	}
}

func TestCreateStreamRefusesSymlinkedParentEscape(t *testing.T) {
	fs := newTestFs(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(fs.Root(), "esc")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.CreateStream("esc/pwn.txt", 0o644, false); err == nil {
		t.Fatal("write through an escaping parent symlink should be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); err == nil {
		t.Fatal("a file was written outside the workspace")
	}
}

func TestCreateStreamTraversalRefused(t *testing.T) {
	fs := newTestFs(t)
	if _, err := fs.CreateStream("../escape.txt", 0o644, false); err == nil {
		t.Fatal("traversal should be refused")
	}
}

func TestCreateStreamQuota(t *testing.T) {
	fs := newTestFs(t)
	fs.SetDiskLimit(10)
	w, err := fs.CreateStream("f.bin", 0o644, false)
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	if _, err := w.Write(make([]byte, 20)); err == nil {
		t.Fatal("write past the quota should fail")
	}
	_ = w.Close()
}

func TestCreateStreamOverwriteSemantics(t *testing.T) {
	fs := newTestFs(t)
	w, _ := fs.CreateStream("f.txt", 0o644, false)
	_, _ = w.Write([]byte("first"))
	_ = w.Close()

	// overwrite=false -> ErrExists.
	if _, err := fs.CreateStream("f.txt", 0o644, false); err == nil {
		t.Fatal("expected ErrExists")
	} else if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	// overwrite=true -> replaces.
	w2, err := fs.CreateStream("f.txt", 0o644, true)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	_, _ = w2.Write([]byte("second"))
	_ = w2.Close()
	data, _ := os.ReadFile(filepath.Join(fs.Root(), "f.txt"))
	if string(data) != "second" {
		t.Fatalf("content=%q", data)
	}
}

func TestMkdirStream(t *testing.T) {
	fs := newTestFs(t)
	if err := fs.MkdirStream("x/y/z", 0o755); err != nil {
		t.Fatalf("MkdirStream: %v", err)
	}
	fi, err := os.Stat(filepath.Join(fs.Root(), "x", "y", "z"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// Idempotent.
	if err := fs.MkdirStream("x/y/z", 0o755); err != nil {
		t.Fatalf("MkdirStream idempotent: %v", err)
	}
}
