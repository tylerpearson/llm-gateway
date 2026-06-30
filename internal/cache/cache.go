// Package cache is the Redis backed exact-match response cache. A request is
// keyed by a hash of its tenant, client shape, resolved provider and model,
// and the canonicalized request body. The cache is scoped per tenant (team,
// falling back to key) so two different tenants issuing an identical request
// never share a cached entry. A cache hit replays the stored client-facing
// bytes (including streamed SSE) without calling the upstream, so an identical
// request from the same tenant costs nothing.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// Defaults for the cache.
const (
	DefaultTTL      = 5 * time.Minute
	DefaultMaxBytes = 1 << 20 // 1 MiB; larger responses are not cached
)

// Entry is a cached response: the exact bytes to replay to the client plus the
// metadata needed to reconstruct the response and attribute it.
type Entry struct {
	Status      int            `json:"status"`
	ContentType string         `json:"content_type"`
	Body        []byte         `json:"body"`
	Usage       provider.Usage `json:"usage"`
}

// Cache is a best-effort Redis cache: lookups and stores never fail a request,
// they just log and fall back to the upstream.
type Cache struct {
	rdb      *redis.Client
	ttl      time.Duration
	maxBytes int
	log      *slog.Logger
}

// New connects to Redis and verifies the connection.
func New(addr string, ttl time.Duration, maxBytes int, log *slog.Logger) (*Cache, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &Cache{rdb: rdb, ttl: ttl, maxBytes: maxBytes, log: log}, nil
}

// Close releases the Redis client.
func (c *Cache) Close() error { return c.rdb.Close() }

// MaxBytes is the largest response body that will be cached.
func (c *Cache) MaxBytes() int { return c.maxBytes }

// Get returns the cached entry for key, if present. Errors other than a miss
// are logged and reported as a miss.
func (c *Cache) Get(ctx context.Context, key string) (*Entry, bool) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		c.log.Warn("cache get failed", slog.Any("error", err))
		return nil, false
	}
	var e Entry
	if err := json.Unmarshal(val, &e); err != nil {
		return nil, false
	}
	return &e, true
}

// Set stores an entry under key with the configured TTL. Oversized bodies and
// errors are ignored (logged), since caching is best effort.
func (c *Cache) Set(ctx context.Context, key string, e *Entry) {
	if len(e.Body) > c.maxBytes {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, key, b, c.ttl).Err(); err != nil {
		c.log.Warn("cache set failed", slog.Any("error", err))
	}
}

// Key derives the cache key from the tenant, client shape, resolved provider
// and model, and the canonicalized request body. tenant scopes the key to a
// tenant boundary (team, falling back to key, or "" for unauthenticated or
// dev traffic) so two tenants issuing an identical request never collide.
// Canonicalization makes the key stable across insignificant whitespace and
// key ordering.
func Key(tenant string, shape provider.Shape, providerName, model string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(shape))
	h.Write([]byte{0})
	h.Write([]byte(providerName))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(canonicalJSON(body))
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalJSON(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	b, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return b
}
