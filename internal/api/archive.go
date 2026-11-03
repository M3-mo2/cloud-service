package api

import (
	"io"
	"net/http"
	"strconv"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// handleUploadArchive implements POST .../files/archive: a multipart upload
// whose archive is extracted into the workspace in one request. Fields:
// archive=<file> (required), path=<dest dir> (optional, default "/"), and the
// optional form values overwrite=true|false and dry_run=true.
func (s *Server) handleUploadArchive(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if !s.svc.ArchivesEnabled() {
		writeErrorWithCode(w, model.New(422, model.CodeUnprocessable, "archive support is disabled"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.svc.ArchivesUploadBytes())
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if asMaxBytes(err, &maxErr) {
			writeErrorWithCode(w, model.Wrap(413, model.CodePayloadTooLarge,
				"archive upload too large", map[string]string{"limit": "archives.upload_mb"}))
			return
		}
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "invalid multipart form",
			map[string]string{"error": err.Error()}))
		return
	}
	dest := r.FormValue("path")
	overwrite, err := formBoolDefault(r, "overwrite", s.cfg.Archives.Overwrite)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	dryRun, err := formBoolDefault(r, "dry_run", false)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	file, _, err := r.FormFile("archive")
	if err != nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "archive is required", nil))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, s.svc.ArchivesUploadBytes()+1))
	if err != nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "read upload failed", nil))
		return
	}
	if int64(len(data)) > s.svc.ArchivesUploadBytes() {
		writeErrorWithCode(w, model.Wrap(413, model.CodePayloadTooLarge,
			"archive upload too large", map[string]string{"limit": "archives.upload_mb"}))
		return
	}
	res, err := s.svc.ExtractArchive(ws, data, dest, overwrite, dryRun)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// extractRequest is the POST .../files/extract body.
type extractRequest struct {
	Path      string `json:"path" validate:"required"`
	Dest      string `json:"dest"`
	Overwrite *bool  `json:"overwrite"`
	DryRun    *bool  `json:"dry_run"`
}

// handleExtractArchive implements POST .../files/extract: it extracts an
// archive that is ALREADY in the workspace, using the same engine code.
func (s *Server) handleExtractArchive(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if !s.svc.ArchivesEnabled() {
		writeErrorWithCode(w, model.New(422, model.CodeUnprocessable, "archive support is disabled"))
		return
	}
	var req extractRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	overwrite := s.cfg.Archives.Overwrite
	if req.Overwrite != nil {
		overwrite = *req.Overwrite
	}
	dryRun := req.DryRun != nil && *req.DryRun
	res, err := s.svc.ExtractArchiveInPlace(ws, req.Path, req.Dest, overwrite, dryRun)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleExportArchive implements GET .../files/archive?path=<dir>: it streams a
// zip of the directory tree. The export direction is built by the same engine
// package as extraction, so "upload a project" is symmetrical with "download my
// project".
func (s *Server) handleExportArchive(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if !s.svc.ArchivesEnabled() {
		writeErrorWithCode(w, model.New(422, model.CodeUnprocessable, "archive support is disabled"))
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	name := exportFilename(path)
	// The zip is streamed directly to the client. A pre-write failure (unknown
	// path, not a directory) is reported as JSON because nothing has been sent
	// yet; a mid-stream failure can only truncate, which the client detects.
	cw := &countingWriter{w: w}
	cw.Header().Set("Content-Type", "application/zip")
	cw.Header().Set("Content-Disposition", service.ContentDisposition(name))
	_, err := s.svc.ExportArchive(ws, path, cw)
	if err != nil {
		if cw.n == 0 && !cw.h {
			// Nothing streamed yet: a normal JSON error is still possible.
			writeErrorWithCode(w, err)
			return
		}
		s.logger.Error("archive export failed mid-stream",
			"workspace_id", ws.ID, "path", path, "error", err, "bytes_written", cw.n)
		return
	}
}

// countingWriter records how many bytes have been written to the response, so
// the export handler can tell a pre-write failure (nothing sent: JSON error is
// possible) from a mid-stream one (truncation only).
type countingWriter struct {
	w http.ResponseWriter
	n int64
	h bool
}

func (c *countingWriter) Header() http.Header { return c.w.Header() }

func (c *countingWriter) WriteHeader(status int) {
	if !c.h {
		c.h = true
		c.w.WriteHeader(status)
	}
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if !c.h {
		c.h = true
		c.w.WriteHeader(http.StatusOK)
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// exportFilename derives a sensible .zip filename from the exported directory.
func exportFilename(path string) string {
	name := path
	for len(name) > 0 && name[len(name)-1] == '/' {
		name = name[:len(name)-1]
	}
	base := "workspace"
	if i := lastSlash(name); i >= 0 {
		if name[i+1:] != "" {
			base = name[i+1:]
		}
	} else if name != "" {
		base = name
	}
	return base + ".zip"
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// formBoolDefault parses an optional boolean multipart form value. An absent
// value falls back to def; a present but unparseable value is a 400.
func formBoolDefault(r *http.Request, name string, def bool) (bool, error) {
	v := r.FormValue(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, model.Wrap(400, model.CodeValidation,
			name+" must be true or false", map[string]string{"field": name})
	}
	return b, nil
}
