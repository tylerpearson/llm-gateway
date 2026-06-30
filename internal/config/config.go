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
}

// Server holds HTTP listener settings.
type Server struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
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
}

// Route is a concrete provider and model target for an alias.
type Route struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
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
	defaultAddr            = ":8080"
	defaultReadTimeout     = 30 * time.Second
	defaultWriteTimeout    = 0 // streaming responses must not be cut off
	defaultShutdownTimeout = 15 * time.Second
	defaultLogLevel        = "info"
	defaultLogFormat       = "json"
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
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = defaultWriteTimeout
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
	}
	return nil
}
