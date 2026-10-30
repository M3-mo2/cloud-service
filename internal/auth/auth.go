// Package auth provides static bearer-token authentication.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"sandboxd/internal/config"
	"sandboxd/internal/model"
)

type ctxKey int

const principalKey ctxKey = iota

// Authenticator resolves bearer keys to principals.
type Authenticator struct {
	keys map[string]string // key -> principal_id
}

// New builds an Authenticator from configured keys. Duplicate keys keep the
// first mapping.
func New(keys []config.KeyConfig) *Authenticator {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		if k.Key == "" {
			continue
		}
		if _, exists := m[k.Key]; exists {
			continue
		}
		m[k.Key] = k.PrincipalID
	}
	return &Authenticator{keys: m}
}

// Principal returns the principal id for a key. The comparison is constant
// time per candidate key to avoid timing side channels on key equality.
func (a *Authenticator) Principal(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var found string
	ok := false
	for k, pid := range a.keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			found = pid
			ok = true
		}
	}
	return found, ok
}

// Bearer is HTTP middleware that authenticates the Authorization header and
// injects the principal id into the request context.
func (a *Authenticator) Bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			writeUnauthorized(w)
			return
		}
		pid, ok := a.Principal(token)
		if !ok {
			writeUnauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, pid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PrincipalFrom returns the authenticated principal id from a context.
func PrincipalFrom(ctx context.Context) (string, bool) {
	pid, ok := ctx.Value(principalKey).(string)
	return pid, ok
}

func extractBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func writeUnauthorized(w http.ResponseWriter) {
	e := model.ErrUnauth
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(e.Status)
	_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"missing or invalid credentials"}}`))
}
