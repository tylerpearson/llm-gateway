// Command gateway is the llm-gateway server entrypoint. It loads config, builds
// the HTTP server, and runs it until an interrupt triggers a graceful shutdown.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/proxy"
	"github.com/tylerpearson/llm-gateway/internal/server"
	"github.com/tylerpearson/llm-gateway/internal/store/mysql"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to the gateway config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		slog.Error("gateway exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.Logging)
	slog.SetDefault(log)

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Virtual key auth is enabled when a config store is configured. Without
	// one the gateway runs unauthenticated, which is acceptable only for local
	// development, so the absence is logged loudly.
	var authMW func(http.Handler) http.Handler
	if cfg.Storage.MySQLDSN != "" {
		st, err := mysql.Open(cfg.Storage.MySQLDSN)
		if err != nil {
			return fmt.Errorf("open config store: %w", err)
		}
		defer func() { _ = st.Close() }()
		authMW = auth.New(st, log, 0).Middleware
		log.Info("virtual key auth enabled")
	} else {
		log.Warn("AUTH DISABLED: MYSQL_DSN not configured, /v1/messages is unauthenticated (development only)")
	}

	providers := buildProviders(cfg.Providers, log)

	var routeFns []func(chi.Router)
	if msgProvider := selectMessagesProvider(cfg, providers); msgProvider != nil {
		h := proxy.New(msgProvider, log)
		routeFns = append(routeFns, func(r chi.Router) {
			r.Group(func(gr chi.Router) {
				if authMW != nil {
					gr.Use(authMW)
				}
				gr.Post("/v1/messages", h.Messages)
			})
		})
		log.Info("mounted /v1/messages", slog.String("provider", msgProvider.Name()))
	} else {
		log.Warn("no anthropic provider configured; /v1/messages not mounted")
	}

	srv := server.New(cfg.Server, log, reg, routeFns...)

	// Remaining startup wiring (stores, caches, rate limits) lands in later
	// phases. Once wiring completes the gateway is ready to take traffic.
	srv.SetReady(true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// buildProviders constructs upstream adapters from config. Only the anthropic
// adapter exists in P1; openai and glm are wired in P4.
func buildProviders(pcs map[string]config.Provider, log *slog.Logger) provider.Registry {
	reg := provider.Registry{}
	for name, pc := range pcs {
		switch pc.Type {
		case "anthropic":
			reg[name] = anthropic.New(name, pc.BaseURL, pc.APIKey)
		case "openai", "glm":
			log.Debug("provider type not yet implemented; skipping",
				slog.String("name", name), slog.String("type", pc.Type))
		default:
			log.Warn("unknown provider type; skipping",
				slog.String("name", name), slog.String("type", pc.Type))
		}
	}
	return reg
}

// selectMessagesProvider picks the anthropic-shaped provider that serves
// /v1/messages in P1. It prefers the provider behind the default alias, then
// falls back to the first anthropic provider by name. Full alias and tier
// routing arrives in P4.
func selectMessagesProvider(cfg *config.Config, reg provider.Registry) provider.Provider {
	if cfg.Routing.DefaultAlias != "" {
		if route, ok := cfg.Routing.Aliases[cfg.Routing.DefaultAlias]; ok {
			if pc, ok := cfg.Providers[route.Provider]; ok && pc.Type == "anthropic" {
				if p, ok := reg.Get(route.Provider); ok {
					return p
				}
			}
		}
	}
	names := make([]string, 0, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		if pc.Type == "anthropic" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		if p, ok := reg.Get(names[0]); ok {
			return p
		}
	}
	return nil
}

func newLogger(cfg config.Logging) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
