package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

const cacheBody = `{"model":"claude-haiku-4-5-20251001","stream":true}`

// cacheHandler builds a proxy over a counting upstream and the given cache.
func cacheHandler(t *testing.T, fc *fakeCache) (*Handler, *atomic.Int32, func()) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithCache(fc))
	return h, &calls, upstream.Close
}

func postCC(h *Handler, path, body, cacheControl string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if cacheControl != "" {
		req.Header.Set("Cache-Control", cacheControl)
	}
	rec := httptest.NewRecorder()
	if path == "/v1/messages" {
		h.Messages(rec, req)
	} else {
		h.ChatCompletions(rec, req)
	}
	return rec
}

func TestCacheControl_NoStore(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, calls, done := cacheHandler(t, fc)
	defer done()

	rec := postCC(h, "/v1/messages", cacheBody, "no-store")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fc.sets != 0 {
		t.Errorf("no-store should not write the cache, sets = %d", fc.sets)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
	// The key is still surfaced so a client can act on it.
	if rec.Header().Get("x-llm-cache-key") == "" {
		t.Error("x-llm-cache-key should be set even on no-store")
	}
}

func TestCacheControl_NoCache(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, calls, done := cacheHandler(t, fc)
	defer done()

	// Prime the cache with a normal request.
	if rec := post(h, "/v1/messages", cacheBody); rec.Code != http.StatusOK {
		t.Fatalf("prime status = %d", rec.Code)
	}
	if fc.sets != 1 || calls.Load() != 1 {
		t.Fatalf("after prime: sets=%d calls=%d, want 1 and 1", fc.sets, calls.Load())
	}

	// no-cache bypasses the read and hits the upstream again, then re-stores.
	rec := postCC(h, "/v1/messages", cacheBody, "no-cache")
	if rec.Header().Get("x-llm-cache") != "miss" {
		t.Errorf("no-cache x-llm-cache = %q, want miss", rec.Header().Get("x-llm-cache"))
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2 (no-cache forced fresh)", calls.Load())
	}
	if fc.sets != 2 {
		t.Errorf("no-cache should still store, sets = %d, want 2", fc.sets)
	}
}

func TestCacheControl_TTLDirective(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, _, done := cacheHandler(t, fc)
	defer done()

	if rec := postCC(h, "/v1/messages", cacheBody, "ttl=600"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fc.lastTTL != 600*time.Second {
		t.Errorf("stored ttl = %v, want 10m from the ttl directive", fc.lastTTL)
	}
}

func TestCacheControl_SMaxAgeStaleMiss(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, calls, done := cacheHandler(t, fc)
	defer done()

	key := cache.Key("", provider.ShapeAnthropic, "anthropic", "claude-haiku-4-5-20251001", []byte(cacheBody))
	fc.store[key] = &cache.Entry{
		Status:      http.StatusOK,
		ContentType: "text/event-stream",
		Body:        []byte(anthropicSSE),
		CreatedAt:   time.Now().Add(-time.Hour).Unix(),
	}

	rec := postCC(h, "/v1/messages", cacheBody, "s-maxage=60")
	if rec.Header().Get("x-llm-cache") != "miss" {
		t.Errorf("stale entry under s-maxage should miss, got %q", rec.Header().Get("x-llm-cache"))
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (stale entry bypassed)", calls.Load())
	}
}

func TestCacheControl_SMaxAgeFreshHit(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, calls, done := cacheHandler(t, fc)
	defer done()

	key := cache.Key("", provider.ShapeAnthropic, "anthropic", "claude-haiku-4-5-20251001", []byte(cacheBody))
	fc.store[key] = &cache.Entry{
		Status:      http.StatusOK,
		ContentType: "text/event-stream",
		Body:        []byte(anthropicSSE),
		CreatedAt:   time.Now().Unix(),
	}

	rec := postCC(h, "/v1/messages", cacheBody, "s-maxage=60")
	if rec.Header().Get("x-llm-cache") != "hit" {
		t.Errorf("fresh entry under s-maxage should hit, got %q", rec.Header().Get("x-llm-cache"))
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0 (served from fresh cache)", calls.Load())
	}
}

func TestCacheControl_KeyHeaderConsistent(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, _, done := cacheHandler(t, fc)
	defer done()

	miss := post(h, "/v1/messages", cacheBody)
	hit := post(h, "/v1/messages", cacheBody)
	k1 := miss.Header().Get("x-llm-cache-key")
	k2 := hit.Header().Get("x-llm-cache-key")
	if k1 == "" || k2 == "" {
		t.Fatalf("cache key header missing: miss=%q hit=%q", k1, k2)
	}
	if k1 != k2 {
		t.Errorf("cache key should be stable across miss and hit: %q vs %q", k1, k2)
	}
}

func TestParseCacheControl(t *testing.T) {
	tests := []struct {
		header string
		want   cacheControl
	}{
		{"", cacheControl{}},
		{"no-store", cacheControl{noStore: true}},
		{"no-cache", cacheControl{noCache: true}},
		{"s-maxage=30", cacheControl{sMaxAge: 30 * time.Second, hasSMaxAge: true}},
		{"ttl=600", cacheControl{ttl: 600 * time.Second}},
		{"no-cache, ttl=120", cacheControl{noCache: true, ttl: 120 * time.Second}},
		{"NO-STORE", cacheControl{noStore: true}},
		{"s-maxage=notanumber", cacheControl{}},
		{"ttl=0", cacheControl{}}, // non-positive ttl is ignored
	}
	for _, tc := range tests {
		got := parseCacheControl(http.Header{"Cache-Control": {tc.header}})
		if got != tc.want {
			t.Errorf("parseCacheControl(%q) = %+v, want %+v", tc.header, got, tc.want)
		}
	}
}

func TestEntryFresh(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name string
		e    *cache.Entry
		cc   cacheControl
		want bool
	}{
		{"no s-maxage always fresh", &cache.Entry{CreatedAt: 0}, cacheControl{}, true},
		{"within bound", &cache.Entry{CreatedAt: now}, cacheControl{sMaxAge: 60 * time.Second, hasSMaxAge: true}, true},
		{"past bound", &cache.Entry{CreatedAt: now - 3600}, cacheControl{sMaxAge: 60 * time.Second, hasSMaxAge: true}, false},
	}
	for _, tc := range cases {
		if got := entryFresh(tc.e, tc.cc); got != tc.want {
			t.Errorf("%s: entryFresh = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeAdminCache exercises the cache admin handlers.
type fakeAdminCache struct {
	pingErr error
	delErr  error
	deleted []string
}

func (f *fakeAdminCache) Ping(context.Context) error { return f.pingErr }
func (f *fakeAdminCache) Delete(_ context.Context, key string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func TestCacheAdmin_Ping(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ok := NewCacheAdmin(&fakeAdminCache{}, log)
	rec := httptest.NewRecorder()
	ok.Ping(rec, httptest.NewRequest(http.MethodGet, "/cache/ping", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthy ping status = %d, want 200", rec.Code)
	}

	down := NewCacheAdmin(&fakeAdminCache{pingErr: io.ErrUnexpectedEOF}, log)
	rec = httptest.NewRecorder()
	down.Ping(rec, httptest.NewRequest(http.MethodGet, "/cache/ping", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy ping status = %d, want 503", rec.Code)
	}
}

func TestCacheAdmin_Delete(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fac := &fakeAdminCache{}
	admin := NewCacheAdmin(fac, log)

	// Valid delete.
	rec := httptest.NewRecorder()
	admin.Delete(rec, httptest.NewRequest(http.MethodPost, "/cache/delete", strings.NewReader(`{"key":"abc123"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rec.Code)
	}
	if len(fac.deleted) != 1 || fac.deleted[0] != "abc123" {
		t.Errorf("deleted = %v, want [abc123]", fac.deleted)
	}

	// Missing key is a 400.
	rec = httptest.NewRecorder()
	admin.Delete(rec, httptest.NewRequest(http.MethodPost, "/cache/delete", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty-key status = %d, want 400", rec.Code)
	}

	// Backend error is a 500.
	bad := NewCacheAdmin(&fakeAdminCache{delErr: io.ErrClosedPipe}, log)
	rec = httptest.NewRecorder()
	bad.Delete(rec, httptest.NewRequest(http.MethodPost, "/cache/delete", strings.NewReader(`{"key":"x"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("backend-error status = %d, want 500", rec.Code)
	}
}
