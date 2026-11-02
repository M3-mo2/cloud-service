package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestHealthzAlwaysOK(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/healthz", reqOpts{})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if body := decode[map[string]string](t, rec); body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyzReportsNotReadyWithoutNsjail(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/readyz", reqOpts{})
	if rec.Code != 503 {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	if body["nsjail"] != false || body["data_dir"] != true {
		t.Fatalf("body = %v", body)
	}
	if body["status"] != "not_ready" {
		t.Fatalf("status = %v", body["status"])
	}
}

func TestReadyzReportsNotReadyWithoutCgroups(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.CgroupsFlag = false
	rec := h.do("GET", "/readyz", reqOpts{})
	if rec.Code != 503 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[map[string]any](t, rec)
	if body["cgroups"] != false || body["status"] != "not_ready" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyzReadyWithNsjail(t *testing.T) {
	h := newHarness(t)
	h.runner.AvailableFlag = true
	h.runner.CgroupsFlag = true
	rec := h.do("GET", "/readyz", reqOpts{})
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decode[map[string]any](t, rec)
	if body["status"] != "ready" || body["nsjail"] != true || body["cgroups"] != true {
		t.Fatalf("body = %v", body)
	}
}

func TestMetaRequiresAuth(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{})
	if rec.Code != 401 {
		t.Fatalf("code = %d", rec.Code)
	}
	if errCode(t, rec) != "unauthorized" {
		t.Fatalf("code = %q", errCode(t, rec))
	}
}

func TestMeta(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{key: keyA})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	if body["version"] != "0.0.17" || body["api_version"] != "v1" {
		t.Fatalf("body = %v", body)
	}
	runtimes, ok := body["runtimes"].([]any)
	if !ok || len(runtimes) != 1 || runtimes[0] != "python3" {
		t.Fatalf("runtimes = %v", body["runtimes"])
	}
	if _, ok := body["limits_defaults"].(map[string]any); !ok {
		t.Fatalf("limits_defaults missing: %v", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t)
	h.do("GET", "/v1/meta", reqOpts{key: keyA})
	rec := h.do("GET", "/metrics", reqOpts{})
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sandboxd_http_requests_total") {
		t.Fatalf("metrics missing counter:\n%s", rec.Body.String())
	}
}

func TestUnknownRoute(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/nope", reqOpts{key: keyA})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
}
