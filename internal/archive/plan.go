package archive

import (
	"io/fs"
	"strings"
)

// Options configures an extraction. It mirrors the config `archives:` section.
type Options struct {
	MaxEntries    int
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxRatio      int
	Overwrite     bool
	SkipJunk      bool
	Formats       []Format
}

// FormatAllowed reports whether a detected format is enabled by the allowlist.
func (o Options) FormatAllowed(f Format) bool {
	for _, a := range o.Formats {
		if a == f {
			return true
		}
	}
	return false
}

// EntryKind classifies an archive member.
type EntryKind string

// Entry kinds.
const (
	KindFile     EntryKind = "file"
	KindDir      EntryKind = "dir"
	KindSymlink  EntryKind = "symlink"
	KindHardlink EntryKind = "hardlink"
)

// rawEntry is one parsed archive header, before validation.
type rawEntry struct {
	Name         string
	Kind         EntryKind
	LinkTarget   string
	Mode         fs.FileMode
	Uncompressed int64
	Compressed   int64
	SizeKnown    bool
	Encrypted    bool
}

// Entry is a validated (or rejected) archive member, as reported to a client.
type Entry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       EntryKind `json:"type"`
	Size       int64     `json:"size"`
	Compressed int64     `json:"compressed_size,omitempty"`
	Mode       string    `json:"mode"`
	// Rejected is true when the entry may not be written (unsafe name, escaping
	// link, collision, or a per-entry cap). Reason is then a stable code.
	Rejected bool   `json:"rejected"`
	Reason   string `json:"reason,omitempty"`
	// Skipped is true when the entry was intentionally not extracted (macOS
	// resource-fork junk when skip_junk is on). SkipReason is informational.
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// planEntry carries the rawEntry alongside its decision and cleaned rel path.
type planEntry struct {
	raw    rawEntry
	rel    string // cleaned, slash path relative to destination ("" for root)
	entry  Entry
	reject string // stable code; "" = ok
}

// buildPlan validates the whole header list BEFORE a single byte is written.
// It returns the per-entry decisions. Archive-level caps (entry count, total
// size) are enforced by the caller during iteration because they bound the
// iteration itself.
//
// Every name goes through workspace.CleanRelative (via caller-supplied clean),
// never a second validator. Absolute names, drive letters and escaping `..`
// surface as errTraversal/IsUnsafe there. Rejected entries are returned flagged
// rather than dropped, so dry_run can show exactly what would be refused.
func buildPlan(raw []rawEntry, opts Options, destRel string, clean func(string) (string, error)) []planEntry {
	seen := make(map[string]bool, len(raw))
	out := make([]planEntry, 0, len(raw))
	for _, r := range raw {
		pe := planEntry{raw: r}
		pe.entry = Entry{
			Name:       r.Name,
			Size:       r.Uncompressed,
			Compressed: r.Compressed,
			Type:       r.Kind,
			Mode:       modeString(r.Mode, r.Kind == KindDir),
		}
		// macOS resource-fork / metadata junk: skipped by default, before any
		// safety decision (it is never written, so it cannot be unsafe).
		if opts.SkipJunk && isJunk(r.Name) {
			pe.entry.Skipped = true
			pe.entry.SkipReason = "junk"
			pe.entry.Path = joinDest(destRel, r.Name)
			out = append(out, pe)
			continue
		}
		// Symlinks and hardlinks are REFUSED outright (see ADR 92): the engine
		// refuses by default rather than trying to decide a target is safe, and
		// never materializes a link it does not have to.
		if r.Kind == KindSymlink || r.Kind == KindHardlink {
			pe.reject = "archive_unsafe_entry"
		}
		// Validate the name through the workspace's own cleaner. It strips
		// leading slashes and normalises backslashes; errTraversal surfaces an
		// escaping `..` (reported by workspace.IsUnsafe).
		rel, err := clean(r.Name)
		if err != nil {
			pe.reject = "archive_unsafe_entry"
			rel = ""
		}
		// Reject an absolute name or a drive letter explicitly: the cleaner
		// strips a leading slash (correct for the API path model) but an
		// archive entry must not silently accept an absolute path.
		if hasDriveOrAbsolute(r.Name) {
			pe.reject = "archive_unsafe_entry"
		}
		pe.rel = rel
		pe.entry.Path = joinDest(destRel, rel)
		if rel == "" && pe.reject == "" {
			// An entry that cleans to the destination root is a no-op (e.g.
			// "./"); treat as junk, not a file.
			pe.entry.Skipped = true
			pe.entry.SkipReason = "root"
		}
		// Intra-archive collision: two entries normalising to the same path.
		// Refusing is safer than last-wins. Case-sensitive (the workspace fs
		// is case-sensitive); documented in ADR 92.
		if pe.reject == "" && rel != "" {
			if seen[rel] {
				pe.reject = "archive_unsafe_entry"
			} else {
				seen[rel] = true
			}
		}
		// Per-entry caps (declared sizes; the streaming budget is the real
		// guard during writing).
		if pe.reject == "" && r.Kind == KindFile {
			if r.SizeKnown && opts.MaxFileBytes > 0 && r.Uncompressed > opts.MaxFileBytes {
				pe.reject = "archive_too_large"
			}
			if pe.reject == "" && opts.MaxRatio > 0 && ratioExceeded(r.Uncompressed, r.Compressed, opts.MaxRatio) {
				pe.reject = "archive_ratio_exceeded"
			}
		}
		out = append(out, pe)
	}
	return out
}

// joinDest renders a destination + relative entry path as an absolute,
// slash-rooted workspace path for reporting.
func joinDest(destRel, rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(rel, "/")
	base := "/"
	if destRel != "" {
		base = "/" + strings.Trim(destRel, "/")
	}
	if rel == "" || rel == "." {
		return base
	}
	return base + "/" + strings.Trim(rel, "/")
}

// hasDriveOrAbsolute reports whether an archive entry name is absolute or
// carries a Windows drive letter. Both are refused even though the cleaner
// would strip a leading slash (the archive must not look like it accepted an
// absolute path).
func hasDriveOrAbsolute(name string) bool {
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") {
		return true
	}
	// UNC path \\host\share (already slash-normalised to //host/share).
	if strings.HasPrefix(n, "//") {
		return true
	}
	if len(n) >= 2 && n[1] == ':' {
		c := n[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// isJunk reports whether an entry is macOS resource-fork / metadata noise that
// is skipped by default.
func isJunk(name string) bool {
	n := strings.ReplaceAll(name, "\\", "/")
	if n == "__MACOSX" || strings.HasPrefix(n, "__MACOSX/") {
		return true
	}
	base := n
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == ".DS_Store" || base == "Thumbs.db" || base == "desktop.ini" {
		return true
	}
	// AppleDouble sidecar files ("." prefix) anywhere in the tree.
	if strings.HasPrefix(base, "._") {
		return true
	}
	return false
}

// ratioExceeded reports whether uncompressed/compressed exceeds maxRatio. A
// non-zero uncompressed with a zero/absent compressed size is treated as an
// infinite ratio (refused): a stored entry never has that shape.
func ratioExceeded(uncompressed, compressed int64, maxRatio int) bool {
	if uncompressed <= 0 {
		return false
	}
	if compressed <= 0 {
		return true
	}
	return uncompressed > compressed*int64(maxRatio)
}

// SanitizeMode strips setuid, setgid and sticky bits from an extracted mode and
// forces a safe default when nothing usable remains. It NEVER returns a
// privileged bit. The mode is only advisory (the fuse mount maps ownership to
// the sandbox uid); the point is that no archive can request a privileged file.
func SanitizeMode(mode fs.FileMode) fs.FileMode {
	perm := mode.Perm() // perm bits only; drops setuid/setgid/sticky by definition
	if perm == 0 {
		return 0o644
	}
	return perm
}

// modeString renders a mode for reporting, dropping privileged bits.
func modeString(mode fs.FileMode, isDir bool) string {
	perm := SanitizeMode(mode)
	var b strings.Builder
	if isDir {
		b.WriteByte('d')
	} else {
		b.WriteByte('-')
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if perm&(1<<uint(8-i)) != 0 {
			b.WriteByte(rwx[i])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
