package main

import (
	"strings"
	"testing"
)

func TestDSNFromEnv(t *testing.T) {
	t.Run("flag takes precedence", func(t *testing.T) {
		t.Setenv("MYSQL_DSN", "env-dsn")
		got, err := dsnFromEnv("flag-dsn")
		if err != nil {
			t.Fatalf("dsnFromEnv: %v", err)
		}
		if got != "flag-dsn" {
			t.Errorf("dsn = %q, want flag-dsn", got)
		}
	})

	t.Run("falls back to env", func(t *testing.T) {
		t.Setenv("MYSQL_DSN", "env-dsn")
		got, err := dsnFromEnv("")
		if err != nil {
			t.Fatalf("dsnFromEnv: %v", err)
		}
		if got != "env-dsn" {
			t.Errorf("dsn = %q, want env-dsn", got)
		}
	})

	t.Run("error when neither set", func(t *testing.T) {
		t.Setenv("MYSQL_DSN", "")
		if _, err := dsnFromEnv(""); err == nil {
			t.Fatal("expected error when MYSQL_DSN unset and no --dsn")
		}
	})
}

func TestActor_NeverEmpty(t *testing.T) {
	// actor falls back to a stable label when the OS user cannot be resolved,
	// so audit records always carry a non-empty actor.
	if got := actor(); got == "" {
		t.Error("actor() returned empty string")
	}
}

// TestCommandValidation exercises the argument-validation paths that must
// return an error before any database connection is attempted. Each case has
// no MYSQL_DSN configured, so reaching the store would surface as a different
// error; a clean validation error proves the guard fires first.
func TestCommandValidation(t *testing.T) {
	cases := []struct {
		name    string
		fn      func([]string) error
		args    []string
		wantSub string
	}{
		{"team no subcommand", cmdTeam, nil, "team <create|list>"},
		{"team unknown subcommand", cmdTeam, []string{"frobnicate"}, "team <create|list>"},
		{"team create missing name", cmdTeam, []string{"create"}, "team create <name>"},
		{"key no subcommand", cmdKey, nil, "key <create|list>"},
		{"key unknown subcommand", cmdKey, []string{"frobnicate"}, "key <create|list|disable>"},
		{"key create missing flags", cmdKey, []string{"create"}, "--team and --name"},
		{"key create missing name", cmdKey, []string{"create", "--team", "t1"}, "--team and --name"},
		{"key list missing team", cmdKey, []string{"list"}, "requires --team"},
		{"key disable missing id", cmdKey, []string{"disable"}, "requires --id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MYSQL_DSN", "")
			err := tc.fn(tc.args)
			if err == nil {
				t.Fatalf("%s: expected validation error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestDSNGuardBeforeStore confirms commands that pass argument validation still
// fail cleanly when no DSN is configured, rather than panicking or dialing a
// nonexistent default.
func TestDSNGuardBeforeStore(t *testing.T) {
	t.Setenv("MYSQL_DSN", "")
	cases := []struct {
		name string
		fn   func([]string) error
		args []string
	}{
		{"team create", cmdTeam, []string{"create", "acme"}},
		{"team list", cmdTeam, []string{"list"}},
		{"key create", cmdKey, []string{"create", "--team", "t1", "--name", "k1"}},
		{"key list", cmdKey, []string{"list", "--team", "t1"}},
		{"key disable", cmdKey, []string{"disable", "--id", "k1"}},
		{"audit", cmdAudit, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn(tc.args)
			if err == nil {
				t.Fatalf("%s: expected DSN error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "MYSQL_DSN") {
				t.Errorf("error = %q, want it to mention MYSQL_DSN", err.Error())
			}
		})
	}
}
