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
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/provider/openai"
	"github.com/tylerpearson/llm-gateway/internal/proxy"
	"github.com/tylerpearson/llm-gateway/internal/ratelimit"
	"github.com/tylerpearson/llm-gateway/internal/router"
	"github.com/tylerpearson/llm-gateway/internal/server"
	"github.com/tylerpearson/llm-gateway/internal/store/clickhouse"
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

	// Cost attribution writes one row per request to ClickHouse when configured.
	var proxyOpts []proxy.Option
	if cfg.Storage.ClickHouseDSN != "" {
		ch, err := clickhouse.Open(cfg.Storage.ClickHouseDSN)
		if err != nil {
			return fmt.Errorf("open analytics store: %w", err)
		}
		writer := attribution.NewWriter(ch, log, attribution.Options{})
		defer func() {
			writer.Close()
			_ = ch.Close()
		}()
		proxyOpts = append(proxyOpts, proxy.WithAttribution(writer, pricing.DefaultTable()))
		log.Info("request attribution enabled", slog.String("sink", "clickhouse"))
	} else {
		log.Warn("attribution disabled: CLICKHOUSE_DSN not configured")
	}

	// Exact-match response cache (Redis) when configured.
	if cfg.Storage.RedisAddr != "" {
		c, err := cache.New(cfg.Storage.RedisAddr, cache.DefaultTTL, cache.DefaultMaxBytes, log)
		if err != nil {
			return fmt.Errorf("open response cache: %w", err)
		}
		defer func() { _ = c.Close() }()
		proxyOpts = append(proxyOpts, proxy.WithCache(c))
		log.Info("response cache enabled", slog.String("sink", "redis"))

		lim, err := ratelimit.New(cfg.Storage.RedisAddr, limitSettings(cfg.Limits), log)
		if err != nil {
			return fmt.Errorf("open rate limiter: %w", err)
		}
		defer func() { _ = lim.Close() }()
		proxyOpts = append(proxyOpts, proxy.WithRateLimit(lim))
		log.Info("rate limiting enabled", slog.String("mode", cfg.Limits.Mode))
	} else {
		log.Warn("response cache and rate limiting disabled: REDIS_ADDR not configured")
	}

	providers := buildProviders(cfg.Providers, log)

	var routeFns []func(chi.Router)
	if len(providers) > 0 {
		shapes := make(map[string]provider.Shape, len(providers))
		for name, p := range providers {
			shapes[name] = p.Shape()
		}
		rtr := router.New(cfg.Routing, shapes)
		h := proxy.New(providers, rtr, log, proxyOpts...)
		routeFns = append(routeFns, func(r chi.Router) {
			r.Group(func(gr chi.Router) {
				if authMW != nil {
					gr.Use(authMW)
				}
				gr.Post("/v1/messages", h.Messages)
				gr.Post("/v1/chat/completions", h.ChatCompletions)
			})
		})
		log.Info("mounted proxy endpoints", slog.Int("providers", len(providers)))
	} else {
		log.Warn("no providers configured; proxy endpoints not mounted")
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

// buildProviders constructs upstream adapters from config. anthropic uses the
// Anthropic adapter; openai and glm both use the OpenAI compatible adapter (GLM
// exposes an OpenAI shaped endpoint).
func buildProviders(pcs map[string]config.Provider, log *slog.Logger) provider.Registry {
	reg := provider.Registry{}
	for name, pc := range pcs {
		switch pc.Type {
		case "anthropic":
			reg[name] = anthropic.New(name, pc.BaseURL, pc.APIKey)
		case "openai", "glm":
			reg[name] = openai.New(name, pc.BaseURL, pc.APIKey)
		default:
			log.Warn("unknown provider type; skipping",
				slog.String("name", name), slog.String("type", pc.Type))
		}
	}
	return reg
}

// limitSettings maps config limits to the ratelimit package settings.
func limitSettings(c config.Limits) ratelimit.Settings {
	conv := func(s config.LimitSet) ratelimit.Limits {
		return ratelimit.Limits{
			RequestsPerMin: s.RequestsPerMin,
			TokensPerMin:   s.TokensPerMin,
			MonthlyUSD:     s.MonthlyUSD,
		}
	}
	return ratelimit.Settings{
		Mode:    ratelimit.Mode(c.Mode),
		PerKey:  conv(c.PerKey),
		PerTeam: conv(c.PerTeam),
	}
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
