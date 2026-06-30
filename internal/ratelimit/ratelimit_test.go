package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestLimiter(t *testing.T, s Settings) *Limiter {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	l := NewWithClient(client, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.now = func() time.Time { return time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) }
	return l
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestRequestsPerMin_Hard(t *testing.T) {
	l := newTestLimiter(t, Settings{Mode: ModeHard, PerKey: Limits{RequestsPerMin: 2}})
	id := Identity{KeyID: "k1"}
	ctx := context.Background()

	if d := l.Check(ctx, id); !d.Allowed {
		t.Fatal("request 1 should be allowed")
	}
	if d := l.Check(ctx, id); !d.Allowed {
		t.Fatal("request 2 should be allowed")
	}
	d := l.Check(ctx, id)
	if d.Allowed {
		t.Fatal("request 3 should be denied in hard mode")
	}
	if !contains(d.Exceeded, "key:requests_per_min") {
		t.Errorf("exceeded = %v, want key:requests_per_min", d.Exceeded)
	}
}

func TestRequestsPerMin_Soft(t *testing.T) {
	l := newTestLimiter(t, Settings{Mode: ModeSoft, PerKey: Limits{RequestsPerMin: 1}})
	id := Identity{KeyID: "k1"}
	ctx := context.Background()

	_ = l.Check(ctx, id)
	d := l.Check(ctx, id) // over limit
	if !d.Allowed {
		t.Error("soft mode should allow over-limit requests")
	}
	if !contains(d.Exceeded, "key:requests_per_min") {
		t.Errorf("exceeded = %v, want key:requests_per_min", d.Exceeded)
	}
}

func TestTokensPerMin(t *testing.T) {
	l := newTestLimiter(t, Settings{Mode: ModeHard, PerKey: Limits{TokensPerMin: 100}})
	id := Identity{KeyID: "k1"}
	ctx := context.Background()

	if d := l.Check(ctx, id); !d.Allowed {
		t.Fatal("should be allowed before any tokens recorded")
	}
	l.RecordUsage(ctx, id, 100, 0)
	d := l.Check(ctx, id)
	if d.Allowed || !contains(d.Exceeded, "key:tokens_per_min") {
		t.Errorf("after 100 tokens, decision = %+v, want denied tokens_per_min", d)
	}
}

func TestMonthlyUSD_Team(t *testing.T) {
	l := newTestLimiter(t, Settings{Mode: ModeHard, PerTeam: Limits{MonthlyUSD: 1.0}})
	id := Identity{TeamID: "t1"}
	ctx := context.Background()

	if d := l.Check(ctx, id); !d.Allowed {
		t.Fatal("should be allowed before any spend")
	}
	l.RecordUsage(ctx, id, 0, 1.5)
	d := l.Check(ctx, id)
	if d.Allowed || !contains(d.Exceeded, "team:monthly_usd") {
		t.Errorf("after 1.50 spend, decision = %+v, want denied team:monthly_usd", d)
	}
}

func TestNoLimitsAllowsEverything(t *testing.T) {
	l := newTestLimiter(t, Settings{Mode: ModeHard})
	id := Identity{KeyID: "k1", TeamID: "t1"}
	for i := 0; i < 50; i++ {
		if d := l.Check(context.Background(), id); !d.Allowed {
			t.Fatalf("request %d denied with no limits configured", i)
		}
	}
}
