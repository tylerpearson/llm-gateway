// Package server wires the gateway HTTP listener: the chi router, the standard
// middleware chain, and the operational endpoints (health, readiness, metrics).
// Provider proxy routes are added by later build phases.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tylerpearson/llm-gateway/internal/config"
)

// Server owns the HTTP listener and its lifecycle.
type Server struct {
	log        *slog.Logger
	httpServer *http.Server
	ready      atomic.Bool
}

// New builds a Server from the listener config. The Prometheus registry backs
// the /metrics endpoint; pass a dedicated registry rather than the global one
// so tests stay isolated. Each routeFn mounts additional application routes
// (for example the proxy endpoints) onto the router after the operational
// endpoints are registered.
func New(cfg config.Server, log *slog.Logger, reg *prometheus.Registry, routeFns ...func(chi.Router)) *Server {
	s := &Server{log: log}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger(log))

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	for _, fn := range routeFns {
		fn(r)
	}

	// WriteTimeout bounds the entire response, including the body, so a
	// nonzero value would truncate long running SSE streams; the gateway
	// keeps it at the configured value (commonly 0/unset, see
	// config.defaultWriteTimeout) so streaming responses are not cut off.
	// ReadHeaderTimeout guards against slowloris style connections that
	// trickle in request headers without affecting an in-flight streaming
	// response, since it only bounds the time to receive headers, not the
	// time spent writing the response body. IdleTimeout reaps keep-alive
	// connections that go idle between requests, freeing the underlying
	// socket back to the pool.
	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	return s
}

// Router exposes the configured handler. Useful for in process tests.
func (s *Server) Router() http.Handler { return s.httpServer.Handler }

// SetReady flips the readiness flag reported by /readyz. The gateway marks
// itself ready once startup wiring is complete.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Start begins serving and blocks until the listener stops. http.ErrServerClosed
// from a graceful shutdown is returned as nil.
func (s *Server) Start() error {
	s.log.Info("http server listening", slog.String("addr", s.httpServer.Addr))
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains in flight requests, bounded by ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
