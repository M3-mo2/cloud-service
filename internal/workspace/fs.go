package workspace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"sandboxd/internal/model"
)

// Entry describes a directory child.
//
// Mode is the octal permission string (e.g. "0644"). Target is set only for
// symlinks and is the raw link target; TargetInside reports whether that target
// resolves inside the workspace root (false when it escapes or is dangling and
// lexically outside). Runnable names the interpreter that runs a regular file
// by its extension (empty when no interpreter is registered for it).
type Entry struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Type         string    `json:"type"`
	Size         int64     `json:"size"`
	Mode         string    `json:"mode"`
	ModTime      time.Time `json:"mod_time"`
	Target       string    `json:"target,omitempty"`
	TargetInside bool      `json:"target_inside,omitempty"`
	Runnable     string    `json:"runnable,omitempty"`
}

// Listing is one bounded page of a directory listing.
type Listing struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Count     int     `json:"count"`
	Total     int     `json:"total"`
	Truncated bool    `json:"truncated"`
}

// FileInfo describes a file (or symlink/directory) for metadata reads.
type FileInfo struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Size         int64     `json:"size"`
	Mode         string    `json:"mode"`
	ModTime      time.Time `json:"mod_time"`
	ContentType  string    `json:"content_type"`
	SHA256       string    `json:"sha256"`
	Binary       bool      `json:"binary"`
	Target       string    `json:"target,omitempty"`
	TargetInside bool      `json:"target_inside,omitempty"`
	// Runnable names the interpreter that runs this regular file by extension
	// (empty when no interpreter is registered). A UI can offer "run this file"
	// only on files where it is set.
	Runnable string `json:"runnable,omitempty"`
}

// binarySampleSize bounds how many leading bytes are inspected for binary
// detection (invalid UTF-8 or a NUL byte). It keeps detection cheap and bounded
// regardless of file size.
const binarySampleSize = 8192

// ReadResult is a bounded byte-range read of a file. SHA256 is the hash of
// exactly Data (the bytes returned), never of a larger slice, so a client can
// verify what it received. WholeFile is true only when Data is the entire file.
type ReadResult struct {
	Data        []byte
	Total       int64
	Offset      int64
	Length      int64
	SHA256      string
	WholeFile   bool
	Truncated   bool
	ContentType string
	Binary      bool

	modTime time.Time
}

// LineResult is a bounded line-range read of a text file. Data holds the bytes
// of lines StartLine..EndLine (1-based, inclusive). SHA256 is the hash of
// exactly Data. TotalLines counts every line in the file.
type LineResult struct {
	Data       []byte
	StartLine  int64
	EndLine    int64
	TotalLines int64
	SHA256     string
	Truncated  bool
}

// Fs provides safe filesystem operations rooted at a workspace directory.
type Fs struct {
	root      string
	diskLimit int64
	used      int64
	mu        sync.Mutex
}

// NewFs creates a filesystem view rooted at root. root must exist.
func NewFs(root string) (*Fs, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Canonicalize so symlinked temp dirs compare correctly.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	f := &Fs{root: abs}
	if used, err := f.DiskUsage(); err == nil {
		f.used = used
	}
	return f, nil
}

// Root returns the canonical workspace root.
func (f *Fs) Root() string { return f.root }

// SetDiskLimit sets the disk quota in bytes (0 disables enforcement).
func (f *Fs) SetDiskLimit(bytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diskLimit = bytes
}

// Used returns the currently tracked disk usage in bytes.
func (f *Fs) Used() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used
}

// Recheck recomputes disk usage from the filesystem (quota audit on load).
func (f *Fs) Recheck() error {
	used, err := f.DiskUsage()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.used = used
	f.mu.Unlock()
	return nil
}

// reserve atomically checks and accounts for a size delta under the quota.
func (f *Fs) reserve(delta int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diskLimit > 0 && f.used+delta > f.diskLimit {
		return &QuotaError{Used: f.used, Limit: f.diskLimit}
	}
	f.used += delta
	if f.used < 0 {
		f.used = 0
	}
	return nil
}

// Resolve validates a user path and returns its absolute location.
func (f *Fs) Resolve(userPath string) (string, error) {
	clean, err := cleanRelative(userPath)
	if err != nil {
		return "", err
	}
	return secureJoin(f.root, clean)
}

// resolveNoFollowFinal resolves a user path without following a symlink in the
// final component, so the link itself can be reported.
func (f *Fs) resolveNoFollowFinal(userPath string) (string, error) {
	clean, err := cleanRelative(userPath)
	if err != nil {
		return "", err
	}
	return resolveFinal(f.root, clean)
}

// relPath converts an absolute in-workspace path back to a slash-prefixed
// path rooted at /.
func (f *Fs) relPath(abs string) string {
	rel, err := filepath.Rel(f.root, abs)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// List returns the direct children of a directory, sorted directories-first
// then by name (byte-wise). It returns every child; use ListPage to bound the
// result.
func (f *Fs) List(userPath string) ([]Entry, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errNotDirectory
	}
	children, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(children))
	for _, c := range children {
		e, err := f.entryFor(abs, c)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

// ListPage returns a bounded page of a directory listing. limit caps the number
// of entries returned (<= 0 means no bound); offset skips that many sorted
// entries. Total is the full child count, so a client can page even when the
// page is truncated.
func (f *Fs) ListPage(userPath string, limit, offset int) (Listing, error) {
	entries, err := f.List(userPath)
	if err != nil {
		return Listing{}, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	window := entries[offset:]
	truncated := false
	if limit > 0 && len(window) > limit {
		window = window[:limit]
		truncated = true
	}
	return Listing{
		Path:      f.relPath(mustResolve(f, userPath)),
		Entries:   window,
		Count:     len(window),
		Total:     len(entries),
		Truncated: truncated,
	}, nil
}

// entryFor builds an Entry for one directory child.
func (f *Fs) entryFor(dir string, c os.DirEntry) (Entry, error) {
	abs := filepath.Join(dir, c.Name())
	// Lstat (not the DirEntry's cached info) so the mode and size describe the
	// link itself, never its target.
	li, err := os.Lstat(abs)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		Name:    c.Name(),
		Path:    f.relPath(abs),
		Size:    li.Size(),
		Mode:    fmt.Sprintf("%04o", li.Mode().Perm()),
		ModTime: li.ModTime().UTC(),
	}
	switch {
	case li.Mode()&os.ModeSymlink != 0:
		e.Type = "symlink"
		target, terr := os.Readlink(abs)
		if terr == nil {
			e.Target = target
			e.TargetInside = f.targetInside(abs)
		}
	case li.IsDir():
		e.Type = "dir"
		e.Size = 0
	default:
		e.Type = "file"
		e.Runnable = model.InterpreterForFile(c.Name())
	}
	return e, nil
}

// targetInside reports whether the symlink at abs resolves within the root. A
// dangling link whose lexical target stays inside the root is reported inside;
// an escaping or unresolvable link is not.
func (f *Fs) targetInside(abs string) bool {
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Dangling: fall back to a lexical check of the link's target.
		target, rerr := os.Readlink(abs)
		if rerr != nil {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		return within(f.root, filepath.Clean(target))
	}
	return within(f.root, resolved)
}

// sortEntries orders directories before files (symlinks count as files), then
// by name byte-wise.
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "dir", entries[j].Type == "dir"
		if di != dj {
			return di
		}
		return entries[i].Name < entries[j].Name
	})
}

// mustResolve resolves a path for reporting; on error it returns a best-effort
// slash path.
func mustResolve(f *Fs, userPath string) string {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return userPath
	}
	return abs
}

var errNotFound = &plainError{"file not found"}

// IsNotFound reports whether err is the workspace not-found error. The fs layer
// normalizes os.ErrNotExist into errNotFound, so unrelated os.ErrNotExist
// errors (e.g. a missing nsjail template) are intentionally not matched.
func IsNotFound(err error) bool { return err == errNotFound }

// Stat returns metadata for a path (file, directory or symlink) without reading
// its contents. For a regular file it includes the full-file sha256; for a
// directory or symlink SHA256 is empty. errNotDirectory is no longer returned:
// callers that require a file check Type themselves.
func (f *Fs) Stat(userPath string) (FileInfo, error) {
	abs, err := f.resolveNoFollowFinal(userPath)
	if err != nil {
		return FileInfo{}, err
	}
	li, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return FileInfo{}, errNotFound
		}
		return FileInfo{}, err
	}
	info := FileInfo{
		Path:    f.relPath(abs),
		Name:    li.Name(),
		Size:    li.Size(),
		Mode:    fmt.Sprintf("%04o", li.Mode().Perm()),
		ModTime: li.ModTime().UTC(),
	}
	switch {
	case li.Mode()&os.ModeSymlink != 0:
		info.Type = "symlink"
		if target, terr := os.Readlink(abs); terr == nil {
			info.Target = target
			info.TargetInside = f.targetInside(abs)
		}
		return info, nil
	case li.IsDir():
		info.Type = "dir"
		info.Size = 0
		return info, nil
	}
	info.Type = "file"
	sum, err := hashFile(abs)
	if err != nil {
		return FileInfo{}, err
	}
	ct := detectContentType(abs, nil)
	info.ContentType = ct
	info.SHA256 = sum
	info.Binary = isBinaryFile(abs)
	info.Runnable = model.InterpreterForFile(li.Name())
	return info, nil
}

// Read reads up to maxBytes from a file. If the file is larger than maxBytes
// it returns the prefix and truncated=true. The returned FileInfo.SHA256 is the
// hash of exactly the returned bytes; FileInfo.Binary is detected from a
// bounded sample of those bytes. WholeFile is true when the whole file was
// returned.
func (f *Fs) Read(userPath string, maxBytes int64) (data []byte, info FileInfo, truncated bool, err error) {
	res, err := f.ReadRange(userPath, 0, maxBytes)
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	info = FileInfo{
		Path:        f.relPath(mustResolve(f, userPath)),
		Name:        filepath.Base(f.relPath(mustResolve(f, userPath))),
		Type:        "file",
		Size:        res.Total,
		ModTime:     res.modTime,
		ContentType: res.ContentType,
		SHA256:      res.SHA256,
		Binary:      res.Binary,
	}
	return res.Data, info, res.Truncated, nil
}

// ReadRange reads up to limit bytes starting at byte offset. It streams from
// disk, so it does not load more than the requested slice. limit <= 0 means
// "no explicit limit" and is treated as the whole remaining file, which the
// service layer bounds with the read cap. The returned ReadResult.SHA256 is the
// hash of exactly the returned bytes and WholeFile is true only when the slice
// covers the entire file.
func (f *Fs) ReadRange(userPath string, offset, limit int64) (ReadResult, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return ReadResult{}, err
	}
	fh, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ReadResult{}, errNotFound
		}
		return ReadResult{}, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return ReadResult{}, err
	}
	if fi.IsDir() {
		return ReadResult{}, errNotDirectory
	}
	total := fi.Size()
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	toRead := total - offset
	if limit > 0 && limit < toRead {
		toRead = limit
	}
	buf := make([]byte, toRead)
	if toRead > 0 {
		if _, err := fh.ReadAt(buf, offset); err != nil && err != io.EOF {
			return ReadResult{}, err
		}
	}
	whole := offset == 0 && toRead == total
	sum := sha256.Sum256(buf)
	return ReadResult{
		Data:        buf,
		Total:       total,
		Offset:      offset,
		Length:      toRead,
		SHA256:      hex.EncodeToString(sum[:]),
		WholeFile:   whole,
		Truncated:   !whole,
		ContentType: detectContentType(abs, buf),
		Binary:      isBinary(buf),
		modTime:     fi.ModTime().UTC(),
	}, nil
}

// maxLineBytes bounds a single line so a file with no newline cannot force an
// unbounded allocation while scanning.
const maxLineBytes = 1 << 20

// ReadLines returns lines [startLine, endLine] (1-based, inclusive) of a text
// file. endLine <= 0 means "to the end of the file"; startLine <= 0 is treated
// as 1. Lines longer than maxLineBytes are treated as a single line capped at
// that size. TotalLines counts every line (a final line without a trailing
// newline still counts). maxBytes bounds the accumulated output (<= 0 means no
// bound); when the requested lines exceed it, Truncated is set. SHA256 is the
// hash of exactly the returned bytes.
func (f *Fs) ReadLines(userPath string, startLine, endLine, maxBytes int64) (LineResult, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return LineResult{}, err
	}
	fh, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return LineResult{}, errNotFound
		}
		return LineResult{}, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return LineResult{}, err
	}
	if fi.IsDir() {
		return LineResult{}, errNotDirectory
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine > 0 && endLine < startLine {
		return LineResult{}, errRange
	}
	br := bufio.NewReaderSize(fh, 64*1024)
	var (
		out        bytes.Buffer
		line       int64
		totalLines int64
		overCap    bool
	)
	for {
		chunk, err := readBoundedLine(br)
		if len(chunk) > 0 || err == nil {
			line++
			totalLines = line
			if line >= startLine && (endLine <= 0 || line <= endLine) {
				if maxBytes > 0 && int64(out.Len()+len(chunk)) > maxBytes {
					overCap = true
				} else if !overCap {
					out.Write(chunk)
				}
			}
		}
		if err != nil {
			break
		}
	}
	returnedEnd := endLine
	if returnedEnd <= 0 || returnedEnd > totalLines {
		returnedEnd = totalLines
	}
	data := out.Bytes()
	sum := sha256.Sum256(data)
	return LineResult{
		Data:       data,
		StartLine:  startLine,
		EndLine:    returnedEnd,
		TotalLines: totalLines,
		SHA256:     hex.EncodeToString(sum[:]),
		Truncated:  overCap || (endLine > 0 && endLine < totalLines),
	}, nil
}

// readBoundedLine reads one line (including its trailing newline) up to
// maxLineBytes. It returns the bytes and io.EOF when the final line has no
// trailing newline.
func readBoundedLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) >= maxLineBytes {
			// Discard the rest of an over-long line but keep a newline as a
			// delimiter if one is reachable.
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			return buf[:maxLineBytes], err
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// Open returns an *os.File positioned at the start of a regular file for
// streaming (used by download). The caller closes it. A directory is rejected
// with errNotDirectory.
func (f *Fs) Open(userPath string) (*os.File, FileInfo, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return nil, FileInfo{}, err
	}
	fh, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, FileInfo{}, errNotFound
		}
		return nil, FileInfo{}, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, FileInfo{}, err
	}
	if fi.IsDir() {
		fh.Close()
		return nil, FileInfo{}, errNotDirectory
	}
	info := FileInfo{
		Path:        f.relPath(abs),
		Name:        fi.Name(),
		Type:        "file",
		Size:        fi.Size(),
		Mode:        fmt.Sprintf("%04o", fi.Mode().Perm()),
		ModTime:     fi.ModTime().UTC(),
		ContentType: detectContentType(abs, nil),
	}
	return fh, info, nil
}

// WriteOptions controls a write.
type WriteOptions struct {
	MaxBytes int64 // per-file ceiling (0 = unlimited)
}

// Write writes data to a file, optionally appending, enforcing maxBytes and
// the disk quota.
func (f *Fs) Write(userPath string, data []byte, appendMode bool, opts WriteOptions) error {
	if opts.MaxBytes > 0 && int64(len(data)) > opts.MaxBytes {
		return errTooLarge
	}
	abs, err := f.Resolve(userPath)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return errIsDirectory
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	var delta int64
	if appendMode {
		delta = int64(len(data))
	} else {
		delta = int64(len(data)) - statSize(abs)
	}
	if err := f.reserve(delta); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	fh, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		_ = f.reserve(-delta)
		if isNoSpace(err) {
			return &QuotaError{Used: f.Used(), Limit: f.diskLimit}
		}
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		_ = f.reserve(-delta)
		return err
	}
	return fh.Close()
}

// Remove deletes a file or directory tree.
func (f *Fs) Remove(userPath string) error {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return err
	}
	if abs == f.root {
		return errTraversal
	}
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return errNotFound
		}
		return err
	}
	usage, _ := subtreeSize(abs)
	if err := os.RemoveAll(abs); err != nil {
		return err
	}
	_ = f.reserve(-usage)
	return nil
}

// Mkdir creates a directory and any parents.
func (f *Fs) Mkdir(userPath string) error {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// Rename moves a file or directory within the workspace.
func (f *Fs) Rename(from, to string) error {
	src, err := f.Resolve(from)
	if err != nil {
		return err
	}
	dst, err := f.Resolve(to)
	if err != nil {
		return err
	}
	if src == f.root || dst == f.root {
		return errTraversal
	}
	if _, err := os.Lstat(src); err != nil {
		if os.IsNotExist(err) {
			return errNotFound
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// HashSHA256 returns the hex sha256 of a file.
func (f *Fs) HashSHA256(userPath string) (string, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return "", err
	}
	return hashFile(abs)
}

// DiskUsage returns the number of bytes used by the workspace tree.
func (f *Fs) DiskUsage() (int64, error) {
	return subtreeSize(f.root)
}

// CreateStream opens a file for streaming writes under a workspace-relative
// path, applying the SAME quota accounting as Write. It is used by the archive
// extractor, which must account for bytes as they are actually written (never
// trusting an archive's declared sizes).
//
// Safety properties, because this is a security boundary:
//   - the parent directory is resolved with secureJoin, so a symlink in the
//     path that escapes the root is rejected before anything is created;
//   - MkdirAll is only reached after every existing component was checked by
//     secureJoin (secureJoin returns early at the first non-existent component,
//     and no symlink can exist beneath a non-existent component);
//   - the final component is opened with O_NOFOLLOW, so an existing symlink at
//     the destination is NEVER followed (the write cannot pass through it);
//   - the file is reserved against the workspace quota; a write past the
//     allowance fails with ENOSPC at the filesystem boundary and is normalized
//     to a QuotaError, matching UploadFile/WriteFile;
//   - the mode drops setuid/setgid/sticky and is masked to the sandbox's
//     ownership model.
func (f *Fs) CreateStream(userPath string, mode os.FileMode, overwrite bool) (*StreamWriter, error) {
	clean, err := cleanRelative(userPath)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return nil, errIsDirectory
	}
	abs, err := secureJoin(f.root, clean)
	if err != nil {
		return nil, err
	}
	if abs == f.root {
		return nil, errTraversal
	}
	// Refuse a directory at the destination explicitly.
	if fi, err := os.Lstat(abs); err == nil && fi.IsDir() {
		return nil, errIsDirectory
	}
	perm := mode.Perm() // drops setuid/setgid/sticky
	if perm == 0 {
		perm = 0o644
	}
	flags := os.O_CREATE | os.O_WRONLY
	if overwrite {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	// Resolve parent safety first, then create it (parents are all checked by
	// secureJoin above; no escaping symlink can be beneath a missing component).
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	// Re-validate the immediate parent: MkdirAll could in principle have raced
	// with an attacker-created symlink inside the workspace. resolveFinal
	// rejects an escaping final symlink, and we check the parent is a real
	// directory we can trust.
	if resolvedParent, err := filepath.EvalSymlinks(parent); err != nil || !within(f.root, resolvedParent) {
		return nil, errSymlinkEsc
	}
	// O_NOFOLLOW ensures an existing symlink at abs is refused rather than
	// followed. os.OpenFile does not expose it directly, so use syscall via
	// O_NOFOLLOW in the flags on Linux.
	fh, err := os.OpenFile(abs, flags|syscall.O_NOFOLLOW, perm)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrExists
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, errSymlinkEsc
		}
		if isNoSpace(err) {
			return nil, &QuotaError{Used: f.Used(), Limit: f.diskLimit}
		}
		return nil, err
	}
	return &StreamWriter{fs: f, file: fh, path: abs}, nil
}

// StreamWriter writes a file under quota accounting. Close must be called; it
// reconciles the tracked usage with the bytes actually written.
type StreamWriter struct {
	fs       *Fs
	file     *os.File
	path     string
	written  int64
	reserved int64
	closed   bool
}

// Write appends p, reconciling the quota by the delta actually accepted. A
// short write or an ENOSPC at the filesystem boundary is surfaced (ENOSPC is
// normalized to a QuotaError by the archive layer).
func (w *StreamWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	// Reserve the bytes about to be written; if the quota refuses, fail before
	// touching the file so the disk error is attributed correctly.
	if err := w.fs.reserve(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	if n > 0 {
		w.written += int64(n)
		w.reserved += int64(n)
	}
	if n < len(p) {
		// Refund the bytes not written.
		_ = w.fs.reserve(int64(n - len(p)))
		w.reserved += int64(n - len(p))
	}
	if err != nil && isNoSpace(err) {
		return n, &QuotaError{Used: w.fs.Used(), Limit: w.fs.diskLimit}
	}
	return n, err
}

// Written returns the number of bytes written so far.
func (w *StreamWriter) Written() int64 { return w.written }

// Close closes the file.
func (w *StreamWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// MkdirStream creates a directory under a workspace-relative path with the same
// symlink safety as CreateStream. The mode drops privileged bits.
func (f *Fs) MkdirStream(userPath string, mode os.FileMode) error {
	clean, err := cleanRelative(userPath)
	if err != nil {
		return err
	}
	if clean == "." {
		return nil
	}
	abs, err := secureJoin(f.root, clean)
	if err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o755
	}
	if fi, err := os.Lstat(abs); err == nil {
		if fi.IsDir() {
			return nil
		}
		return errIsDirectory
	}
	if err := os.MkdirAll(abs, perm); err != nil {
		return err
	}
	return nil
}

// ErrExists signals that a destination already exists and overwrite is false.
var ErrExists = &plainError{"file already exists"}

// IsExists reports whether err indicates an existing destination.
func IsExists(err error) bool { return err == ErrExists }

// errWalkLimit is an internal sentinel used to stop a bounded walk early.
var errWalkLimit = &plainError{"walk entry limit reached"}

// DiskUsageBounded returns the bytes used by the workspace tree, walking at
// most maxEntries directory entries. When the tree is larger, it stops early
// and reports complete=false; the returned size is then a lower bound. A
// maxEntries <= 0 imposes no bound. This keeps the usage endpoint cheap on a
// large workspace (see docs/DECISIONS.md).
func (f *Fs) DiskUsageBounded(maxEntries int) (size int64, complete bool, err error) {
	count := 0
	walkErr := filepath.WalkDir(f.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		count++
		if maxEntries > 0 && count > maxEntries {
			return errWalkLimit
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	if errors.Is(walkErr, errWalkLimit) {
		return size, false, nil
	}
	if walkErr != nil {
		return size, false, walkErr
	}
	return size, true, nil
}

func subtreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func hashFile(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func detectContentType(path string, sniff []byte) string {
	if len(sniff) > 0 {
		if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); ct != "" {
			return ct
		}
		return detectBySniff(sniff)
	}
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func detectBySniff(b []byte) string {
	ct := "text/plain; charset=utf-8"
	if isBinary(b) {
		return "application/octet-stream"
	}
	return ct
}

// isBinary reports whether a bounded sample looks binary: it contains a NUL
// byte or invalid UTF-8. Only the first binarySampleSize bytes are inspected,
// so a large file with binary data only past the sample is reported as text.
// The rule cannot inspect the whole file and is documented as such.
func isBinary(b []byte) bool {
	sample := b
	if len(sample) > binarySampleSize {
		sample = sample[:binarySampleSize]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	return !utf8.Valid(sample)
}

// IsBinary reports whether a sample looks binary using the same bounded rule as
// the read path. It is exported for callers that already hold bytes.
func IsBinary(b []byte) bool { return isBinary(b) }

// isBinaryFile reads a bounded leading sample of a file and applies isBinary.
// An unreadable file is reported as non-binary.
func isBinaryFile(path string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	buf := make([]byte, binarySampleSize)
	n, _ := io.ReadFull(fh, buf)
	return isBinary(buf[:n])
}

var errRange = &plainError{"invalid line range"}

// IsRangeError reports whether err indicates a nonsensical line range.
func IsRangeError(err error) bool { return err == errRange }

var errTooLarge = &plainError{"file exceeds maximum allowed size"}

// IsTooLarge reports whether err indicates an oversized write.
func IsTooLarge(err error) bool { return err == errTooLarge }

// QuotaError signals a disk quota violation.
type QuotaError struct {
	Used  int64
	Limit int64
}

func (e *QuotaError) Error() string { return "disk quota exceeded" }

// statSize returns the size of a path or 0.
func statSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return fi.Size()
	}
	return 0
}

// isNoSpace reports whether err is ENOSPC.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
