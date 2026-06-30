package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/store"
)

type fakeLookup struct {
	keys  map[string]*store.VirtualKey
	err   error
	calls int
}

func (f *fakeLookup) LookupKeyByHash(_ context.Context, hash string) (*store.VirtualKey, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	vk, ok := f.keys[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return vk, nil
}

func testAuth(t *testing.T, look *fakeLookup) *Authenticator {
	t.Helper()
	return New(look, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
}

// principalEcho is a downstream handler that writes the authenticated team id,
// proving the principal reached the protected handler.
func principalEcho(w http.ResponseWriter, r *http.Request) {
	p, ok := FromContext(r.Context())
	if !ok {
		http.Error(w, "no principal", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, p.TeamID)
}

func TestMiddleware_ValidKey(t *testing.T) {
	const key = "llmgw_valid"
	look := &fakeLookup{keys: map[string]*store.VirtualKey{
		HashKey(key): {ID: "k1", TeamID: "team-1", Name: "dev"},
	}}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "team-1" {
		t.Errorf("body = %q, want team-1", rec.Body.String())
	}
}

func TestMiddleware_BearerHeader(t *testing.T) {
	const key = "llmgw_bearer"
	look := &fakeLookup{keys: map[string]*store.VirtualKey{
		HashKey(key): {ID: "k1", TeamID: "team-2"},
	}}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "team-2" {
		t.Fatalf("got %d %q, want 200 team-2", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_Rejections(t *testing.T) {
	const good = "llmgw_good"
	look := &fakeLookup{keys: map[string]*store.VirtualKey{
		HashKey(good):            {ID: "k1", TeamID: "t"},
		HashKey("llmgw_off"):     {ID: "k2", TeamID: "t", Disabled: true},
	}}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	tests := []struct {
		name   string
		set    func(*http.Request)
		status int
	}{
		{"missing key", func(*http.Request) {}, http.StatusUnauthorized},
		{"unknown key", func(r *http.Request) { r.Header.Set("x-api-key", "llmgw_nope") }, http.StatusUnauthorized},
		{"disabled key", func(r *http.Request) { r.Header.Set("x-api-key", "llmgw_off") }, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			tc.set(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

func TestMiddleware_StoreErrorIs503(t *testing.T) {
	look := &fakeLookup{err: errors.New("db down")}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "llmgw_x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestMiddleware_CachesPositiveLookups(t *testing.T) {
	const key = "llmgw_cache"
	look := &fakeLookup{keys: map[string]*store.VirtualKey{
		HashKey(key): {ID: "k1", TeamID: "t"},
	}}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	if look.calls != 1 {
		t.Errorf("store lookups = %d, want 1 (cached)", look.calls)
	}
}
