package service

import (
	"mime"
	"path/filepath"

	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// ListFiles lists a directory in a workspace.
func (s *Service) ListFiles(w *model.Workspace, path string) ([]workspace.Entry, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return nil, err
	}
	return fs.List(path)
}

// ReadFileResult holds a file read plus its metadata.
type ReadFileResult struct {
	Data        []byte
	ContentType string
	Size        int64
	SHA256      string
	Truncated   bool
}

// ReadFile reads a file enforcing the configured read cap.
func (s *Service) ReadFile(w *model.Workspace, path string) (ReadFileResult, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return ReadFileResult{}, err
	}
	data, info, truncated, err := fs.Read(path, s.cfg.Caps.FileReadBytes())
	if err != nil {
		return ReadFileResult{}, err
	}
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ReadFileResult{
		Data:        data,
		ContentType: ct,
		Size:        info.Size,
		SHA256:      info.SHA256,
		Truncated:   truncated,
	}, nil
}

// StatFile returns metadata for a file.
func (s *Service) StatFile(w *model.Workspace, path string) (workspace.FileInfo, error) {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	return fs.Stat(path)
}

// WriteFile writes a file enforcing the configured write cap and the disk quota.
func (s *Service) WriteFile(w *model.Workspace, path string, data []byte, encoding string) error {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Write(path, data, false, workspace.WriteOptions{MaxBytes: s.cfg.Caps.FileWriteBytes()})
}

// DeleteFile removes a file or directory.
func (s *Service) DeleteFile(w *model.Workspace, path string) error {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Remove(path)
}

// Mkdir creates a directory.
func (s *Service) Mkdir(w *model.Workspace, path string) error {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Mkdir(path)
}

// Rename moves a file or directory.
func (s *Service) Rename(w *model.Workspace, from, to string) error {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Rename(from, to)
}

// UploadFile writes uploaded bytes as a file, enforcing write cap and quota.
func (s *Service) UploadFile(w *model.Workspace, path string, data []byte) error {
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return err
	}
	return fs.Write(path, data, false, workspace.WriteOptions{MaxBytes: s.cfg.Caps.FileWriteBytes()})
}

// ContentDisposition builds a safe attachment header for a path.
func ContentDisposition(path string) string {
	name := filepath.Base(path)
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
