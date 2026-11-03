package service

import (
	"io"
	"io/fs"
	"os"

	"sandboxd/internal/archive"
	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// archiveOptions projects the configured `archives:` section into the pure
// extraction engine's Options.
func (s *Service) archiveOptions() archive.Options {
	a := s.cfg.Archives
	formats := make([]archive.Format, 0, len(a.Formats))
	for _, f := range a.Formats {
		formats = append(formats, archive.Format(f))
	}
	return archive.Options{
		MaxEntries:    a.MaxEntries,
		MaxTotalBytes: a.MaxTotalBytes(),
		MaxFileBytes:  a.MaxFileBytes(),
		MaxRatio:      a.MaxRatio,
		Overwrite:     a.Overwrite,
		SkipJunk:      a.SkipJunk,
		Formats:       formats,
	}
}

// ArchivesEnabled reports the server-level archive switch.
func (s *Service) ArchivesEnabled() bool { return s.cfg.Archives.Enabled }

// ArchivesUploadBytes returns the archive upload cap.
func (s *Service) ArchivesUploadBytes() int64 { return s.cfg.Archives.UploadBytes() }

// fsSink writes validated archive entries into a workspace filesystem. It is
// the ONLY place the extraction engine touches disk, and it routes through the
// workspace's symlink-safe, quota-accounted streaming writers.
type fsSink struct {
	fs *workspace.Fs
}

func (s fsSink) Mkdir(rel string, mode fs.FileMode) error {
	return s.fs.MkdirStream(rel, mode)
}

func (s fsSink) Open(rel string, mode fs.FileMode, overwrite bool) (io.WriteCloser, error) {
	w, err := s.fs.CreateStream(rel, mode, overwrite)
	if err != nil {
		if workspace.IsExists(err) {
			return nil, archive.ErrEntryExists
		}
		return nil, err
	}
	return w, nil
}

// ExtractArchive uploads-and-extracts in one step: the bytes are the uploaded
// archive, dest is the destination directory (workspace-relative). A dry run
// validates and lists without writing.
func (s *Service) ExtractArchive(w *model.Workspace, data []byte, dest string, overwrite, dryRun bool) (archive.Result, error) {
	if !s.cfg.Archives.Enabled {
		return archive.Result{}, model.New(422, model.CodeUnprocessable, "archive support is disabled")
	}
	if err := s.ensureActive(w); err != nil {
		return archive.Result{}, err
	}
	if err := s.ensureEngineWriteAllowed(w); err != nil {
		return archive.Result{}, err
	}
	fsys, err := s.WorkspaceFs(w)
	if err != nil {
		return archive.Result{}, err
	}
	destRel, err := cleanArchiveDest(dest)
	if err != nil {
		return archive.Result{}, err
	}
	opts := s.archiveOptions()
	opts.Overwrite = overwrite
	res, err := archive.Extract(data, opts, destRel, fsSink{fs: fsys}, workspace.CleanRelative, dryRun)
	if err != nil {
		return res, err
	}
	res.Destination = reportDest(destRel)
	return res, nil
}

// ExtractArchiveInPlace extracts an archive ALREADY in the workspace (an
// uploaded file, a git-clone artifact, a downloaded release) into dest. Same
// engine code as the upload path.
func (s *Service) ExtractArchiveInPlace(w *model.Workspace, path, dest string, overwrite, dryRun bool) (archive.Result, error) {
	if !s.cfg.Archives.Enabled {
		return archive.Result{}, model.New(422, model.CodeUnprocessable, "archive support is disabled")
	}
	if err := s.ensureActive(w); err != nil {
		return archive.Result{}, err
	}
	if err := s.ensureEngineWriteAllowed(w); err != nil {
		return archive.Result{}, err
	}
	fsys, err := s.WorkspaceFs(w)
	if err != nil {
		return archive.Result{}, err
	}
	// Read the source file with a bound; a workspace file is already within the
	// disk allowance but the engine still refuses to read an unbounded blob
	// into memory.
	rc, info, err := fsys.Open(path)
	if err != nil {
		return archive.Result{}, err
	}
	defer rc.Close()
	max := s.cfg.Archives.UploadBytes()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return archive.Result{}, err
	}
	if int64(len(data)) > max {
		return archive.Result{}, model.Wrap(413, model.CodePayloadTooLarge,
			"archive exceeds archives.upload_mb", map[string]string{"limit_bytes": itoa64(max), "path": info.Path})
	}
	destRel, err := cleanArchiveDest(dest)
	if err != nil {
		return archive.Result{}, err
	}
	opts := s.archiveOptions()
	opts.Overwrite = overwrite
	res, err := archive.Extract(data, opts, destRel, fsSink{fs: fsys}, workspace.CleanRelative, dryRun)
	if err != nil {
		return res, err
	}
	res.Destination = reportDest(destRel)
	return res, nil
}

// ExportArchive streams a zip of a workspace directory to w. It is the export
// direction ("download my project"), built by the same engine package.
func (s *Service) ExportArchive(w *model.Workspace, path string, w2 io.Writer) (archive.ExportResult, error) {
	if !s.cfg.Archives.Enabled {
		return archive.ExportResult{}, model.New(422, model.CodeUnprocessable, "archive support is disabled")
	}
	fsys, err := s.WorkspaceFs(w)
	if err != nil {
		return archive.ExportResult{}, err
	}
	abs, err := fsys.Resolve(path)
	if err != nil {
		return archive.ExportResult{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return archive.ExportResult{}, model.ErrNotFound
		}
		return archive.ExportResult{}, err
	}
	if !fi.IsDir() {
		return archive.ExportResult{}, model.New(422, model.CodeNotDirectory, "path is not a directory")
	}
	return archive.WriteZip(w2, abs, s.cfg.Archives.SkipJunk)
}

// cleanArchiveDest validates a destination directory through the workspace
// path machinery (never a second validator).
func cleanArchiveDest(dest string) (string, error) {
	rel, err := workspace.CleanRelative(dest)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

// reportDest renders a cleaned destination for the response ("/" for root).
func reportDest(rel string) string {
	if rel == "" {
		return "/"
	}
	return "/" + rel
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
