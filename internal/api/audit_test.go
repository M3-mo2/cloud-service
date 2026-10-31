package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"sandboxd/internal/auth"
	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/service"
	"sandboxd/internal/storage"
)

func TestAuditLogIncludesPrincipal(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := config.Default()
	cfg.Storage.DataDir = t.TempDir()
	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &executor.FakeRunner{AvailableFlag: true}
	svc := service.New(cfg, store, fake, logger)
	a := auth.New([]config.KeyConfig{{Key: "k", PrincipalID: "user_z"}})
	router := NewServer(svc, a, logger).Router()

	req := httptest.NewRequest("GET", "/v1/meta", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == "request" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("no request audit line in %q", buf.String())
	}
	if found["principal_id"] != "user_z" {
		t.Fatalf("principal_id = %v (line=%v)", found["principal_id"], found)
	}
	if found["method"] != "GET" || found["path"] != "/v1/meta" {
		t.Fatalf("line = %v", found)
	}
}
