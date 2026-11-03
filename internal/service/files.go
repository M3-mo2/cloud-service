package service

import (
	"io"
	"mime"
	"path/filepath"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// ListFiles lists a directory in a workspace, returning every entry.
func (s *Service) ListFiles(w *model.Workspace, path string) ([]workspace.Entry, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return nil, err
	}
	return fs.List(path)
}

// ListFilesPage lists a directory in a workspace as a bounded page. limit
// caps the returned entries (<= 0 uses the configured cap); offset skips sorted
// entries. Listing a path that is not a directory fails with a not-a-directory
// error, which the API maps to 422.
func (s *Service) ListFilesPage(w *model.Workspace, path string, limit, offset int) (workspace.Listing, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return workspace.Listing{}, err
	}
	if limit <= 0 {
		limit = s.cfg.Caps.FileListMaxEntries()
	}
	return fs.ListPage(path, limit, offset)
}

// ReadFileResult holds a file read plus its metadata. SHA256 is the hash of
// exactly Data (the bytes returned in the response); it is not the whole-file
// hash unless WholeFile is true. Size is the total file size.
type ReadFileResult struct {
	Data        []byte
	ContentType string
	Size        int64
	SHA256      string
	Truncated   bool
	Offset      int64
	Length      int64
	WholeFile   bool
	Binary      bool
}

// ReadFile reads a file enforcing the configured read cap. It is the default
// whole-file view: a file larger than caps.file_read_mb yields a capped prefix
// with Truncated=true, and SHA256 hashes exactly that prefix.
func (s *Service) ReadFile(w *model.Workspace, path string) (ReadFileResult, error) {
	return s.ReadFileRange(w, path, 0, 0)
}

// ReadFileRange reads a byte range of a file. offset is a byte offset and limit
// a byte count; both default to 0. A limit above caps.file_read_mb is rejected
// with 413 (the whole-file read cap governs inline viewing); a limit <= 0 reads
// at most caps.file_read_mb bytes. A slice that fits within the cap is served
// regardless of the whole-file size. SHA256 is the hash of exactly the returned
// slice and WholeFile is true only when the slice is the entire file.
func (s *Service) ReadFileRange(w *model.Workspace, path string, offset, limit int64) (ReadFileResult, error) {
	capBytes := s.cfg.Caps.FileReadBytes()
	if limit > 0 && limit > capBytes {
		return ReadFileResult{}, model.New(413, model.CodePayloadTooLarge, "limit exceeds file_read_mb")
	}
	if limit <= 0 {
		limit = capBytes
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return ReadFileResult{}, err
	}
	res, err := fs.ReadRange(path, offset, limit)
	if err != nil {
		return ReadFileResult{}, err
	}
	ct := res.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ReadFileResult{
		Data:        res.Data,
		ContentType: ct,
		Size:        res.Total,
		SHA256:      res.SHA256,
		Truncated:   res.Truncated,
		Offset:      res.Offset,
		Length:      res.Length,
		WholeFile:   res.WholeFile,
		Binary:      res.Binary,
	}, nil
}

// ReadFileLines returns lines [startLine, endLine] (1-based, inclusive) of a
// text file. endLine <= 0 means "to the end". The returned SHA256 is the hash
// of exactly the returned bytes; Truncated is true when the requested lines
// exceed the read cap or the range does not reach the end of the file.
func (s *Service) ReadFileLines(w *model.Workspace, path string, startLine, endLine int64) (workspace.LineResult, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return workspace.LineResult{}, err
	}
	return fs.ReadLines(path, startLine, endLine, s.cfg.Caps.FileReadBytes())
}

// OpenFile opens a file for streaming, returning the reader and metadata. It is
// exempt from caps.file_read_mb (that cap governs inline viewing) but the
// caller still holds the workspace disk quota for storage. The caller closes
// the reader.
func (s *Service) OpenFile(w *model.Workspace, path string) (io.ReadCloser, workspace.FileInfo, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	fh, info, err := fs.Open(path)
	if err != nil {
		return nil, workspace.FileInfo{}, err
	}
	return fh, info, nil
}

// StatFile returns metadata for a file, directory or symlink.
func (s *Service) StatFile(w *model.Workspace, path string) (workspace.FileInfo, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return fs.Stat(path)
}

// WriteFile writes a file enforcing the configured write cap and the disk quota.
// It is a MUTATING operation, so a locked workspace refuses it (subscription_
// expired) while file READS remain allowed.
func (s *Service) WriteFile(w *model.Workspace, path string, data []byte, encoding string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Write(path, data, false, workspace.WriteOptions{MaxBytes: s.cfg.Caps.FileWriteBytes()})
}

// DeleteFile removes a file or directory. A mutating operation: refused while
// locked (manual workspace DELETE is still allowed at the workspace level).
func (s *Service) DeleteFile(w *model.Workspace, path string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Remove(path)
}

// Mkdir creates a directory. A mutating operation: refused while locked.
func (s *Service) Mkdir(w *model.Workspace, path string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Mkdir(path)
}

// Rename moves a file or directory. A mutating operation: refused while locked.
func (s *Service) Rename(w *model.Workspace, from, to string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Rename(from, to)
}

// UploadFile writes uploaded bytes as a file, enforcing the upload cap and
// the workspace disk quota. A mutating operation: refused while locked.
func (s *Service) UploadFile(w *model.Workspace, path string, data []byte) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Write(path, data, false, workspace.WriteOptions{MaxBytes: s.cfg.Caps.UploadBytes()})
}

// ContentDisposition builds a safe attachment header for a path.
func ContentDisposition(path string) string {
	name := filepath.Base(path)
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
