package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	p := filepath.Join(append([]string{"testdata"}, parts...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return b
}

func rarOpts() Options {
	o := optsFor()
	o.Formats = []Format{FormatRAR}
	return o
}

func TestExtractRAR4Subdirs(t *testing.T) {
	data := readFixture(t, "rar4", "subdirs.rar")
	if f, _ := Detect(data); f != FormatRAR {
		t.Fatalf("detected %q want rar", f)
	}
	sink := newMemSink()
	res, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
	if err != nil {
		t.Fatalf("rar4 extract: %v", err)
	}
	if res.EntriesRejected != 0 {
		t.Fatalf("rejections: %+v", res.Entries)
	}
	want := map[string]string{
		"sub/dir1/file1.txt":         "file1\n",
		"sub/dir2/file2.txt":         "file2\n",
		"sub/with space/long fn.txt": "long fn\n",
		"sub/üȵĩöḋè/file.txt":        "file\n",
	}
	for name, body := range want {
		if string(sink.files[name]) != body {
			t.Errorf("%s = %q want %q", name, sink.files[name], body)
		}
	}
	if !sink.dirs["sub/empty"] {
		t.Error("empty dir sub/empty not created")
	}
}

func TestExtractRAR5Subdirs(t *testing.T) {
	data := readFixture(t, "rar5", "subdirs.rar")
	if f, _ := Detect(data); f != FormatRAR {
		t.Fatalf("detected %q want rar", f)
	}
	sink := newMemSink()
	res, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
	if err != nil {
		t.Fatalf("rar5 extract: %v", err)
	}
	if res.EntriesRejected != 0 {
		t.Fatalf("rejections: %+v", res.Entries)
	}
	if string(sink.files["sub/dir1/file1.txt"]) != "file1\n" {
		t.Errorf("rar5 file content wrong: %q", sink.files["sub/dir1/file1.txt"])
	}
}

func TestExtractRARSolid(t *testing.T) {
	// Solid entries must still be read in order; the header pass must not
	// consume the data the second pass needs.
	for _, fam := range []string{"rar4", "rar5"} {
		data := readFixture(t, fam, "solid.rar")
		sink := newMemSink()
		if _, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false); err != nil {
			t.Fatalf("%s solid extract: %v", fam, err)
		}
		if len(sink.files) != 2 {
			t.Errorf("%s solid: %d files want 2", fam, len(sink.files))
		}
	}
}

func TestExtractRAREncrypted(t *testing.T) {
	for _, fam := range []string{"rar4", "rar5"} {
		data := readFixture(t, fam, "encrypted.rar")
		sink := newMemSink()
		_, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != model.CodeArchiveEncrypted {
			t.Fatalf("%s encrypted: want archive_encrypted, got %v", fam, err)
		}
		if len(sink.files) != 0 {
			t.Errorf("%s encrypted wrote files", fam)
		}
	}
}

func TestExtractRARMultivolume(t *testing.T) {
	data := readFixture(t, "rar5", "multivolume.part1.rar")
	sink := newMemSink()
	_, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveMultivolume {
		t.Fatalf("want archive_multivolume, got %v", err)
	}
}

func TestExtractRARSymlinkRefused(t *testing.T) {
	data := readFixture(t, "rar5", "evil-symlink.rar")
	sink := newMemSink()
	_, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveUnsafeEntry {
		t.Fatalf("want archive_unsafe_entry, got %v", err)
	}
	if len(sink.files) != 0 {
		t.Errorf("nothing should be written: %v", sink.files)
	}
}

func TestExtractRARHardlinkRefused(t *testing.T) {
	data := readFixture(t, "rar5", "hardlink.rar")
	sink := newMemSink()
	_, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveUnsafeEntry {
		t.Fatalf("want archive_unsafe_entry, got %v", err)
	}
}

func TestExtractRARDryRun(t *testing.T) {
	data := readFixture(t, "rar5", "subdirs.rar")
	sink := newMemSink()
	res, err := Extract(data, rarOpts(), "", sink, workspace.CleanRelative, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !res.DryRun || res.BytesWritten != 0 || len(sink.files) != 0 {
		t.Errorf("dry run wrote: %+v files=%v", res, sink.files)
	}
	if res.EntriesTotal == 0 {
		t.Error("dry run should list entries")
	}
}

func TestWriteZipRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Build a small tree with a nested dir and a symlink (which must be
	// skipped, never emitted).
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "x.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}

	var buf writeBuf
	er, err := WriteZip(&buf, dir, true)
	if err != nil {
		t.Fatalf("WriteZip: %v", err)
	}
	if er.Skipped != 1 {
		t.Errorf("symlink should be skipped, skipped=%d", er.Skipped)
	}

	// Extract it back and compare.
	sink := newMemSink()
	if _, err := Extract(buf.b, optsFor(), "", sink, workspace.CleanRelative, false); err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	if string(sink.files["a/b/x.txt"]) != "hello\n" || string(sink.files["top.txt"]) != "top\n" {
		t.Errorf("round trip mismatch: %v", sink.files)
	}
	if _, ok := sink.files["evil"]; ok {
		t.Error("symlink must not survive an export")
	}
}

type writeBuf struct{ b []byte }

func (w *writeBuf) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}
