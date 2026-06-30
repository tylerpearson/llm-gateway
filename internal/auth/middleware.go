package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/tylerpearson/llm-gateway/internal/store"
)

// Lookup is the subset of the store the authenticator needs. Narrowing the
// dependency keeps the middleware easy to test with a fake.
type Lookup interface {
	LookupKeyByHash(ctx context.Context, keyHash string) (*store.VirtualKey, error)
}

// Authenticator authenticates requests against a key store, caching positive
// lookups for a short TTL to keep the hot path off the database.
//
// The cache stores the resolved key, so a key disabled in the store keeps
// authenticating until its cached entry expires. The TTL is therefore the
// revocation SLA: after gatewayctl disables a key, this instance may still
// accept it for up to ttl. Keep the TTL short, or set it to a small value via
// config, when fast revocation matters more than database load.
//
// The cache also stores negative entries (key == nil) for hashes the store
// reports as store.ErrNotFound. Without this, a flood of invalid or garbage
// API keys turns into one store lookup per request: an unauthenticated
// database-amplification path. Negative entries share the positive TTL and
// the same entry cap, so the cache stays bounded under either kind of flood.
type Authenticator struct {
	store Lookup
	log   *slog.Logger
	ttl   time.Duration

	mu         sync.RWMutex
	cache      map[string]cacheEntry
	maxEntries int
}

type cacheEntry struct {
	// key is nil for a negative entry: a hash known, as of expires, not to
	// resolve to any virtual key.
	key     *store.VirtualKey
	expires time.Time
}

// DefaultCacheTTL is the default lifetime of a cached key lookup, and so the
// default upper bound on how long a disabled key keeps working after it is
// revoked. Kept short to bound that revocation window; operators who need
// stricter revocation can lower it via config. The same TTL bounds negative
// (not-found) entries.
const DefaultCacheTTL = 5 * time.Second

// maxCacheEntries bounds the lookup cache so a flood of unique invalid or
// valid keys cannot grow the map without limit. It applies to positive and
// negative entries combined.
const maxCacheEntries = 50000

// New builds an Authenticator. A ttl of 0 uses DefaultCacheTTL.
func New(s Lookup, log *slog.Logger, ttl time.Duration) *Authenticator {
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	return &Authenticator{
		store:      s,
		log:        log,
		ttl:        ttl,
		cache:      make(map[string]cacheEntry),
		maxEntries: maxCacheEntries,
	}
}

// Middleware authenticates the request, attaches the Principal to the context,
// and rejects unauthenticated or disabled keys with 401.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := extractKey(r)
		if raw == "" {
			a.deny(w, r, "missing API key")
			return
		}
		hash := HashKey(raw)

		vk, err := a.lookup(r.Context(), hash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				a.deny(w, r, "invalid API key")
				return
			}
			a.log.Error("auth lookup failed",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.Any("error", err),
			)
			a.fail(w, http.StatusServiceUnavailable, "service_unavailable", "authentication backend unavailable")
			return
		}
		if vk.Disabled {
			a.deny(w, r, "disabled API key")
			return
		}

		p := &Principal{
			KeyID:        vk.ID,
			KeyName:      vk.Name,
			TeamID:       vk.TeamID,
			DefaultAlias: vk.DefaultAlias,
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// lookup checks the cache (positive or negative), then the store, caching
// the result for ttl. Only store.ErrNotFound is cached negatively; any other
// store error (for example a backend outage) is returned uncached so the
// next request retries the store and the failure keeps surfacing as 503.
func (a *Authenticator) lookup(ctx context.Context, hash string) (*store.VirtualKey, error) {
	a.mu.RLock()
	entry, ok := a.cache[hash]
	a.mu.RUnlock()
	if ok && time.Now().Before(entry.expires) {
		if entry.key == nil {
			return nil, store.ErrNotFound
		}
		return entry.key, nil
	}

	vk, err := a.store.LookupKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.setCache(hash, nil)
		}
		return nil, err
	}

	a.setCache(hash, vk)
	return vk, nil
}

// setCache inserts a cache entry for hash, enforcing maxEntries. A nil key
// records a negative (not-found) entry. If the cache is at or over capacity,
// expired entries are swept first; if it is still at capacity afterward, the
// entry is dropped and the lookup falls back to the store next time rather
// than letting the map grow without bound.
func (a *Authenticator) setCache(hash string, vk *store.VirtualKey) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.cache) >= a.maxEntries {
		a.sweepExpiredLocked()
		if len(a.cache) >= a.maxEntries {
			return
		}
	}
	a.cache[hash] = cacheEntry{key: vk, expires: time.Now().Add(a.ttl)}
}

// sweepExpiredLocked removes expired entries. Callers must hold a.mu.
func (a *Authenticator) sweepExpiredLocked() {
	now := time.Now()
	for h, e := range a.cache {
		if now.After(e.expires) {
			delete(a.cache, h)
		}
	}
}

// extractKey reads the virtual key from the Anthropic (x-api-key) or OpenAI
// (Authorization: Bearer) header.
func extractKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// deny logs a redacted auth failure and returns 401. The key is never logged.
func (a *Authenticator) deny(w http.ResponseWriter, r *http.Request, reason string) {
	a.log.Warn("authentication rejected",
		slog.String("request_id", middleware.GetReqID(r.Context())),
		slog.String("reason", reason),
	)
	a.fail(w, http.StatusUnauthorized, "authentication_error", reason)
}

func (a *Authenticator) fail(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}
