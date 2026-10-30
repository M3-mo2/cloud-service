package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanRelative(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", ".", false},
		{"/", ".", false},
		{"main.py", "main.py", false},
		{"/main.py", "main.py", false},
		{"/src/a/b.txt", "src/a/b.txt", false},
		{"src/../main.py", "main.py", false},
		{"./src/./a", "src/a", false},
		{"a//b", "a/b", false},
		{"../etc/passwd", "", true},
		{"/../etc/passwd", "", true},
		{"a/../../etc/passwd", "", true},
		{"..", "", true},
	}
	for _, c := range cases {
		got, err := cleanRelative(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("cleanRelative(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("cleanRelative(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("cleanRelative(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	fs, err := NewFs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../etc/passwd", "/../etc/passwd", "a/../../x", ".."} {
		_, err := fs.Resolve(p)
		if err == nil || !IsUnsafe(err) {
			t.Errorf("Resolve(%q) err = %v, want unsafe", p, err)
		}
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	fs, err := NewFs(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Resolve("/escape"); err == nil || !IsUnsafe(err) {
		t.Fatalf("expected symlink escape rejection, got %v", err)
	}
}

func TestResolveSymlinkInsideAllowed(t *testing.T) {
	root := t.TempDir()
	fs, _ := NewFs(root)
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	abs, err := fs.Resolve("/link/a.txt")
	if err != nil {
		t.Fatalf("inside symlink should resolve: %v", err)
	}
	if !strings.HasPrefix(abs, fs.Root()) {
		t.Fatalf("resolved outside root: %s", abs)
	}
}

func TestSymlinkEscapeViaIntermediateDir(t *testing.T) {
	root := t.TempDir()
	fs, _ := NewFs(root)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fs.Read("/etc/passwd", 1024); err == nil || !IsUnsafe(err) {
		t.Fatalf("expected rejection, got %v", err)
	}
	if err := fs.Write("/etc/passwd", []byte("pwn"), false, WriteOptions{}); err == nil || !IsUnsafe(err) {
		t.Fatalf("expected write rejection, got %v", err)
	}
	if err := fs.Remove("/etc/passwd"); err == nil || !IsUnsafe(err) {
		t.Fatalf("expected remove rejection, got %v", err)
	}
}

func TestPathErrorMessage(t *testing.T) {
	if !strings.Contains(errTraversal.Error(), "escapes") {
		t.Fatalf("unexpected message %q", errTraversal.Error())
	}
}
