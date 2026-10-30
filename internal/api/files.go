package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"time"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

type entryJSON struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
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
	entries, err := s.svc.ListFiles(ws, path)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{
			Name:    e.Name,
			Path:    e.Path,
			Type:    e.Type,
			Size:    e.Size,
			ModTime: e.ModTime.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (s *Server) handleGetFileContent(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "path is required", nil))
		return
	}
	res, err := s.svc.ReadFile(ws, path)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	encoding := r.URL.Query().Get("encoding")
	if r.URL.Query().Get("meta") == "true" {
		body := map[string]any{
			"path":         path,
			"content":      encodeContent(res.Data, encoding),
			"encoding":     contentEncoding(encoding),
			"size":         res.Size,
			"sha256":       res.SHA256,
			"content_type": res.ContentType,
			"truncated":    res.Truncated,
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	w.Header().Set("Content-Type", res.ContentType)
	w.Header().Set("Content-Disposition", service.ContentDisposition(path))
	w.Header().Set("X-Sandboxd-Truncated", strconv.FormatBool(res.Truncated))
	if res.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(res.Data)), 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Data)
}

type putFileRequest struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
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
	data, err := decodeContent(req.Content, req.Encoding)
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

func encodeContent(data []byte, encoding string) string {
	if encoding == "base64" {
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

func contentEncoding(encoding string) string {
	if encoding == "base64" {
		return "base64"
	}
	return "text"
}

func asMaxBytes(err error, target **http.MaxBytesError) bool {
	if maxErr, ok := err.(*http.MaxBytesError); ok {
		*target = maxErr
		return true
	}
	return false
}
