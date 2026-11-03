package archive

import (
	"io/fs"
	"testing"

	"sandboxd/internal/workspace"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want Format
	}{
		{"zip local", []byte{'P', 'K', 3, 4, 0}, FormatZip},
		{"zip empty", []byte{'P', 'K', 5, 6, 0}, FormatZip},
		{"zip spanned", []byte{'P', 'K', 7, 8, 0}, FormatZip},
		{"rar4", []byte{'R', 'a', 'r', '!', 0x1a, 0x07, 0x00, 0}, FormatRAR},
		{"rar5", []byte{'R', 'a', 'r', '!', 0x1a, 0x07, 0x01, 0x00}, FormatRAR},
		{"empty", nil, ""},
		{"random", []byte{0xde, 0xad, 0xbe, 0xef}, ""},
		{"truncated rar sig", []byte{'R', 'a', 'r', '!'}, ""},
	}
	for _, c := range cases {
		got, sig := Detect(c.data)
		if got != c.want {
			t.Errorf("%s: Detect=%q want %q (sig=%s)", c.name, got, c.want, sig)
		}
	}
}

func TestSanitizeMode(t *testing.T) {
	cases := []struct {
		in   fs.FileMode
		want fs.FileMode
	}{
		{0o4755, 0o755},                // setuid stripped
		{0o2755, 0o755},                // setgid stripped
		{0o1777, 0o777},                // sticky stripped
		{0o000, 0o644},                 // nothing usable -> safe default
		{0o644, 0o644},                 // unchanged
		{fs.ModeSetuid | 0o600, 0o600}, // setuid + perm
		{fs.ModeSetgid | fs.ModeSticky | 0o700, 0o700},
	}
	for _, c := range cases {
		got := SanitizeMode(c.in)
		if got != c.want {
			t.Errorf("SanitizeMode(%o)=%o want %o", c.in, got, c.want)
		}
		if got&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Errorf("SanitizeMode(%o) kept a privileged bit: %o", c.in, got)
		}
	}
}

func TestBuildPlanUnsafeNames(t *testing.T) {
	clean := workspace.CleanRelative
	opts := Options{MaxEntries: 100, MaxTotalBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxRatio: 200, SkipJunk: true, Formats: []Format{FormatZip}}

	raw := []rawEntry{
		{Name: "ok.txt", Kind: KindFile, Mode: 0o644, Uncompressed: 5, Compressed: 5, SizeKnown: true},
		{Name: "../escape.txt", Kind: KindFile, Mode: 0o644, Uncompressed: 5, Compressed: 5, SizeKnown: true},
		{Name: "/abs.txt", Kind: KindFile, Mode: 0o644, Uncompressed: 5, Compressed: 5, SizeKnown: true},
		{Name: "C:/drive.txt", Kind: KindFile, Mode: 0o644, Uncompressed: 5, Compressed: 5, SizeKnown: true},
		{Name: "link", Kind: KindSymlink, LinkTarget: "/etc/passwd", Mode: 0o777, SizeKnown: true},
		{Name: "hard", Kind: KindHardlink, LinkTarget: "../../etc/shadow", SizeKnown: true},
		{Name: "__MACOSX/._x", Kind: KindFile, Mode: 0o644, Uncompressed: 5, Compressed: 5, SizeKnown: true},
	}

	plan := buildPlan(raw, opts, "", clean)
	if len(plan) != len(raw) {
		t.Fatalf("plan len=%d want %d", len(plan), len(raw))
	}

	byName := map[string]planEntry{}
	for _, pe := range plan {
		byName[pe.raw.Name] = pe
	}

	if pe := byName["ok.txt"]; pe.reject != "" {
		t.Errorf("ok.txt rejected: %s", pe.reject)
	}
	for _, name := range []string{"../escape.txt", "/abs.txt", "C:/drive.txt", "link", "hard"} {
		pe := byName[name]
		if pe.reject != "archive_unsafe_entry" {
			t.Errorf("%s: reject=%q want archive_unsafe_entry", name, pe.reject)
		}
	}
	if pe := byName["__MACOSX/._x"]; !pe.entry.Skipped || pe.reject != "" {
		t.Errorf("junk not skipped cleanly: %+v", pe)
	}
}

func TestBuildPlanCollision(t *testing.T) {
	clean := workspace.CleanRelative
	opts := Options{MaxFileBytes: 1 << 20, MaxRatio: 200}
	raw := []rawEntry{
		{Name: "a/b.txt", Kind: KindFile, Uncompressed: 1, Compressed: 1, SizeKnown: true},
		{Name: "a/../a/b.txt", Kind: KindFile, Uncompressed: 1, Compressed: 1, SizeKnown: true},
	}
	plan := buildPlan(raw, opts, "", clean)
	if len(plan) != 2 {
		t.Fatalf("plan len=%d", len(plan))
	}
	if plan[0].reject != "" {
		t.Errorf("first entry rejected: %s", plan[0].reject)
	}
	if plan[1].reject != "archive_unsafe_entry" {
		t.Errorf("collision not refused: %q", plan[1].reject)
	}
}

func TestBuildPlanCaps(t *testing.T) {
	clean := workspace.CleanRelative
	opts := Options{MaxFileBytes: 10, MaxRatio: 2}

	// Per-entry size over cap.
	plan := buildPlan([]rawEntry{{Name: "big", Kind: KindFile, Uncompressed: 11, Compressed: 6, SizeKnown: true}}, opts, "", clean)
	if plan[0].reject != "archive_too_large" {
		t.Errorf("size cap: %q", plan[0].reject)
	}
	// Ratio over cap: 100/1 = 100 > 2 (keep the entry under the size cap so
	// the ratio rule is what fires).
	ratioOpts := Options{MaxFileBytes: 1 << 20, MaxRatio: 2}
	plan = buildPlan([]rawEntry{{Name: "bomb", Kind: KindFile, Uncompressed: 100, Compressed: 1, SizeKnown: true}}, ratioOpts, "", clean)
	if plan[0].reject != "archive_ratio_exceeded" {
		t.Errorf("ratio cap: %q", plan[0].reject)
	}
	// Exactly at the size cap: allowed (boundary).
	plan = buildPlan([]rawEntry{{Name: "exact", Kind: KindFile, Uncompressed: 10, Compressed: 5, SizeKnown: true}}, opts, "", clean)
	if plan[0].reject != "" {
		t.Errorf("exactly at size cap should be allowed, got %q", plan[0].reject)
	}
}

func TestRatioBoundary(t *testing.T) {
	// exactly maxRatio allowed, one over refused.
	if ratioExceeded(200, 1, 200) {
		t.Error("ratio exactly at cap should be allowed")
	}
	if !ratioExceeded(201, 1, 200) {
		t.Error("ratio one over cap should be refused")
	}
	if ratioExceeded(0, 0, 200) {
		t.Error("zero-size entry is not a ratio violation")
	}
	if !ratioExceeded(1, 0, 200) {
		t.Error("non-zero uncompressed with zero compressed should be refused")
	}
}

func TestHasDriveOrAbsolute(t *testing.T) {
	abs := []string{"/x", "\\x", "C:/x", "c:\\x", "//host/share"}
	for _, s := range abs {
		if !hasDriveOrAbsolute(s) {
			t.Errorf("%q should be absolute/drive", s)
		}
	}
	rel := []string{"a/b", "./a", "a/b:c"}
	for _, s := range rel {
		if hasDriveOrAbsolute(s) {
			t.Errorf("%q should be relative", s)
		}
	}
	// A single-letter-colon prefix is refused conservatively even when it is not
	// a real drive path: refusing is safer than a special case.
	if !hasDriveOrAbsolute("a:notadrive") {
		t.Error("single-letter colon prefix should be refused")
	}
}
