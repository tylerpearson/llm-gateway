package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfig writes contents to a temp file and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret-key")
	t.Setenv("REDIS_ADDR", "localhost:6379")

	path := writeConfig(t, `
server:
  addr: ":9090"
  read_timeout: 5s
  read_header_timeout: 3s
  idle_timeout: 60s
logging:
  level: debug
  format: text
providers:
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
routing:
  default_alias: default
  aliases:
    default:
      provider: anthropic
      model: claude-haiku-4-5-20251001
storage:
  redis_addr_env: REDIS_ADDR
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("addr = %q, want :9090", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 5*time.Second {
		t.Errorf("read_timeout = %v, want 5s", cfg.Server.ReadTimeout)
	}
	if cfg.Server.ReadHeaderTimeout != 3*time.Second {
		t.Errorf("read_header_timeout = %v, want 3s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.IdleTimeout != 60*time.Second {
		t.Errorf("idle_timeout = %v, want 60s", cfg.Server.IdleTimeout)
	}
	if got := cfg.Providers["anthropic"].APIKey; got != "secret-key" {
		t.Errorf("api key = %q, want secret-key (env override)", got)
	}
	if cfg.Storage.RedisAddr != "localhost:6379" {
		t.Errorf("redis addr = %q, want localhost:6379", cfg.Storage.RedisAddr)
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, "logging:\n  level: info\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != defaultAddr {
		t.Errorf("addr = %q, want default %q", cfg.Server.Addr, defaultAddr)
	}
	if cfg.Server.ReadTimeout != defaultReadTimeout {
		t.Errorf("read_timeout = %v, want default %v", cfg.Server.ReadTimeout, defaultReadTimeout)
	}
	if cfg.Server.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Errorf("read_header_timeout = %v, want default %v", cfg.Server.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
	if cfg.Server.IdleTimeout != defaultIdleTimeout {
		t.Errorf("idle_timeout = %v, want default %v", cfg.Server.IdleTimeout, defaultIdleTimeout)
	}
	if cfg.Server.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("shutdown_timeout = %v, want default %v", cfg.Server.ShutdownTimeout, defaultShutdownTimeout)
	}
	if cfg.Logging.Format != defaultLogFormat {
		t.Errorf("format = %q, want default %q", cfg.Logging.Format, defaultLogFormat)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{"invalid log level", "logging:\n  level: verbose\n"},
		{"invalid log format", "logging:\n  format: xml\n"},
		{"unknown field", "server:\n  nope: true\n"},
		{"default alias missing", "routing:\n  default_alias: ghost\n"},
		{"alias unknown provider", `
routing:
  aliases:
    fast:
      provider: missing
      model: x
`},
		{"alias missing model", `
providers:
  anthropic:
    type: anthropic
routing:
  aliases:
    fast:
      provider: anthropic
`},
		{"fallback unknown provider", `
providers:
  anthropic:
    type: anthropic
routing:
  aliases:
    default:
      provider: anthropic
      model: claude
      fallbacks:
        - provider: missing
          model: x
`},
		{"fallback missing model", `
providers:
  anthropic:
    type: anthropic
routing:
  aliases:
    default:
      provider: anthropic
      model: claude
      fallbacks:
        - provider: anthropic
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.contents)
			if _, err := Load(path); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestRedactPromptsDefaultsTrue(t *testing.T) {
	path := writeConfig(t, "logging:\n  level: info\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RedactPrompts() {
		t.Error("RedactPrompts should default to true")
	}

	path2 := writeConfig(t, "security:\n  redact_prompts: false\n")
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.RedactPrompts() {
		t.Error("RedactPrompts should be false when explicitly disabled")
	}
}

func TestLimitsModeDefaultAndValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.Mode != "soft" {
		t.Errorf("limits.mode default = %q, want soft", cfg.Limits.Mode)
	}
	if _, err := Load(writeConfig(t, "limits:\n  mode: nuke\n")); err == nil {
		t.Error("expected error for invalid limits.mode")
	}
}

func TestResilienceDefaultsAndDetection(t *testing.T) {
	base := `
providers:
  anthropic:
    type: anthropic
  openai:
    type: openai
routing:
  default_alias: default
  aliases:
    default:
      provider: anthropic
      model: claude
`

	t.Run("absent when no failover configured", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, base))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Routing.FailoverConfigured() {
			t.Error("FailoverConfigured should be false without fallbacks or a resilience block")
		}
		if cfg.Routing.Resilience.MaxRetries != 0 {
			t.Errorf("max_retries = %d, want 0 (defaults not applied)", cfg.Routing.Resilience.MaxRetries)
		}
	})

	t.Run("declaring fallbacks fills defaults", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, base+`      fallbacks:
        - provider: openai
          model: gpt-4o-mini
`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.Routing.FailoverConfigured() {
			t.Fatal("FailoverConfigured should be true when fallbacks are declared")
		}
		res := cfg.Routing.Resilience
		if res.MaxRetries != 2 {
			t.Errorf("max_retries default = %d, want 2", res.MaxRetries)
		}
		if res.RetryBackoff != 200*time.Millisecond {
			t.Errorf("retry_backoff default = %v, want 200ms", res.RetryBackoff)
		}
		if res.Cooldown != 30*time.Second {
			t.Errorf("cooldown default = %v, want 30s", res.Cooldown)
		}
		if res.CooldownThreshold != 5 {
			t.Errorf("cooldown_threshold default = %d, want 5", res.CooldownThreshold)
		}
		want := []int{429, 500, 502, 503, 504}
		if len(res.RetryableStatus) != len(want) {
			t.Fatalf("retryable_status = %v, want %v", res.RetryableStatus, want)
		}
		for i := range want {
			if res.RetryableStatus[i] != want[i] {
				t.Errorf("retryable_status[%d] = %d, want %d", i, res.RetryableStatus[i], want[i])
			}
		}
	})

	t.Run("context check fills its own defaults when enabled", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, base+`  resilience:
    context_check:
      enabled: true
`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cc := cfg.Routing.Resilience.ContextCheck
		if !cc.Enabled {
			t.Fatal("context_check.enabled should be true")
		}
		if cc.CharsPerToken != 4 {
			t.Errorf("chars_per_token default = %d, want 4", cc.CharsPerToken)
		}
		if cc.SafetyMargin != 0.15 {
			t.Errorf("safety_margin default = %v, want 0.15", cc.SafetyMargin)
		}
		// Enabling only the context check does not turn on failover retries.
		if cfg.Routing.Resilience.MaxRetries != 0 {
			t.Errorf("max_retries = %d, want 0 (context check is independent of failover)", cfg.Routing.Resilience.MaxRetries)
		}
	})

	t.Run("explicit values are preserved", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, base+`  resilience:
    max_retries: 1
    request_timeout: 5s
    retryable_status: [503]
`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		res := cfg.Routing.Resilience
		if res.MaxRetries != 1 {
			t.Errorf("max_retries = %d, want 1", res.MaxRetries)
		}
		if res.RequestTimeout != 5*time.Second {
			t.Errorf("request_timeout = %v, want 5s", res.RequestTimeout)
		}
		if len(res.RetryableStatus) != 1 || res.RetryableStatus[0] != 503 {
			t.Errorf("retryable_status = %v, want [503]", res.RetryableStatus)
		}
	})
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
