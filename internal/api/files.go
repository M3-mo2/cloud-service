package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
	"sandboxd/internal/workspace"
)

type entryJSON struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Mode         string `json:"mode"`
	ModTime      string `json:"mod_time"`
	Target       string `json:"target,omitempty"`
	TargetInside bool   `json:"target_inside,omitempty"`
}

func toEntryJSON(e workspace.Entry) entryJSON {
	return entryJSON{
		Name:         e.Name,
		Path:         e.Path,
		Type:         e.Type,
		Size:         e.Size,
		Mode:         e.Mode,
		ModTime:      e.ModTime.UTC().Format(time.RFC3339),
		Target:       e.Target,
		TargetInside: e.TargetInside,
	}
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	offset, err := queryInt(r.URL.Query(), "offset", 0)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	limit, err := queryInt(r.URL.Query(), "limit", 0)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	listing, err := s.svc.ListFilesPage(ws, path, limit, offset)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]entryJSON, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		out = append(out, toEntryJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      listing.Path,
		"entries":   out,
		"count":     listing.Count,
		"total":     listing.Total,
		"truncated": listing.Truncated,
	})
}

type statJSON struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Mode         string `json:"mode"`
	ModTime      string `json:"mod_time"`
	Target       string `json:"target,omitempty"`
	TargetInside bool   `json:"target_inside,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	Binary       bool   `json:"binary,omitempty"`
}

func (s *Server) handleStatFile(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	info, err := s.svc.StatFile(ws, path)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statJSON{
		Name:         info.Name,
		Path:         info.Path,
		Type:         info.Type,
		Size:         info.Size,
		Mode:         info.Mode,
		ModTime:      info.ModTime.UTC().Format(time.RFC3339),
		Target:       info.Target,
		TargetInside: info.TargetInside,
		ContentType:  info.ContentType,
		SHA256:       info.SHA256,
		Binary:       info.Binary,
	})
}

func (s *Server) handleGetFileContent(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	encoding := q.Get("encoding")
	meta := q.Get("meta") == "true"

	hasBytes := q.Has("offset") || q.Has("limit")
	hasLines := q.Has("start_line") || q.Has("end_line")
	if hasBytes && hasLines {
		writeErrorWithCode(w, model.Wrap(422, model.CodeUnprocessable,
			"byte (offset/limit) and line (start_line/end_line) ranges are mutually exclusive", nil))
		return
	}

	if hasLines {
		s.serveFileLines(w, ws, path, q, encoding)
		return
	}
	s.serveFileBytes(w, ws, path, q, encoding, meta)
}

func (s *Server) serveFileBytes(w http.ResponseWriter, ws *model.Workspace, path string, q url.Values, encoding string, meta bool) {
	offset, err := queryInt64(q, "offset", 0)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	limit, err := queryInt64(q, "limit", 0)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	res, err := s.svc.ReadFileRange(ws, path, offset, limit)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if meta {
		writeJSON(w, http.StatusOK, map[string]any{
			"path":         path,
			"content":      encodeContent(res.Data, encoding, res.Binary),
			"encoding":     contentEncoding(encoding, res.Binary),
			"size":         res.Size,
			"total_size":   res.Size,
			"range":        map[string]any{"offset": res.Offset, "length": res.Length},
			"whole_file":   res.WholeFile,
			"sha256":       res.SHA256,
			"content_type": res.ContentType,
			"truncated":    res.Truncated,
			"binary":       res.Binary,
		})
		return
	}
	w.Header().Set("Content-Type", res.ContentType)
	w.Header().Set("Content-Disposition", service.ContentDisposition(path))
	w.Header().Set("X-Sandboxd-Truncated", strconv.FormatBool(res.Truncated))
	w.Header().Set("X-Sandboxd-Whole-File", strconv.FormatBool(res.WholeFile))
	w.Header().Set("X-Sandboxd-Binary", strconv.FormatBool(res.Binary))
	w.Header().Set("X-Sandboxd-Total-Size", strconv.FormatInt(res.Size, 10))
	w.Header().Set("X-Sandboxd-Range-Offset", strconv.FormatInt(res.Offset, 10))
	w.Header().Set("X-Sandboxd-Range-Length", strconv.FormatInt(res.Length, 10))
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Data)
}

func (s *Server) serveFileLines(w http.ResponseWriter, ws *model.Workspace, path string, q url.Values, encoding string) {
	startLine, err := queryInt64(q, "start_line", 1)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	endLine, err := queryInt64(q, "end_line", 0)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if startLine < 1 {
		writeErrorWithCode(w, model.Wrap(422, model.CodeUnprocessable, "start_line must be >= 1", nil))
		return
	}
	if endLine != 0 && endLine < startLine {
		writeErrorWithCode(w, model.Wrap(422, model.CodeUnprocessable, "end_line must be >= start_line", nil))
		return
	}
	res, err := s.svc.ReadFileLines(ws, path, startLine, endLine)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	binary := isBinarySample(res.Data)
	lineCount := res.EndLine - res.StartLine + 1
	if res.EndLine < res.StartLine {
		lineCount = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":        path,
		"content":     encodeContent(res.Data, encoding, binary),
		"encoding":    contentEncoding(encoding, binary),
		"start_line":  res.StartLine,
		"end_line":    res.EndLine,
		"line_count":  lineCount,
		"total_lines": res.TotalLines,
		"sha256":      res.SHA256,
		"truncated":   res.Truncated,
		"binary":      binary,
	})
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	rc, info, err := s.svc.OpenFile(ws, path)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	defer rc.Close()
	if info.ContentType == "" {
		info.ContentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Disposition", service.ContentDisposition(path))
	w.Header().Set("X-Sandboxd-Truncated", "false")
	w.Header().Set("X-Sandboxd-Whole-File", "true")
	if info.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// putFileRequest uses a pointer for content so an absent field is
// distinguishable from an explicit empty string: the former is a 400, the
// latter a valid 0-byte write.
type putFileRequest struct {
	Content  *string `json:"content"`
	Encoding string  `json:"encoding"`
}

func (s *Server) handlePutFileContent(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	var req putFileRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if req.Content == nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "content is required", nil))
		return
	}
	data, err := decodeContent(*req.Content, req.Encoding)
	if err != nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "invalid base64 content", nil))
		return
	}
	if err := s.svc.WriteFile(ws, path, data, req.Encoding); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	if err := s.svc.DeleteFile(ws, path); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type mkdirRequest struct {
	Path string `json:"path" validate:"required"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req mkdirRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := s.svc.Mkdir(ws, req.Path); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": req.Path})
}

type renameRequest struct {
	From string `json:"from" validate:"required"`
	To   string `json:"to" validate:"required"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req renameRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := s.svc.Rename(ws, req.From, req.To); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Caps.UploadBytes())
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if ok := asMaxBytes(err, &maxErr); ok {
			writeErrorWithCode(w, model.New(413, model.CodePayloadTooLarge, "upload too large"))
			return
		}
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "invalid multipart form", map[string]string{"error": err.Error()}))
		return
	}
	path := r.FormValue("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "file is required", nil))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, s.cfg.Caps.UploadBytes()))
	if err != nil {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "read upload failed", nil))
		return
	}
	if err := s.svc.UploadFile(ws, path, data); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": path,
		"size": len(data),
		"name": header.Filename,
	})
}

// encodeContent renders data for the JSON wrapper. Binary content is always
// base64-encoded so it is never mangled into invalid UTF-8 JSON, regardless of
// the requested encoding.
func encodeContent(data []byte, encoding string, binary bool) string {
	if encoding == "base64" || binary {
		return base64.StdEncoding.EncodeToString(data)
	}
	return string(data)
}

func decodeContent(content, encoding string) ([]byte, error) {
	if encoding == "base64" {
		return base64.StdEncoding.DecodeString(content)
	}
	return []byte(content), nil
}

func contentEncoding(encoding string, binary bool) string {
	if encoding == "base64" || binary {
		return "base64"
	}
	return "text"
}

// isBinarySample applies the bounded binary rule to already-read bytes.
func isBinarySample(b []byte) bool {
	return workspace.IsBinary(b)
}

// queryInt parses an optional non-negative integer query parameter.
func queryInt(q url.Values, name string, def int) (int, error) {
	if !q.Has(name) {
		return def, nil
	}
	v, err := strconv.Atoi(q.Get(name))
	if err != nil || v < 0 {
		return 0, model.Wrap(422, model.CodeUnprocessable, name+" must be a non-negative integer", map[string]string{"param": name})
	}
	return v, nil
}

// queryInt64 parses an optional non-negative integer query parameter.
func queryInt64(q url.Values, name string, def int64) (int64, error) {
	if !q.Has(name) {
		return def, nil
	}
	v, err := strconv.ParseInt(q.Get(name), 10, 64)
	if err != nil || v < 0 {
		return 0, model.Wrap(422, model.CodeUnprocessable, name+" must be a non-negative integer", map[string]string{"param": name})
	}
	return v, nil
}

func asMaxBytes(err error, target **http.MaxBytesError) bool {
	if maxErr, ok := err.(*http.MaxBytesError); ok {
		*target = maxErr
		return true
	}
	return false
}
