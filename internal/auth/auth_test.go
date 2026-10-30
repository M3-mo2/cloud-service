package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sandboxd/internal/config"
)

func testAuth() *Authenticator {
	return New([]config.KeyConfig{
		{Key: "key-a", PrincipalID: "user_a"},
		{Key: "key-b", PrincipalID: "user_a"},
		{Key: "key-c", PrincipalID: "user_b"},
	})
}

func TestPrincipalLookup(t *testing.T) {
	a := testAuth()
	cases := map[string]string{
		"key-a": "user_a",
		"key-b": "user_a",
		"key-c": "user_b",
	}
	for token, want := range cases {
		got, ok := a.Principal(token)
		if !ok || got != want {
			t.Errorf("Principal(%q) = %q,%v want %q", token, got, ok, want)
		}
	}
	if _, ok := a.Principal("nope"); ok {
		t.Error("unknown key should fail")
	}
	if _, ok := a.Principal(""); ok {
		t.Error("empty key should fail")
	}
}

func TestDuplicateKeyFirstWins(t *testing.T) {
	a := New([]config.KeyConfig{
		{Key: "dup", PrincipalID: "first"},
		{Key: "dup", PrincipalID: "second"},
	})
	got, _ := a.Principal("dup")
	if got != "first" {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyKeyIgnored(t *testing.T) {
	a := New([]config.KeyConfig{{Key: "", PrincipalID: "x"}})
	if _, ok := a.Principal(""); ok {
		t.Fatal("empty key must not authenticate")
	}
}

func TestBearerMiddleware(t *testing.T) {
	a := testAuth()
	var seenPrincipal string
	h := a.Bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, ok := PrincipalFrom(r.Context())
		if !ok {
			t.Error("principal missing from context")
		}
		seenPrincipal = pid
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer key-c")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if seenPrincipal != "user_b" {
		t.Fatalf("principal = %q", seenPrincipal)
	}
}

func TestBearerMiddlewareRejects(t *testing.T) {
	a := testAuth()
	h := a.Bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called")
	}))
	cases := []string{
		"",
		"Bearer bad",
		"Bearer ",
		"Token key-a",
		"key-a",
	}
	for _, auth := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("auth=%q code = %d, want 401", auth, rec.Code)
		}
	}
}

func TestBearerCaseInsensitiveScheme(t *testing.T) {
	a := testAuth()
	h := a.Bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "bearer key-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
}
