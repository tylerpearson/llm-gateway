// Package config loads and validates gateway configuration from a YAML file
// with environment variable overrides for secrets. The schema covers the full
// v1 surface (providers, routing, storage) but only the fields needed for the
// current build phase are consumed; the rest are seams for later phases.
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top level gateway configuration.
type Config struct {
	Server    Server              `yaml:"server"`
	Logging   Logging             `yaml:"logging"`
	Providers map[string]Provider `yaml:"providers"`
	Routing   Routing             `yaml:"routing"`
	Storage   Storage             `yaml:"storage"`
	Limits    Limits              `yaml:"limits"`
	Security  Security            `yaml:"security"`
}

// Security holds hardening toggles. RedactPrompts (default true) keeps prompt
// and response content out of logs and analytics; the gateway never persists
// request or response bodies, and prompt previews are logged only when this is
// explicitly disabled. AuthCacheTTL bounds how long a disabled key keeps
// authenticating after revocation: it is the lifetime of a cached key lookup,
// and so the revocation SLA. Zero uses the auth package default.
type Security struct {
	RedactPrompts *bool         `yaml:"redact_prompts"`
	AuthCacheTTL  time.Duration `yaml:"auth_cache_ttl"`
	Guard         Guard         `yaml:"guard"`
}

// Guard configures the pre-call request guardrail. When enabled, the named guard
// inspects each request before it is sent upstream and may mask its body or
// block it. Unlike RedactPrompts (which only affects logs), a guard acts on the
// request actually sent to the provider.
type Guard struct {
	// Enabled turns request guarding on.
	Enabled bool `yaml:"enabled"`
	// Type selects the guard implementation. Currently only "regex_mask" (a
	// built-in regex PII and secret masker) is supported.
	Type string `yaml:"type"`
}

// Limits configures per key and per team budgets and rate limits. A limit of 0
// means unlimited. Mode selects soft enforcement (allow and flag) or hard
// enforcement (reject with 429).
type Limits struct {
	Mode    string   `yaml:"mode"`
	PerKey  LimitSet `yaml:"per_key"`
	PerTeam LimitSet `yaml:"per_team"`
}

// LimitSet is one identity's thresholds.
type LimitSet struct {
	RequestsPerMin int64   `yaml:"requests_per_min"`
	TokensPerMin   int64   `yaml:"tokens_per_min"`
	MonthlyUSD     float64 `yaml:"monthly_usd"`
}

// Server holds HTTP listener settings. ReadHeaderTimeout bounds how long the
// server waits to receive request headers, which closes off slowloris style
// connection exhaustion on ingress. IdleTimeout bounds how long a keep-alive
// connection may sit idle between requests. Neither field affects an
// in-progress streaming response body, so they are safe defaults even though
// WriteTimeout (if ever set) would truncate long SSE streams.
type Server struct {
	Addr              string        `yaml:"addr"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// Logging controls structured logging output.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Provider describes one upstream LLM provider. APIKey is normally injected
// from the environment via APIKeyEnv rather than written in the file.
type Provider struct {
	Type      string `yaml:"type"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"`
}

// Routing maps virtual model aliases to concrete provider plus model targets.
type Routing struct {
	DefaultAlias string           `yaml:"default_alias"`
	Aliases      map[string]Route `yaml:"aliases"`
	Resilience   Resilience       `yaml:"resilience"`
}

// Route is a concrete provider and model target for an alias. Fallbacks are
// tried in order when the primary target fails with a retryable error; they are
// ignored unless resilience is configured.
type Route struct {
	Provider  string  `yaml:"provider"`
	Model     string  `yaml:"model"`
	Fallbacks []Route `yaml:"fallbacks"`
}

// Resilience configures upstream failover: bounded retries with backoff, a
// per-attempt request timeout, and a circuit breaker that ejects a repeatedly
// failing target for a cooldown. The zero value disables failover, in which case
// each request makes a single attempt against its primary target. Failover is
// considered configured when any of these fields is set or when any alias
// declares fallbacks; sensible defaults are then filled for the rest.
type Resilience struct {
	// MaxRetries is the number of extra attempts per candidate on a retryable
	// failure, on top of the first attempt.
	MaxRetries int `yaml:"max_retries"`
	// RetryBackoff is the base delay before a retry; it grows exponentially with
	// jitter across attempts.
	RetryBackoff time.Duration `yaml:"retry_backoff"`
	// RequestTimeout bounds a single attempt's wait for the upstream response.
	// It never cuts off an in-progress streamed body. Zero means no added
	// deadline beyond the client's request context.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// Cooldown is how long an ejected target stays out of rotation.
	Cooldown time.Duration `yaml:"cooldown"`
	// CooldownThreshold is the number of consecutive failures that ejects a
	// target.
	CooldownThreshold int `yaml:"cooldown_threshold"`
	// RetryableStatus lists upstream HTTP status codes that trigger a retry or
	// failover. Any status not listed (including client errors and 2xx) is
	// relayed to the caller as is.
	RetryableStatus []int `yaml:"retryable_status"`
	// ContextCheck configures the pre-call context-window check.
	ContextCheck ContextCheck `yaml:"context_check"`
}

// ContextCheck configures the optional pre-call context-window check. When
// enabled, the gateway estimates a request's token size and skips candidate
// models whose context window cannot fit it, falling through the failover chain
// or rejecting the request when nothing fits. The estimate is conservative and
// not an exact token count.
type ContextCheck struct {
	// Enabled turns the check on.
	Enabled bool `yaml:"enabled"`
	// CharsPerToken is the divisor used to approximate tokens from the request
	// body's character count. Defaults to 4.
	CharsPerToken int `yaml:"chars_per_token"`
	// SafetyMargin inflates the estimate by this fraction to bias toward
	// over-estimating. Defaults to 0.15.
	SafetyMargin float64 `yaml:"safety_margin"`
}

// Storage holds backend connection settings. DSNs are resolved from the
// environment when the matching *Env field is set.
type Storage struct {
	MySQLDSNEnv      string `yaml:"mysql_dsn_env"`
	MySQLDSN         string `yaml:"-"`
	ClickHouseDSNEnv string `yaml:"clickhouse_dsn_env"`
	ClickHouseDSN    string `yaml:"-"`
	RedisAddrEnv     string `yaml:"redis_addr_env"`
	RedisAddr        string `yaml:"-"`
}

// Default values applied when the file omits a field.
const (
	defaultAddr              = ":8080"
	defaultReadTimeout       = 30 * time.Second
	defaultReadHeaderTimeout = 10 * time.Second
	defaultWriteTimeout      = 0 // streaming responses must not be cut off
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownTimeout   = 15 * time.Second
	defaultLogLevel          = "info"
	defaultLogFormat         = "json"
)

// Load reads, parses, applies defaults and env overrides, and validates the
// config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = defaultAddr
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = defaultReadTimeout
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = defaultWriteTimeout
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = defaultIdleTimeout
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.Logging.Level == "" {
		c.Logging.Level = defaultLogLevel
	}
	if c.Logging.Format == "" {
		c.Logging.Format = defaultLogFormat
	}
	if c.Limits.Mode == "" {
		c.Limits.Mode = "soft"
	}
	if c.Security.RedactPrompts == nil {
		t := true
		c.Security.RedactPrompts = &t
	}
	c.applyResilienceDefaults()
}

// defaultRetryableStatus is the failover trigger set applied when resilience is
// configured but the field is left empty: rate limiting and the standard 5xx
// upstream failures.
var defaultRetryableStatus = []int{429, 500, 502, 503, 504}

// FailoverConfigured reports whether the routing config asks for upstream
// failover, either through an explicit resilience block or by declaring
// fallbacks on any alias.
func (r Routing) FailoverConfigured() bool {
	res := r.Resilience
	if res.MaxRetries > 0 || res.RetryBackoff > 0 || res.RequestTimeout > 0 ||
		res.Cooldown > 0 || res.CooldownThreshold > 0 || len(res.RetryableStatus) > 0 {
		return true
	}
	for _, route := range r.Aliases {
		if len(route.Fallbacks) > 0 {
			return true
		}
	}
	return false
}

// applyResilienceDefaults fills sensible defaults for the resilience block.
// Failover defaults apply once failover is configured; context-check defaults
// apply once the check is enabled, independently. RequestTimeout intentionally
// stays at its configured value (zero by default) so a streamed response is
// never cut off by a timeout the operator did not ask for.
func (c *Config) applyResilienceDefaults() {
	res := &c.Routing.Resilience
	if c.Routing.FailoverConfigured() {
		if res.MaxRetries == 0 {
			res.MaxRetries = 2
		}
		if res.RetryBackoff == 0 {
			res.RetryBackoff = 200 * time.Millisecond
		}
		if res.Cooldown == 0 {
			res.Cooldown = 30 * time.Second
		}
		if res.CooldownThreshold == 0 {
			res.CooldownThreshold = 5
		}
		if len(res.RetryableStatus) == 0 {
			res.RetryableStatus = append([]int(nil), defaultRetryableStatus...)
		}
	}
	if res.ContextCheck.Enabled {
		if res.ContextCheck.CharsPerToken == 0 {
			res.ContextCheck.CharsPerToken = 4
		}
		if res.ContextCheck.SafetyMargin == 0 {
			res.ContextCheck.SafetyMargin = 0.15
		}
	}
}

// RedactPrompts reports whether prompt and response content must be kept out of
// logs. It defaults to true.
func (c *Config) RedactPrompts() bool {
	return c.Security.RedactPrompts == nil || *c.Security.RedactPrompts
}

// applyEnv resolves secrets and connection strings from the environment so they
// never need to live in the config file or in version control.
func (c *Config) applyEnv() {
	for name, p := range c.Providers {
		if p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
		}
		c.Providers[name] = p
	}
	if c.Storage.MySQLDSNEnv != "" {
		c.Storage.MySQLDSN = os.Getenv(c.Storage.MySQLDSNEnv)
	}
	if c.Storage.ClickHouseDSNEnv != "" {
		c.Storage.ClickHouseDSN = os.Getenv(c.Storage.ClickHouseDSNEnv)
	}
	if c.Storage.RedisAddrEnv != "" {
		c.Storage.RedisAddr = os.Getenv(c.Storage.RedisAddrEnv)
	}
}

func (c *Config) validate() error {
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid logging.level %q", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		return fmt.Errorf("invalid logging.format %q", c.Logging.Format)
	}
	switch c.Limits.Mode {
	case "soft", "hard":
	default:
		return fmt.Errorf("invalid limits.mode %q", c.Limits.Mode)
	}
	// Routing is optional during early phases, but if a default alias is named
	// it must resolve to a defined alias that names a defined provider.
	if c.Routing.DefaultAlias != "" {
		route, ok := c.Routing.Aliases[c.Routing.DefaultAlias]
		if !ok {
			return fmt.Errorf("routing.default_alias %q has no matching alias", c.Routing.DefaultAlias)
		}
		if _, ok := c.Providers[route.Provider]; !ok {
			return fmt.Errorf("routing alias %q references unknown provider %q", c.Routing.DefaultAlias, route.Provider)
		}
	}
	for name, route := range c.Routing.Aliases {
		if route.Provider == "" || route.Model == "" {
			return fmt.Errorf("routing alias %q must set provider and model", name)
		}
		if _, ok := c.Providers[route.Provider]; !ok {
			return fmt.Errorf("routing alias %q references unknown provider %q", name, route.Provider)
		}
		for i, fb := range route.Fallbacks {
			if fb.Provider == "" || fb.Model == "" {
				return fmt.Errorf("routing alias %q fallback %d must set provider and model", name, i)
			}
			if _, ok := c.Providers[fb.Provider]; !ok {
				return fmt.Errorf("routing alias %q fallback %d references unknown provider %q", name, i, fb.Provider)
			}
		}
	}
	if c.Routing.Resilience.MaxRetries < 0 {
		return fmt.Errorf("routing.resilience.max_retries must not be negative")
	}
	if c.Routing.Resilience.CooldownThreshold < 0 {
		return fmt.Errorf("routing.resilience.cooldown_threshold must not be negative")
	}
	if c.Security.Guard.Enabled {
		switch c.Security.Guard.Type {
		case "", "regex_mask":
		default:
			return fmt.Errorf("invalid security.guard.type %q", c.Security.Guard.Type)
		}
	}
	return nil
}
