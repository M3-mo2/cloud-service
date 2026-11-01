package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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

func TestListFileNotDirectory(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.py", keyA, map[string]any{"content": "x"})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/f.py", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("listing a file code = %d body=%s", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "not_directory" {
		t.Fatalf("code = %q", c)
	}
}

func TestPutOverDirectoryRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/d"})
	rec := h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/d", keyA, map[string]any{"content": "x"})
	if rec.Code != 422 {
		t.Fatalf("put over dir code = %d body=%s", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "not_directory" {
		t.Fatalf("code = %q", c)
	}
}

func TestPutRequiresContent(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	// Missing content field -> 400.
	rec := h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a", keyA, map[string]any{})
	if rec.Code != 400 {
		t.Fatalf("missing content code = %d body=%s", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "validation" {
		t.Fatalf("code = %q", c)
	}
	// Explicit empty content is a valid 0-byte write.
	rec = h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a", keyA, map[string]any{"content": ""})
	if rec.Code != 204 {
		t.Fatalf("empty content code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadDirectoryRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/d"})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/d", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("read dir code = %d body=%s", rec.Code, rec.Body.String())
	}
	if c := errCode(t, rec); c != "not_directory" {
		t.Fatalf("code = %q", c)
	}
}

func TestListSortedWithMetadataAndPaging(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/b.txt", keyA, map[string]any{"content": "bb"})
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "a"})
	h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/dir"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/", keyA, nil)
	var got struct {
		Entries   []entryJSON `json:"entries"`
		Count     int         `json:"count"`
		Total     int         `json:"total"`
		Truncated bool        `json:"truncated"`
		Path      string      `json:"path"`
	}
	got = decode[struct {
		Entries   []entryJSON `json:"entries"`
		Count     int         `json:"count"`
		Total     int         `json:"total"`
		Truncated bool        `json:"truncated"`
		Path      string      `json:"path"`
	}](t, rec)
	if got.Total != 3 || got.Count != 3 || got.Truncated {
		t.Fatalf("listing = %+v", got)
	}
	if got.Entries[0].Name != "dir" || got.Entries[0].Type != "dir" {
		t.Fatalf("first entry = %+v", got.Entries[0])
	}
	if got.Entries[1].Name != "a.txt" || got.Entries[2].Name != "b.txt" {
		t.Fatalf("order = %+v", got.Entries)
	}
	for _, e := range got.Entries {
		if e.Mode == "" || e.ModTime == "" {
			t.Fatalf("entry missing metadata: %+v", e)
		}
	}

	// Paging: limit=1 gives the first entry with truncated=true and total=3.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files?path=/&limit=1&offset=0", keyA, nil)
	page := decode[struct {
		Entries   []entryJSON `json:"entries"`
		Total     int         `json:"total"`
		Truncated bool        `json:"truncated"`
	}](t, rec)
	if len(page.Entries) != 1 || page.Total != 3 || !page.Truncated || page.Entries[0].Name != "dir" {
		t.Fatalf("page = %+v", page)
	}
}

func TestStatEndpoint(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/a.txt", keyA, map[string]any{"content": "hello"})
	h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/d"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/a.txt", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stat code = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
		Mode   string `json:"mode"`
		SHA256 string `json:"sha256"`
	}
	got = decode[struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
		Mode   string `json:"mode"`
		SHA256 string `json:"sha256"`
	}](t, rec)
	if got.Type != "file" || got.Size != 5 || got.Name != "a.txt" || got.Mode == "" || got.SHA256 == "" {
		t.Fatalf("stat = %+v", got)
	}

	// Directory stat is a 200 with type=dir.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/d", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("dir stat code = %d", rec.Code)
	}
	if decode[statJSON](t, rec).Type != "dir" {
		t.Fatalf("dir stat type")
	}

	// Missing path -> 404.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/missing", keyA, nil)
	if rec.Code != 404 {
		t.Fatalf("missing stat code = %d", rec.Code)
	}
}

func TestRangeReadHonestHash(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.txt", keyA, map[string]any{"content": "0123456789"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&offset=4&limit=3&meta=true", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("range code = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Content   string `json:"content"`
		Size      int64  `json:"size"`
		TotalSize int64  `json:"total_size"`
		Range     struct {
			Offset int64 `json:"offset"`
			Length int64 `json:"length"`
		} `json:"range"`
		WholeFile bool   `json:"whole_file"`
		SHA256    string `json:"sha256"`
		Truncated bool   `json:"truncated"`
	}
	got = decode[struct {
		Content   string `json:"content"`
		Size      int64  `json:"size"`
		TotalSize int64  `json:"total_size"`
		Range     struct {
			Offset int64 `json:"offset"`
			Length int64 `json:"length"`
		} `json:"range"`
		WholeFile bool   `json:"whole_file"`
		SHA256    string `json:"sha256"`
		Truncated bool   `json:"truncated"`
	}](t, rec)
	if got.Content != "456" || got.Range.Offset != 4 || got.Range.Length != 3 || got.TotalSize != 10 {
		t.Fatalf("range = %+v", got)
	}
	if got.WholeFile || !got.Truncated {
		t.Fatalf("range flags = %+v", got)
	}
	partSum := sha256.Sum256([]byte("456"))
	if got.SHA256 != hex.EncodeToString(partSum[:]) {
		t.Fatalf("partial hash = %s", got.SHA256)
	}
	fullSum := sha256.Sum256([]byte("0123456789"))
	if got.SHA256 == hex.EncodeToString(fullSum[:]) {
		t.Fatal("partial hash must not equal full hash")
	}

	// A whole-file read reports whole_file=true and the file hash.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&meta=true", keyA, nil)
	full := decode[struct {
		Content   string `json:"content"`
		WholeFile bool   `json:"whole_file"`
		SHA256    string `json:"sha256"`
		Truncated bool   `json:"truncated"`
	}](t, rec)
	if !full.WholeFile || full.Truncated {
		t.Fatalf("full flags = %+v", full)
	}
	if full.SHA256 != hex.EncodeToString(fullSum[:]) {
		t.Fatalf("full hash = %s", full.SHA256)
	}
}

func TestRangeReadBadParams(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.txt", keyA, map[string]any{"content": "abc"})

	// Negative offset.
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&offset=-1", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("negative offset code = %d", rec.Code)
	}
	// Conflicting byte and line params.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&offset=0&start_line=1", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("conflict code = %d", rec.Code)
	}
	// Bad line range.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&start_line=3&end_line=1", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("bad line range code = %d", rec.Code)
	}
}

func TestRangeReadLimitAboveCapRejected(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	// Lower the read cap and rebuild so the limit cap bites.
	h.cfg.Caps.FileReadMB = 1
	h.rebuild()
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.txt", keyA, map[string]any{"content": "abc"})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&limit=2000000", keyA, nil)
	if rec.Code != 413 {
		t.Fatalf("limit above cap code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLineRangeRead(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/f.txt", keyA,
		map[string]any{"content": "one\ntwo\nthree\nfour\n"})
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/f.txt&start_line=2&end_line=3", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("lines code = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Content    string `json:"content"`
		StartLine  int64  `json:"start_line"`
		EndLine    int64  `json:"end_line"`
		LineCount  int64  `json:"line_count"`
		TotalLines int64  `json:"total_lines"`
	}
	got = decode[struct {
		Content    string `json:"content"`
		StartLine  int64  `json:"start_line"`
		EndLine    int64  `json:"end_line"`
		LineCount  int64  `json:"line_count"`
		TotalLines int64  `json:"total_lines"`
	}](t, rec)
	if got.Content != "two\nthree\n" || got.StartLine != 2 || got.EndLine != 3 || got.LineCount != 2 || got.TotalLines != 4 {
		t.Fatalf("lines = %+v", got)
	}
}

func TestBinaryDetectionAndEncoding(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	// Write binary via base64 so it round-trips.
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/b.dat", keyA,
		map[string]any{"content": base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02}), "encoding": "base64"})

	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/b.dat&meta=true", keyA, nil)
	var got struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Binary   bool   `json:"binary"`
	}
	got = decode[struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Binary   bool   `json:"binary"`
	}](t, rec)
	if !got.Binary || got.Encoding != "base64" {
		t.Fatalf("binary meta = %+v", got)
	}
	raw, err := base64.StdEncoding.DecodeString(got.Content)
	if err != nil || !bytes.Equal(raw, []byte{0x00, 0x01, 0x02}) {
		t.Fatalf("binary content = %v err=%v", raw, err)
	}

	// The raw (non-meta) read exposes the signal in a header.
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/content?path=/b.dat", keyA, nil)
	if rec.Header().Get("X-Sandboxd-Binary") != "true" {
		t.Fatalf("binary header = %q", rec.Header().Get("X-Sandboxd-Binary"))
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte{0x00, 0x01, 0x02}) {
		t.Fatalf("raw binary body = %v", rec.Body.Bytes())
	}
}

func TestDownloadFileExemptFromReadCap(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	// A file larger than the read cap, written directly to bypass the write caps.
	big := bytes.Repeat([]byte("A"), 3*1024*1024)
	if err := os.WriteFile(h.store.WorkspaceRoot(id)+"/big.bin", big, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/download?path=/big.bin", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("download code = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != len(big) {
		t.Fatalf("download size = %d want %d", rec.Body.Len(), len(big))
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(len(big)) {
		t.Fatalf("content-length = %q", rec.Header().Get("Content-Length"))
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "big.bin") {
		t.Fatalf("content-disposition = %q", rec.Header().Get("Content-Disposition"))
	}
	// A directory path is a 422.
	h.doJSON("POST", "/v1/workspaces/"+id+"/files/mkdir", keyA, map[string]any{"path": "/d"})
	rec = h.doJSON("GET", "/v1/workspaces/"+id+"/files/download?path=/d", keyA, nil)
	if rec.Code != 422 {
		t.Fatalf("download dir code = %d", rec.Code)
	}
}

func TestStatSymlink(t *testing.T) {
	h := newHarness(t)
	id := h.createWorkspace("files")
	h.doJSON("PUT", "/v1/workspaces/"+id+"/files/content?path=/real.txt", keyA, map[string]any{"content": "x"})
	if err := os.Symlink("real.txt", h.store.WorkspaceRoot(id)+"/link"); err != nil {
		t.Skipf("symlink: %v", err)
	}
	rec := h.doJSON("GET", "/v1/workspaces/"+id+"/files/stat?path=/link", keyA, nil)
	if rec.Code != 200 {
		t.Fatalf("stat link code = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decode[statJSON](t, rec)
	if got.Type != "symlink" || got.Target != "real.txt" || !got.TargetInside {
		t.Fatalf("symlink stat = %+v", got)
	}
}
