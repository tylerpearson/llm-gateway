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
type Authenticator struct {
	store Lookup
	log   *slog.Logger
	ttl   time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	key     *store.VirtualKey
	expires time.Time
}

// DefaultCacheTTL is the default lifetime of a cached key lookup, and so the
// default upper bound on how long a disabled key keeps working after it is
// revoked. Kept short to bound that revocation window; operators who need
// stricter revocation can lower it via config.
const DefaultCacheTTL = 5 * time.Second

// New builds an Authenticator. A ttl of 0 uses DefaultCacheTTL.
func New(s Lookup, log *slog.Logger, ttl time.Duration) *Authenticator {
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	return &Authenticator{
		store: s,
		log:   log,
		ttl:   ttl,
		cache: make(map[string]cacheEntry),
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

// lookup checks the positive cache, then the store, caching hits for ttl.
func (a *Authenticator) lookup(ctx context.Context, hash string) (*store.VirtualKey, error) {
	a.mu.RLock()
	entry, ok := a.cache[hash]
	a.mu.RUnlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.key, nil
	}

	vk, err := a.store.LookupKeyByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.cache[hash] = cacheEntry{key: vk, expires: time.Now().Add(a.ttl)}
	a.mu.Unlock()
	return vk, nil
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
