package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// memSink is an in-memory Sink for the pure extraction tests.
type memSink struct {
	files map[string][]byte
	dirs  map[string]bool
	// failWrite, when set, is returned by the Nth Open (0-based) to simulate a
	// disk error (ENOSPC -> the service normalizes it; here we return it raw).
	writeErr error
	opens    int
}

func newMemSink() *memSink {
	return &memSink{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (m *memSink) Mkdir(rel string, mode fs.FileMode) error {
	m.dirs[rel] = true
	return nil
}

func (m *memSink) Open(rel string, mode fs.FileMode, overwrite bool) (io.WriteCloser, error) {
	m.opens++
	if m.writeErr != nil {
		return nil, m.writeErr
	}
	if !overwrite {
		if _, ok := m.files[rel]; ok {
			return nil, ErrEntryExists
		}
	}
	return &memWriter{m: m, rel: rel}, nil
}

type memWriter struct {
	m   *memSink
	rel string
	buf bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *memWriter) Close() error {
	w.m.files[w.rel] = append([]byte(nil), w.buf.Bytes()...)
	return nil
}

// buildZip constructs an in-memory zip from a map of name -> content. A name
// ending in "/" is a directory. Names with a mode override (setuid etc.) can be
// added through mods.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		if e.dir {
			hdr.Name = strings.TrimSuffix(e.name, "/") + "/"
			hdr.SetMode(fs.ModeDir | 0o755)
		}
		if e.encrypted {
			hdr.Flags |= 0x1
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("create header: %v", err)
		}
		if !e.dir {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name      string
	body      string
	dir       bool
	mode      fs.FileMode
	encrypted bool
}

func optsFor() Options {
	return Options{
		MaxEntries:    100,
		MaxTotalBytes: 1 << 20,
		MaxFileBytes:  1 << 20,
		MaxRatio:      200,
		SkipJunk:      true,
		Formats:       []Format{FormatZip, FormatRAR},
	}
}

func TestExtractNormalZip(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "src/main.py", body: "print(1)\n"},
		{name: "src/util/", dir: true},
		{name: "src/empty.txt", body: ""},
		{name: "src/uni-📦.txt", body: "unicode"},
		{name: "__MACOSX/._main.py", body: "junk"},
	})
	sink := newMemSink()
	res, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, false)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.EntriesRejected != 0 {
		t.Fatalf("unexpected rejections: %+v", res.Entries)
	}
	if string(sink.files["src/main.py"]) != "print(1)\n" {
		t.Errorf("main.py content wrong: %q", sink.files["src/main.py"])
	}
	if _, ok := sink.files["src/empty.txt"]; !ok {
		t.Error("empty file not written")
	}
	if _, ok := sink.files["src/uni-📦.txt"]; !ok {
		t.Error("unicode file not written")
	}
	if _, ok := sink.files["__MACOSX/._main.py"]; ok {
		t.Error("junk should not be written")
	}
	if res.EntriesSkipped != 1 {
		t.Errorf("junk skip count = %d want 1", res.EntriesSkipped)
	}
	if res.BytesWritten == 0 {
		t.Error("bytes written should be positive")
	}
}

func TestExtractTraversalRefused(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "good.txt", body: "ok"},
		{name: "../escape.txt", body: "pwned"},
	})
	sink := newMemSink()
	_, err := Extract(data, optsFor(), "dest", sink, workspace.CleanRelative, false)
	if err == nil {
		t.Fatal("expected refusal")
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveUnsafeEntry {
		t.Fatalf("want archive_unsafe_entry, got %v", err)
	}
	if len(sink.files) != 0 {
		t.Errorf("nothing should be written on refusal, got %v", sink.files)
	}
}

func TestExtractAbsoluteRefused(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "/etc/passwd", body: "x"}})
	sink := newMemSink()
	_, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveUnsafeEntry {
		t.Fatalf("want archive_unsafe_entry, got %v", err)
	}
}

func TestExtractSymlinkRefused(t *testing.T) {
	// A zip symlink entry: mode ModeSymlink, body = target.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "evil"}
	hdr.SetMode(fs.ModeSymlink | 0o777)
	w, _ := zw.CreateHeader(hdr)
	_, _ = w.Write([]byte("../../etc/passwd"))
	_ = zw.Close()

	sink := newMemSink()
	_, err := Extract(buf.Bytes(), optsFor(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveUnsafeEntry {
		t.Fatalf("want archive_unsafe_entry, got %v", err)
	}
}

func TestExtractDryRunWritesNothing(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "a.txt", body: "aaaa"},
		{name: "../evil", body: "x"},
	})
	sink := newMemSink()
	res, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, true)
	if err != nil {
		t.Fatalf("dry run should not fail: %v", err)
	}
	if !res.DryRun || res.BytesWritten != 0 {
		t.Errorf("dry run wrote bytes: %+v", res)
	}
	if len(sink.files) != 0 || len(sink.dirs) != 0 {
		t.Errorf("dry run wrote to sink: files=%v dirs=%v", sink.files, sink.dirs)
	}
	if res.EntriesRejected != 1 {
		t.Errorf("dry run should report 1 rejected, got %d", res.EntriesRejected)
	}
}

func TestExtractEncryptedRefused(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "secret.txt", body: "x", encrypted: true}})
	sink := newMemSink()
	_, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveEncrypted {
		t.Fatalf("want archive_encrypted, got %v", err)
	}
}

func TestExtractUnsupported(t *testing.T) {
	sink := newMemSink()
	_, err := Extract([]byte("not an archive at all"), optsFor(), "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeUnsupportedArchive {
		t.Fatalf("want unsupported_archive, got %v", err)
	}
}

func TestExtractCorruptNoPanic(t *testing.T) {
	// Valid zip magic, truncated body.
	data := append([]byte{'P', 'K', 3, 4}, []byte("garbage-not-a-real-zip")...)
	sink := newMemSink()
	if _, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, false); err == nil {
		t.Fatal("corrupt archive should error")
	}
}

func TestExtractFormatDisabled(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "a.txt", body: "x"}})
	opts := optsFor()
	opts.Formats = []Format{FormatRAR} // zip disabled
	sink := newMemSink()
	_, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeUnsupportedArchive {
		t.Fatalf("want unsupported_archive, got %v", err)
	}
}

func TestExtractEntryCountCap(t *testing.T) {
	var entries []zipEntry
	for i := 0; i < 5; i++ {
		entries = append(entries, zipEntry{name: string(rune('a'+i)) + ".txt", body: "x"})
	}
	data := buildZip(t, entries)
	opts := optsFor()
	opts.MaxEntries = 3
	sink := newMemSink()
	_, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveTooManyEntries {
		t.Fatalf("want archive_too_many_entries, got %v", err)
	}
}

func TestExtractFileSizeCap(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "big.txt", body: strings.Repeat("A", 1000)}})
	opts := optsFor()
	opts.MaxFileBytes = 100
	sink := newMemSink()
	_, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveTooLarge {
		t.Fatalf("want archive_too_large, got %v", err)
	}
	if len(sink.files) != 0 {
		t.Errorf("nothing should be written on refusal: %v", sink.files)
	}
}

func TestExtractTotalBudget(t *testing.T) {
	// Three 400-byte files; total budget 1000 -> the 3rd would cross it.
	var entries []zipEntry
	for i := 0; i < 3; i++ {
		entries = append(entries, zipEntry{name: string(rune('a'+i)) + ".bin", body: strings.Repeat("Z", 400)})
	}
	data := buildZip(t, entries)
	opts := optsFor()
	opts.MaxTotalBytes = 1000
	sink := newMemSink()
	_, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveTooLarge {
		t.Fatalf("want archive_too_large, got %v", err)
	}
	// The streaming budget means the written bytes never exceed the cap.
	var total int
	for _, b := range sink.files {
		total += len(b)
	}
	if total > 1000 {
		t.Errorf("bytes on disk %d exceeded budget 1000", total)
	}
}

func TestExtractExactTotalBudgetAllowed(t *testing.T) {
	var entries []zipEntry
	for i := 0; i < 2; i++ {
		entries = append(entries, zipEntry{name: string(rune('a'+i)) + ".bin", body: strings.Repeat("Z", 500)})
	}
	data := buildZip(t, entries)
	opts := optsFor()
	opts.MaxTotalBytes = 1000 // exactly the sum
	sink := newMemSink()
	res, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	if err != nil {
		t.Fatalf("exactly at total budget should succeed: %v", err)
	}
	if res.BytesWritten != 1000 {
		t.Errorf("bytes written = %d want 1000", res.BytesWritten)
	}
}

func TestCopyBudgetedBoundaries(t *testing.T) {
	// exactly at cap.
	var used int64
	var out bytes.Buffer
	n, err := copyBudgeted(&out, strings.NewReader("12345"), &used, 5, 5)
	if err != nil || n != 5 || used != 5 {
		t.Fatalf("exactly at cap: n=%d err=%v used=%d", n, err, used)
	}
	// one byte over total.
	used = 0
	out.Reset()
	_, err = copyBudgeted(&out, strings.NewReader("123456"), &used, 5, 100)
	if !errors.Is(err, ErrBudgetTotal) {
		t.Fatalf("one over total: want ErrBudgetTotal, got %v", err)
	}
	if used > 5 {
		t.Errorf("used %d exceeded total 5", used)
	}
	// one byte over entry.
	used = 0
	out.Reset()
	_, err = copyBudgeted(&out, strings.NewReader("123456"), &used, 100, 5)
	if !errors.Is(err, ErrBudgetEntry) {
		t.Fatalf("one over entry: want ErrBudgetEntry, got %v", err)
	}
}

func TestExtractOverwrite(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "a.txt", body: "new"}})

	// overwrite=false with an existing file -> archive_entry_exists.
	sink := newMemSink()
	sink.files["a.txt"] = []byte("old")
	opts := optsFor()
	opts.Overwrite = false
	_, err := Extract(data, opts, "", sink, workspace.CleanRelative, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != model.CodeArchiveEntryExists {
		t.Fatalf("want archive_entry_exists, got %v", err)
	}
	if string(sink.files["a.txt"]) != "old" {
		t.Error("existing file must be untouched on refusal")
	}

	// overwrite=true replaces it.
	sink2 := newMemSink()
	sink2.files["a.txt"] = []byte("old")
	opts.Overwrite = true
	if _, err := Extract(data, opts, "", sink2, workspace.CleanRelative, false); err != nil {
		t.Fatalf("overwrite extract: %v", err)
	}
	if string(sink2.files["a.txt"]) != "new" {
		t.Errorf("overwrite did not replace: %q", sink2.files["a.txt"])
	}
}

func TestExtractDiskErrorPassthrough(t *testing.T) {
	data := buildZip(t, []zipEntry{{name: "a.txt", body: "x"}})
	sink := newMemSink()
	diskErr := &workspace.QuotaError{Used: 1, Limit: 1}
	sink.writeErr = diskErr
	_, err := Extract(data, optsFor(), "", sink, workspace.CleanRelative, false)
	if !errors.Is(err, diskErr) {
		t.Fatalf("disk error should pass through untouched, got %v", err)
	}
	var ae *Error
	if errors.As(err, &ae) {
		t.Errorf("disk error must NOT be an archive refusal: %v", ae)
	}
}
