package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestFilePutGetRoundTrip(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")

	rec := h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/main.py", keyA,
		map[string]any{"content": "print('hi')\n"})
	if rec.Code != 204 {
		t.Fatalf("put code = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/main.py&meta=true", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("get code = %d", rec.Code)
	}
	var got struct {
		Content string `json:"content"`
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
	}
	got = decode[struct {
		Content string `json:"content"`
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
	}](t, rec)
	if got.Content != "print('hi')\n" || got.Size != 12 || got.SHA256 == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestFileGetRawBody(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "hello"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "a.txt") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestFileBase64RoundTrip(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	raw := []byte{0x00, 0x01, 0xff, 0xfe}
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/bin.dat", keyA,
		map[string]any{"content": base64.StdEncoding.EncodeToString(raw), "encoding": "base64"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/bin.dat&encoding=base64&meta=true", keyA, nil)
	got := decode[struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}](t, rec)
	if got.Encoding != "base64" {
		t.Fatalf("encoding = %q", got.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Content)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("decoded = %v err=%v", decoded, err)
	}
}

func TestFileListAndMkdir(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/src"})
	if rec.Code != 201 {
		t.Fatalf("mkdir code = %d", rec.Code)
	}
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/src/a.py", keyA, map[string]any{"content": "x"})

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/src", keyA, nil)
	entries := decode[struct {
		Entries []entryJSON `json:"entries"`
	}](t, rec).Entries
	if len(entries) != 1 || entries[0].Name != "a.py" || entries[0].Type != "file" {
		t.Fatalf("entries = %+v", entries)
	}

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyA, nil)
	entries = decode[struct {
		Entries []entryJSON `json:"entries"`
	}](t, rec).Entries
	if len(entries) != 1 || entries[0].Name != "src" || entries[0].Type != "dir" {
		t.Fatalf("root entries = %+v", entries)
	}
}

func TestFileDelete(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "x"})
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/files?path=/a.txt", keyA, nil)
	if rec.Code != 204 {
		t.Fatalf("delete code = %d", rec.Code)
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("after delete code = %d", rec.Code)
	}
}

func TestFileRename(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "x"})
	rec := h.doJSON("POST", "/v1/workspaces/"+id+"/files/rename", keyA,
		map[string]any{"from": "/a.txt", "to": "/b/c.txt"})
	if rec.Code != 204 {
		t.Fatalf("rename code = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/b/c.txt", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("renamed file missing: %d", rec.Code)
	}
}

func TestFileUploadMulti(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", "/uploaded.py")
	fw, _ := mw.CreateFormFile("file", "uploaded.py")
	fw.Write([]byte("print('uploaded')\n"))
	mw.Close()

	req := httptest.NewRequest("POST", "/v1/workspaces/"+id+"/files/upload", &buf)
	req.Header.Set("Authorization", "Bearer "+keyA)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload code = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/uploaded.py", keyA, nil)
	if rec.Body.String() != "print('uploaded')\n" {
		t.Fatalf("uploaded content = %q", rec.Body.String())
	}
}

func TestPathTraversalRejections(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("sec")
	base := "/v1/workspaces/" + id
	escaped := url.QueryEscape("../etc/passwd")

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"read", "GET", base + "/files/content?path=" + escaped, nil},
		{"list", "GET", base + "/files?path=" + escaped, nil},
		{"write", "PUT", base + "/files/content?path=" + escaped, map[string]any{"content": "x"}},
		{"delete", "DELETE", base + "/files?path=" + escaped, nil},
		{"mkdir", "POST", base + "/files/mkdir", map[string]any{"path": "../evil"}},
		{"rename", "POST", base + "/files/rename", map[string]any{"from": "/a", "to": "../evil"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := h.doJSON(c.method, c.path, keyA, c.body)
			if rec.Code != 422 {
				t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
			}
			if errCode(t, rec) != "unprocessable" {
				t.Fatalf("code = %q", errCode(t, rec))
			}
		})
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("sec")
	// Create a symlink inside the workspace pointing at /etc/passwd.
	linkPath := h.store.WorkspaceRoot(id) + "/passwd"
	if err := os.Symlink("/etc/passwd", linkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/passwd", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCrossOwnerFileAccessForbidden(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyB, nil)
	if rec.Code != 403 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestOversizedJSONBody(t *testing.T) {
	h := newHarness(t)
	h.cfg.Caps.JSONBodyMB = 1
	// A >1MB payload must be rejected with 413.
	big := strings.Repeat("a", (1<<20)+100)
	body, _ := json.Marshal(map[string]any{"name": big})
	rec := h.do("POST", "/v1/workspaces", reqOpts{key: keyA, body: body})
	if rec.Code != 413 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if errCode(t, rec) != "payload_too_large" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestUnsafePathErrorStatus(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	rec := h.doJSON("DELETE", "/v1/workspaces/"+id+"/files?path=/", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("deleting root should be rejected: %d", rec.Code)
	}
}
