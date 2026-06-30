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

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
