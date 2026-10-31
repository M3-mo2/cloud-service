package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Entry describes a directory child.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// FileInfo describes a file for metadata reads.
type FileInfo struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	ContentType string    `json:"content_type"`
	SHA256      string    `json:"sha256"`
	Binary      bool      `json:"binary"`
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

// relPath converts an absolute in-workspace path back to a slash-prefixed
// path rooted at /.
func (f *Fs) relPath(abs string) string {
	rel, err := filepath.Rel(f.root, abs)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// List returns the direct children of a directory.
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
		info, err := c.Info()
		if err != nil {
			continue
		}
		typ := "file"
		if c.IsDir() {
			typ = "dir"
		} else if info.Mode()&os.ModeSymlink != 0 {
			typ = "symlink"
		}
		out = append(out, Entry{
			Name:    c.Name(),
			Path:    f.relPath(filepath.Join(abs, c.Name())),
			Type:    typ,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		})
	}
	return out, nil
}

var errNotFound = &plainError{"file not found"}

// IsNotFound reports whether err is the workspace not-found error. The fs layer
// normalizes os.ErrNotExist into errNotFound, so unrelated os.ErrNotExist
// errors (e.g. a missing nsjail template) are intentionally not matched.
func IsNotFound(err error) bool { return err == errNotFound }

// Stat returns metadata for a file without reading its contents.
func (f *Fs) Stat(userPath string) (FileInfo, error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return FileInfo{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return FileInfo{}, errNotFound
		}
		return FileInfo{}, err
	}
	if fi.IsDir() {
		return FileInfo{}, errNotDirectory
	}
	sum, err := hashFile(abs)
	if err != nil {
		return FileInfo{}, err
	}
	ct := detectContentType(abs, nil)
	return FileInfo{
		Path:        f.relPath(abs),
		Size:        fi.Size(),
		ModTime:     fi.ModTime().UTC(),
		ContentType: ct,
		SHA256:      sum,
		Binary:      !isTextContentType(ct),
	}, nil
}

// Read reads up to maxBytes from a file. If the file is larger than maxBytes
// it returns the prefix and truncated=true.
func (f *Fs) Read(userPath string, maxBytes int64) (data []byte, info FileInfo, truncated bool, err error) {
	abs, err := f.Resolve(userPath)
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	fh, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, FileInfo{}, false, errNotFound
		}
		return nil, FileInfo{}, false, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	if fi.IsDir() {
		return nil, FileInfo{}, false, errNotDirectory
	}
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, fh, maxBytes)
	if err != nil && err != io.EOF {
		return nil, FileInfo{}, false, err
	}
	truncated = fi.Size() > maxBytes
	sum := sha256.Sum256(buf.Bytes())
	ct := detectContentType(abs, buf.Bytes())
	info = FileInfo{
		Path:        f.relPath(abs),
		Size:        fi.Size(),
		ModTime:     fi.ModTime().UTC(),
		ContentType: ct,
		SHA256:      hex.EncodeToString(sum[:]),
		Binary:      !isTextContentType(ct),
	}
	_ = n
	return buf.Bytes(), info, truncated, nil
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
		return errNotDirectory
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
	if !isText(b) {
		return "application/octet-stream"
	}
	return ct
}

func isText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

func isTextContentType(ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "yaml") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "python")
}

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
