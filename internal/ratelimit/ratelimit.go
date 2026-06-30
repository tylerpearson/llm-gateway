// Package ratelimit enforces per key and per team budgets and rate limits using
// atomic Redis counters: requests per minute, tokens per minute, and a monthly
// dollar budget. The default mode is soft (allow the request and flag the
// breach via a response header) rather than hard 429s, following the project's
// preference for better defaults over usage caps.
//
// Enforcement bound: The requests-per-minute counter uses atomic Redis INCR
// and is exact. The tokens-per-minute and monthly-USD limits are checked by
// reading counter values that previous requests recorded. Since token and
// dollar usage are only known after a response completes, they are added via
// RecordUsage after the response. This means tokens-per-minute and
// monthly-USD checks lag by roughly one request. Under high concurrency, a
// hard monthly-USD cap can be overshot by requests in flight when checked.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Mode selects soft or hard enforcement.
type Mode string

// Enforcement modes.
const (
	// ModeSoft allows the request and reports breaches via a response header.
	ModeSoft Mode = "soft"
	// ModeHard rejects breaching requests with 429.
	ModeHard Mode = "hard"
)

// Limits is one identity's thresholds. A zero value means unlimited.
type Limits struct {
	RequestsPerMin int64
	TokensPerMin   int64
	MonthlyUSD     float64
}

// Settings holds the mode and the per key and per team limits.
type Settings struct {
	Mode    Mode
	PerKey  Limits
	PerTeam Limits
}

// Identity is the authenticated principal a request is attributed to.
type Identity struct {
	KeyID  string
	TeamID string
}

// Decision is the outcome of a limit check.
type Decision struct {
	Allowed  bool
	Exceeded []string
}

// Limiter tracks usage counters in Redis and enforces Settings.
type Limiter struct {
	rdb redis.Cmdable
	s   Settings
	log *slog.Logger
	now func() time.Time
}

// New connects to Redis and returns a Limiter.
func New(addr string, s Settings, log *slog.Logger) (*Limiter, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return NewWithClient(rdb, s, log), nil
}

// NewWithClient builds a Limiter over an existing client (used in tests).
func NewWithClient(rdb redis.Cmdable, s Settings, log *slog.Logger) *Limiter {
	if s.Mode == "" {
		s.Mode = ModeSoft
	}
	return &Limiter{rdb: rdb, s: s, log: log, now: time.Now}
}

// Close releases the client if it owns one.
func (l *Limiter) Close() error {
	if c, ok := l.rdb.(*redis.Client); ok {
		return c.Close()
	}
	return nil
}

const (
	minuteWindow = 70 * time.Second
	monthWindow  = 35 * 24 * time.Hour
)

// Check increments the per minute request counters and compares all counters to
// the limits. In hard mode any breach denies the request; in soft mode the
// request is always allowed but breaches are reported for the response header.
func (l *Limiter) Check(ctx context.Context, id Identity) Decision {
	now := l.now()
	minute := now.Format("200601021504")
	month := now.Format("200601")

	var exceeded []string
	exceeded = append(exceeded, l.checkScope(ctx, "key", id.KeyID, l.s.PerKey, minute, month)...)
	exceeded = append(exceeded, l.checkScope(ctx, "team", id.TeamID, l.s.PerTeam, minute, month)...)

	allowed := l.s.Mode != ModeHard || len(exceeded) == 0
	return Decision{Allowed: allowed, Exceeded: exceeded}
}

func (l *Limiter) checkScope(ctx context.Context, scope, id string, lim Limits, minute, month string) []string {
	if id == "" {
		return nil
	}
	var exceeded []string

	if lim.RequestsPerMin > 0 {
		key := fmt.Sprintf("rl:req:%s:%s:%s", scope, id, minute)
		n, err := l.rdb.Incr(ctx, key).Result()
		if err == nil {
			_ = l.rdb.Expire(ctx, key, minuteWindow).Err()
			if n > lim.RequestsPerMin {
				exceeded = append(exceeded, scope+":requests_per_min")
			}
		} else {
			l.log.Warn("ratelimit incr failed", slog.Any("error", err))
		}
	}
	if lim.TokensPerMin > 0 {
		key := fmt.Sprintf("rl:tok:%s:%s:%s", scope, id, minute)
		if v, err := l.rdb.Get(ctx, key).Int64(); err == nil && v >= lim.TokensPerMin {
			exceeded = append(exceeded, scope+":tokens_per_min")
		}
	}
	if lim.MonthlyUSD > 0 {
		key := fmt.Sprintf("rl:usd:%s:%s:%s", scope, id, month)
		if v, err := l.rdb.Get(ctx, key).Float64(); err == nil && v >= lim.MonthlyUSD {
			exceeded = append(exceeded, scope+":monthly_usd")
		}
	}
	return exceeded
}

// RecordUsage adds the request's tokens and cost to the per minute and monthly
// counters so later requests see updated usage. It is called after a response.
func (l *Limiter) RecordUsage(ctx context.Context, id Identity, tokens int, costUSD float64) {
	now := l.now()
	minute := now.Format("200601021504")
	month := now.Format("200601")
	l.recordScope(ctx, "key", id.KeyID, l.s.PerKey, tokens, costUSD, minute, month)
	l.recordScope(ctx, "team", id.TeamID, l.s.PerTeam, tokens, costUSD, minute, month)
}

func (l *Limiter) recordScope(ctx context.Context, scope, id string, lim Limits, tokens int, costUSD float64, minute, month string) {
	if id == "" {
		return
	}
	if lim.TokensPerMin > 0 && tokens > 0 {
		key := fmt.Sprintf("rl:tok:%s:%s:%s", scope, id, minute)
		if err := l.rdb.IncrBy(ctx, key, int64(tokens)).Err(); err == nil {
			_ = l.rdb.Expire(ctx, key, minuteWindow).Err()
		}
	}
	if lim.MonthlyUSD > 0 && costUSD > 0 {
		key := fmt.Sprintf("rl:usd:%s:%s:%s", scope, id, month)
		if err := l.rdb.IncrByFloat(ctx, key, costUSD).Err(); err == nil {
			_ = l.rdb.Expire(ctx, key, monthWindow).Err()
		}
	}
}
