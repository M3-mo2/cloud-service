package api

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// multipartArchive builds a multipart/form-data body with the given archive
// bytes and form fields.
func multipartArchive(t *testing.T, field string, data []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(field, "bundle.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *harness) doMultipart(path, key string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest("POST", path, body)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestUploadArchiveEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch")
	data := zipBytes(t, map[string]string{
		"src/main.py":   "print(1)\n",
		"src/empty.txt": "",
		"src/uni-📦.txt": "u",
	})
	body, ct := multipartArchive(t, "archive", data, map[string]string{"path": "/"})
	rec := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		EntriesExtracted int    `json:"entries_extracted"`
		EntriesRejected  int    `json:"entries_rejected"`
		BytesWritten     int64  `json:"bytes_written"`
		Format           string `json:"format"`
		DryRun           bool   `json:"dry_run"`
	}](t, rec)
	if resp.EntriesExtracted != 3 || resp.EntriesRejected != 0 {
		t.Fatalf("unexpected counts: %+v", resp)
	}
	if resp.Format != "zip" || resp.BytesWritten == 0 || resp.DryRun {
		t.Fatalf("unexpected result: %+v", resp)
	}
	// The files must be visible through the normal file API.
	stat := h.do("GET", "/v1/workspaces/"+ws+"/files/stat?path=/src/main.py", reqOpts{key: keyA})
	if stat.Code != 200 {
		t.Fatalf("main.py not written: %d %s", stat.Code, stat.Body.String())
	}
}

func TestUploadArchiveDryRunEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch-dry")
	data := zipBytes(t, map[string]string{"a.txt": "aaaa"})
	body, ct := multipartArchive(t, "archive", data, map[string]string{"path": "/", "dry_run": "true"})
	rec := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		DryRun       bool  `json:"dry_run"`
		BytesWritten int64 `json:"bytes_written"`
	}](t, rec)
	if !resp.DryRun || resp.BytesWritten != 0 {
		t.Fatalf("dry run wrote: %+v", resp)
	}
	// Nothing written.
	stat := h.do("GET", "/v1/workspaces/"+ws+"/files/stat?path=/a.txt", reqOpts{key: keyA})
	if stat.Code != 404 {
		t.Fatalf("dry run wrote a.txt: %d", stat.Code)
	}
}

func TestUploadArchiveUnsupportedEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch-bad")
	body, ct := multipartArchive(t, "archive", []byte("definitely not a zip"), nil)
	rec := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415 body=%s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "unsupported_archive" {
		t.Fatalf("code=%q", code)
	}
}

func TestUploadArchiveTraversalEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch-trav")
	data := zipBytes(t, map[string]string{"../escape.txt": "pwned"})
	body, ct := multipartArchive(t, "archive", data, nil)
	rec := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if rec.Code != 422 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "archive_unsafe_entry" {
		t.Fatalf("code=%q", code)
	}
}

func TestExtractEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("extract")
	// First upload the archive as a regular file.
	data := zipBytes(t, map[string]string{"inner/a.txt": "hello"})
	upBody, upCT := multipartArchive(t, "file", data, nil)
	up := h.doMultipart("/v1/workspaces/"+ws+"/files/upload?path=/bundle.zip", keyA, upBody, upCT)
	if up.Code != 201 {
		t.Fatalf("upload: %d %s", up.Code, up.Body.String())
	}
	rec := h.doJSON("POST", "/v1/workspaces/"+ws+"/files/extract", keyA, map[string]any{
		"path": "/bundle.zip", "dest": "/out",
	})
	if rec.Code != 200 {
		t.Fatalf("extract: %d %s", rec.Code, rec.Body.String())
	}
	stat := h.do("GET", "/v1/workspaces/"+ws+"/files/stat?path=/out/inner/a.txt", reqOpts{key: keyA})
	if stat.Code != 200 {
		t.Fatalf("extracted file missing: %d %s", stat.Code, stat.Body.String())
	}
}

func TestExportArchiveEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("export")
	// Write a small tree.
	for _, p := range []struct{ path, body string }{
		{"/proj/main.go", "package main\n"},
		{"/proj/sub/x.txt", "x\n"},
	} {
		rec := h.doJSON("PUT", "/v1/workspaces/"+ws+"/files/content?path="+p.path, keyA, map[string]any{"content": p.body})
		if rec.Code != 204 {
			t.Fatalf("write %s: %d %s", p.path, rec.Code, rec.Body.String())
		}
	}
	rec := h.do("GET", "/v1/workspaces/"+ws+"/files/archive?path=/proj", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type=%q", ct)
	}
	// The downloaded zip must be a readable zip containing the tree.
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("export is not a zip: %v", err)
	}
	// The tree has one file, one nested dir and one nested file.
	if len(zr.File) != 3 {
		t.Fatalf("zip has %d entries want 3", len(zr.File))
	}
}

func TestArchiveOverwriteEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch-ow")
	// Seed an existing file.
	rec := h.doJSON("PUT", "/v1/workspaces/"+ws+"/files/content?path=/a.txt", keyA, map[string]any{"content": "old"})
	if rec.Code != 204 {
		t.Fatalf("seed: %d", rec.Code)
	}
	data := zipBytes(t, map[string]string{"a.txt": "new"})
	// overwrite=false (default) -> 409 archive_entry_exists.
	body, ct := multipartArchive(t, "archive", data, map[string]string{"overwrite": "false"})
	r1 := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if r1.Code != 409 {
		t.Fatalf("overwrite=false: %d %s", r1.Code, r1.Body.String())
	}
	if code := errCode(t, r1); code != "archive_entry_exists" {
		t.Fatalf("code=%q", code)
	}
	// overwrite=true -> replaces.
	body2, ct2 := multipartArchive(t, "archive", data, map[string]string{"overwrite": "true"})
	r2 := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body2, ct2)
	if r2.Code != 200 {
		t.Fatalf("overwrite=true: %d %s", r2.Code, r2.Body.String())
	}
	got := h.do("GET", "/v1/workspaces/"+ws+"/files/content?path=/a.txt", reqOpts{key: keyA})
	if got.Body.String() != "new" {
		t.Fatalf("content=%q want new", got.Body.String())
	}
}

func TestMetaAdvertisesArchives(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{key: keyA})
	body := decode[map[string]any](t, rec)
	cap, ok := body["archives"].(map[string]any)
	if !ok {
		t.Fatalf("archives capability missing: %v", body)
	}
	if cap["enabled"] != true {
		t.Fatalf("archives.enabled = %v", cap["enabled"])
	}
	formats, ok := cap["formats"].([]any)
	if !ok || len(formats) != 2 {
		t.Fatalf("archives.formats = %v", cap["formats"])
	}
	limits, ok := cap["limits"].(map[string]any)
	if !ok {
		t.Fatalf("archives.limits missing: %v", cap)
	}
	// The advertised limits must be the code's real defaults.
	if limits["max_entries"] != float64(20000) || limits["max_total_mb"] != float64(2048) {
		t.Fatalf("archives.limits = %v", limits)
	}
	codes, ok := cap["refusal_codes"].([]any)
	if !ok || len(codes) != 8 {
		t.Fatalf("archives.refusal_codes = %v", cap["refusal_codes"])
	}
}

func TestArchiveDisabledEndpoint(t *testing.T) {
	h := newHarness(t)
	ws := h.createWorkspace("arch-off")
	h.cfg.Archives.Enabled = false
	h.rebuild()
	data := zipBytes(t, map[string]string{"a.txt": "x"})
	body, ct := multipartArchive(t, "archive", data, nil)
	rec := h.doMultipart("/v1/workspaces/"+ws+"/files/archive", keyA, body, ct)
	if rec.Code != 422 {
		t.Fatalf("disabled archive: %d %s", rec.Code, rec.Body.String())
	}
}
