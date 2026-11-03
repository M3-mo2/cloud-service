package archive

import (
	"archive/zip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ExportResult reports what an export wrote.
type ExportResult struct {
	Entries int    `json:"entries"`
	Skipped int    `json:"skipped"`
	Bytes   int64  `json:"bytes"`
	Format  string `json:"format"`
}

// WriteZip streams a zip of the tree rooted at destAbs to w. The zip is built
// in the engine, so "download my project" is symmetrical with "upload a
// project".
//
// Safety: the walk uses Lstat semantics (filepath.WalkDir does NOT follow
// symlinks), and symlinks are SKIPPED rather than emitted, so an export can
// never smuggle a link out of the workspace. Junk (when skipJunk) is skipped
// too, so an export round-trips an imported archive cleanly. The root itself is
// never emitted.
func WriteZip(w io.Writer, destAbs string, skipJunk bool) (ExportResult, error) {
	res := ExportResult{Format: string(FormatZip)}
	zw := zip.NewWriter(w)
	defer zw.Close()

	err := filepath.WalkDir(destAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == destAbs {
			return nil
		}
		rel, rerr := filepath.Rel(destAbs, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if skipJunk && isJunk(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			res.Skipped++
			return nil
		}
		info, ierr := os.Lstat(p)
		if ierr != nil {
			return ierr
		}
		name := filepath.ToSlash(rel)
		if info.Mode()&os.ModeSymlink != 0 {
			// Never emit a link: extraction refuses them, and emitting one
			// would let a link escape a future consumer.
			res.Skipped++
			return nil
		}
		if info.IsDir() {
			hdr := &zip.FileHeader{Name: name + "/", Method: zip.Deflate}
			hdr.SetMode(info.Mode().Perm() | fs.ModeDir)
			hdr.Modified = modTime(info)
			if _, err := zw.CreateHeader(hdr); err != nil {
				return err
			}
			res.Entries++
			return nil
		}
		if !info.Mode().IsRegular() {
			res.Skipped++
			return nil
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(info.Mode().Perm())
		hdr.Modified = modTime(info)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		fh, err := os.Open(p)
		if err != nil {
			return err
		}
		n, cerr := io.Copy(fw, fh)
		fh.Close()
		if cerr != nil {
			return cerr
		}
		res.Bytes += n
		res.Entries++
		return nil
	})
	if err != nil {
		_ = zw.Close()
		return res, err
	}
	if err := zw.Close(); err != nil {
		return res, err
	}
	return res, nil
}

func modTime(info fs.FileInfo) time.Time {
	t := info.ModTime()
	if t.IsZero() {
		return time.Unix(0, 0)
	}
	return t
}
