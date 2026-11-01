package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sandboxd/internal/auth"
	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/service"
	"sandboxd/internal/storage"
)

const (
	keyA = "key-a"
	keyB = "key-b"
	usrA = "user_a"
	usrB = "user_b"
)

type harness struct {
	t       *testing.T
	server  *Server
	router  http.Handler
	store   *storage.Store
	runner  *executor.FakeRunner
	svc     *service.Service
	cfg     config.Config
	dataDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, t.TempDir())
}

// newHarnessAt builds a harness over an existing data directory, so a test can
// simulate a service restart sharing persisted state.
func newHarnessAt(t *testing.T, dataDir string) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DataDir = dataDir
	cfg.Sandbox.NsjailPath = "/nonexistent/nsjail"
	cfg.Caps.JSONBodyMB = 1
	cfg.Caps.FileWriteMB = 1
	// The session-start reap window is a short startup probe; keep it small in
	// tests so a healthy (never-exiting) fake session does not slow the suite.
	cfg.Sessions.StartProbeMS = 20

	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	fake := &executor.FakeRunner{AvailableFlag: false, CgroupsFlag: true, Stdout: "hi\n"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(cfg, store, fake, logger)
	authn := auth.New([]config.KeyConfig{
		{Key: keyA, PrincipalID: usrA},
		{Key: keyB, PrincipalID: usrB},
	})
	srv := NewServer(svc, authn, logger)
	// Ensure background run goroutines finish before TempDir cleanup runs.
	// t.Cleanup is LIFO, so these execute before the TempDir removal. Live
	// sessions are killed first (bounded), then runs are awaited.
	t.Cleanup(svc.WaitAll)
	t.Cleanup(func() { svc.ShutdownSessions(2 * time.Second) })
	return &harness{
		t:       t,
		server:  srv,
		router:  srv.Router(),
		store:   store,
		runner:  fake,
		svc:     svc,
		cfg:     cfg,
		dataDir: cfg.Storage.DataDir,
	}
}

// rebuild reconstructs the service, server and router over the current
// harness state. It is used when a test mutates h.cfg after construction (the
// service copies the config), so config-dependent behaviour can be exercised.
func (h *harness) rebuild() {
	h.t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(h.cfg, h.store, h.runner, logger)
	authn := auth.New([]config.KeyConfig{
		{Key: keyA, PrincipalID: usrA},
		{Key: keyB, PrincipalID: usrB},
	})
	srv := NewServer(svc, authn, logger)
	h.svc = svc
	h.server = srv
	h.router = srv.Router()
	h.t.Cleanup(svc.WaitAll)
}

type reqOpts struct {
	key     string
	body    []byte
	headers map[string]string
}

func (h *harness) do(method, path string, opts reqOpts) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if opts.body != nil {
		body = bytes.NewReader(opts.body)
	}
	req := httptest.NewRequest(method, path, body)
	if opts.key != "" {
		req.Header.Set("Authorization", "Bearer "+opts.key)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range opts.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *harness) doJSON(method, path, key string, payload any) *httptest.ResponseRecorder {
	h.t.Helper()
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	return h.do(method, path, reqOpts{key: key, body: body})
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return v
}

// createWorkspace creates a workspace as usrA and returns its ID.
func (h *harness) createWorkspace(name string) string {
	h.t.Helper()
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": name})
	if rec.Code != 201 {
		h.t.Fatalf("create workspace: %d %s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}](h.t, rec)
	return resp.Workspace.ID
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	env := decode[errorEnvelope](t, rec)
	return env.Error.Code
}
