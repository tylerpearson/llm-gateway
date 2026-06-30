//go:build integration

// These tests need a real MySQL instance. They run only under the integration
// build tag and skip unless MYSQL_DSN points at a reachable database. The CI
// integration job provides one via a service container.
package mysql

import (
	"context"
	"os"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/store"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping MySQL integration test")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMySQLRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	team, err := s.CreateTeam(ctx, "team-"+store.NewID())
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	plaintext, hash := auth.GenerateKey()
	vk, err := s.CreateKey(ctx, team.ID, "ci-key", hash, "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if vk.TeamID != team.ID {
		t.Errorf("key team = %q, want %q", vk.TeamID, team.ID)
	}

	got, err := s.LookupKeyByHash(ctx, auth.HashKey(plaintext))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != vk.ID || got.DefaultAlias != "default" || got.Disabled {
		t.Errorf("lookup returned %+v, unexpected", got)
	}

	if _, err := s.LookupKeyByHash(ctx, auth.HashKey("llmgw_does_not_exist")); err != store.ErrNotFound {
		t.Errorf("missing key err = %v, want ErrNotFound", err)
	}

	keys, err := s.ListKeys(ctx, team.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list keys = %v (len %d), err %v; want 1", keys, len(keys), err)
	}

	teams, err := s.ListTeams(ctx)
	if err != nil || len(teams) == 0 {
		t.Fatalf("list teams err %v len %d", err, len(teams))
	}

	// Disable the key and confirm the lookup reflects it.
	if err := s.DisableKey(ctx, vk.ID); err != nil {
		t.Fatalf("disable key: %v", err)
	}
	after, err := s.LookupKeyByHash(ctx, auth.HashKey(plaintext))
	if err != nil {
		t.Fatalf("lookup after disable: %v", err)
	}
	if !after.Disabled {
		t.Error("key should be disabled after DisableKey")
	}
	if err := s.DisableKey(ctx, "no-such-key"); err != store.ErrNotFound {
		t.Errorf("disable unknown key err = %v, want ErrNotFound", err)
	}
}

func TestMySQLAuditLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target := store.NewID()
	if err := s.RecordAudit(ctx, "tester", "key.create", target, "name=ci"); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	entries, err := s.ListAudit(ctx, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Target == target && e.Action == "key.create" && e.Actor == "tester" {
			found = true
		}
	}
	if !found {
		t.Error("recorded audit entry not found in ListAudit")
	}
}
