package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestMiddleware_CachesNegativeLookups verifies that repeated requests with
// the same unknown key hit the store once: the negative cache entry answers
// every request inside the TTL window without calling the store, which is
// the fix for the invalid-key database amplification finding.
func TestMiddleware_CachesNegativeLookups(t *testing.T) {
	look := &fakeLookup{keys: map[string]*store.VirtualKey{}}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", "llmgw_invalid")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status %d, want 401", i, rec.Code)
		}
	}
	if look.calls != 1 {
		t.Errorf("store lookups = %d, want 1 (negative cache hit)", look.calls)
	}
}

// TestMiddleware_BackendErrorNotCached verifies that a non-ErrNotFound store
// error (for example a backend outage) is never cached: every request must
// retry the store and keep surfacing as 503, unlike a genuinely invalid key.
func TestMiddleware_BackendErrorNotCached(t *testing.T) {
	look := &fakeLookup{err: errors.New("db down")}
	h := testAuth(t, look).Middleware(http.HandlerFunc(principalEcho))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", "llmgw_x")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d: status %d, want 503", i, rec.Code)
		}
	}
	if look.calls != 2 {
		t.Errorf("store lookups = %d, want 2 (no caching of backend errors)", look.calls)
	}
}

// TestAuthenticator_CacheCapBounded verifies that flooding the authenticator
// with unique key hashes, positive or negative, cannot grow the cache beyond
// its configured cap.
func TestAuthenticator_CacheCapBounded(t *testing.T) {
	const limit = 10
	look := &fakeLookup{keys: map[string]*store.VirtualKey{}}
	a := New(look, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	a.maxEntries = limit

	for i := 0; i < limit*5; i++ {
		hash := HashKey(fmt.Sprintf("llmgw_unique_%d", i))
		if _, err := a.lookup(context.Background(), hash); err == nil {
			t.Fatalf("lookup %d: want ErrNotFound, got nil error", i)
		}
	}

	a.mu.RLock()
	n := len(a.cache)
	a.mu.RUnlock()
	if n > limit {
		t.Errorf("cache size = %d, want <= %d", n, limit)
	}
}
