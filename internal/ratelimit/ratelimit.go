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
	"errors"
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

	// Batch all commands for both scopes in a single pipeline.
	// Capture command objects to evaluate results after the pipeline executes.
	var keyReqCmd, teamReqCmd *redis.IntCmd
	var keyTokCmd, teamTokCmd, keyUSDCmd, teamUSDCmd *redis.StringCmd

	_, err := l.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		// Queue key scope commands in order: requests, tokens, USD.
		if id.KeyID != "" {
			if l.s.PerKey.RequestsPerMin > 0 {
				keyReqKey := fmt.Sprintf("rl:req:key:%s:%s", id.KeyID, minute)
				keyReqCmd = pipe.Incr(ctx, keyReqKey)
				pipe.Expire(ctx, keyReqKey, minuteWindow)
			}
			if l.s.PerKey.TokensPerMin > 0 {
				keyTokKey := fmt.Sprintf("rl:tok:key:%s:%s", id.KeyID, minute)
				keyTokCmd = pipe.Get(ctx, keyTokKey)
			}
			if l.s.PerKey.MonthlyUSD > 0 {
				keyUSDKey := fmt.Sprintf("rl:usd:key:%s:%s", id.KeyID, month)
				keyUSDCmd = pipe.Get(ctx, keyUSDKey)
			}
		}

		// Queue team scope commands in order: requests, tokens, USD.
		if id.TeamID != "" {
			if l.s.PerTeam.RequestsPerMin > 0 {
				teamReqKey := fmt.Sprintf("rl:req:team:%s:%s", id.TeamID, minute)
				teamReqCmd = pipe.Incr(ctx, teamReqKey)
				pipe.Expire(ctx, teamReqKey, minuteWindow)
			}
			if l.s.PerTeam.TokensPerMin > 0 {
				teamTokKey := fmt.Sprintf("rl:tok:team:%s:%s", id.TeamID, minute)
				teamTokCmd = pipe.Get(ctx, teamTokKey)
			}
			if l.s.PerTeam.MonthlyUSD > 0 {
				teamUSDKey := fmt.Sprintf("rl:usd:team:%s:%s", id.TeamID, month)
				teamUSDCmd = pipe.Get(ctx, teamUSDKey)
			}
		}
		return nil
	})

	// Pipelined returns the first command error, and a GET on a counter key
	// that has not been written yet fails with redis.Nil. That is the normal
	// first-request-of-a-window case, not a transport failure, so it is not
	// worth a warning; the evaluation below treats it as zero usage.
	if err != nil && !errors.Is(err, redis.Nil) {
		l.log.Warn("ratelimit check pipeline failed", slog.Any("error", err))
	}

	// Evaluate results in order: key scope before team scope, requests before tokens before USD.
	if id.KeyID != "" {
		if l.s.PerKey.RequestsPerMin > 0 && keyReqCmd != nil {
			if n, err := keyReqCmd.Result(); err == nil && n > l.s.PerKey.RequestsPerMin {
				exceeded = append(exceeded, "key:requests_per_min")
			}
		}
		if l.s.PerKey.TokensPerMin > 0 && keyTokCmd != nil {
			if v, err := keyTokCmd.Int64(); err == nil && v >= l.s.PerKey.TokensPerMin {
				exceeded = append(exceeded, "key:tokens_per_min")
			}
		}
		if l.s.PerKey.MonthlyUSD > 0 && keyUSDCmd != nil {
			if v, err := keyUSDCmd.Float64(); err == nil && v >= l.s.PerKey.MonthlyUSD {
				exceeded = append(exceeded, "key:monthly_usd")
			}
		}
	}

	if id.TeamID != "" {
		if l.s.PerTeam.RequestsPerMin > 0 && teamReqCmd != nil {
			if n, err := teamReqCmd.Result(); err == nil && n > l.s.PerTeam.RequestsPerMin {
				exceeded = append(exceeded, "team:requests_per_min")
			}
		}
		if l.s.PerTeam.TokensPerMin > 0 && teamTokCmd != nil {
			if v, err := teamTokCmd.Int64(); err == nil && v >= l.s.PerTeam.TokensPerMin {
				exceeded = append(exceeded, "team:tokens_per_min")
			}
		}
		if l.s.PerTeam.MonthlyUSD > 0 && teamUSDCmd != nil {
			if v, err := teamUSDCmd.Float64(); err == nil && v >= l.s.PerTeam.MonthlyUSD {
				exceeded = append(exceeded, "team:monthly_usd")
			}
		}
	}

	allowed := l.s.Mode != ModeHard || len(exceeded) == 0
	return Decision{Allowed: allowed, Exceeded: exceeded}
}

// RecordUsage adds the request's tokens and cost to the per minute and monthly
// counters so later requests see updated usage. It is called after a response.
func (l *Limiter) RecordUsage(ctx context.Context, id Identity, tokens int, costUSD float64) {
	now := l.now()
	minute := now.Format("200601021504")
	month := now.Format("200601")

	// Batch all commands for both scopes in a single pipeline.
	_, err := l.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		// Queue key scope commands.
		if id.KeyID != "" {
			if l.s.PerKey.TokensPerMin > 0 && tokens > 0 {
				keyTokKey := fmt.Sprintf("rl:tok:key:%s:%s", id.KeyID, minute)
				pipe.IncrBy(ctx, keyTokKey, int64(tokens))
				pipe.Expire(ctx, keyTokKey, minuteWindow)
			}
			if l.s.PerKey.MonthlyUSD > 0 && costUSD > 0 {
				keyUSDKey := fmt.Sprintf("rl:usd:key:%s:%s", id.KeyID, month)
				pipe.IncrByFloat(ctx, keyUSDKey, costUSD)
				pipe.Expire(ctx, keyUSDKey, monthWindow)
			}
		}

		// Queue team scope commands.
		if id.TeamID != "" {
			if l.s.PerTeam.TokensPerMin > 0 && tokens > 0 {
				teamTokKey := fmt.Sprintf("rl:tok:team:%s:%s", id.TeamID, minute)
				pipe.IncrBy(ctx, teamTokKey, int64(tokens))
				pipe.Expire(ctx, teamTokKey, minuteWindow)
			}
			if l.s.PerTeam.MonthlyUSD > 0 && costUSD > 0 {
				teamUSDKey := fmt.Sprintf("rl:usd:team:%s:%s", id.TeamID, month)
				pipe.IncrByFloat(ctx, teamUSDKey, costUSD)
				pipe.Expire(ctx, teamUSDKey, monthWindow)
			}
		}
		return nil
	})

	if err != nil {
		// Degrade open: log and continue on Redis errors.
		// Missing counters are treated as zero on the next Check call.
		l.log.Warn("ratelimit record usage pipeline failed", slog.Any("error", err))
	}
}
