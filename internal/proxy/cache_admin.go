package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// AdminCache is the subset of the response cache the admin endpoints need. The
// full cache.Cache satisfies it.
type AdminCache interface {
	Ping(ctx context.Context) error
	Delete(ctx context.Context, key string) error
}

// CacheAdmin serves the operational cache endpoints: a health probe and a
// delete-by-key. Callers learn a request's key from the x-llm-cache-key response
// header, then pass it here to evict a specific stored response.
type CacheAdmin struct {
	cache AdminCache
	log   *slog.Logger
}

// NewCacheAdmin builds a CacheAdmin over the given cache.
func NewCacheAdmin(c AdminCache, log *slog.Logger) *CacheAdmin {
	return &CacheAdmin{cache: c, log: log}
}

// Ping reports cache backend health. GET /cache/ping.
func (a *CacheAdmin) Ping(w http.ResponseWriter, r *http.Request) {
	if err := a.cache.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Delete evicts a single cache entry by key. POST /cache/delete with a JSON body
// of {"key": "..."}.
func (a *CacheAdmin) Delete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `request body must be JSON with a non-empty "key"`})
		return
	}
	if err := a.cache.Delete(r.Context(), req.Key); err != nil {
		a.log.Warn("cache delete failed", slog.String("key", req.Key), slog.Any("error", err))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cache delete failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "key": req.Key})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
