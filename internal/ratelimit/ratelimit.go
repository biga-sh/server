// Package ratelimit counts requests in fixed windows, shared across server
// instances through Redis when it is configured.
package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter decides whether a request fits in its budget.
type Limiter interface {
	// Allow counts one request against key and reports whether it is within
	// limit per window, and if not, how long until the window resets.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration)
	// Exceeded reports whether key has already used up limit in its current
	// window, without counting a request, and if so how long until it resets.
	Exceeded(ctx context.Context, key string, limit int) (bool, time.Duration)
}

// Memory is a per-process limiter, used alone on a single instance and as
// the fallback when Redis is unreachable.
type Memory struct {
	mu      sync.Mutex
	windows map[string]*window
	now     func() time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// pruneThreshold is the map size past which expired windows are swept, so
// a long-running process cannot grow without bound.
const pruneThreshold = 10_000

// NewMemory returns an empty in-memory limiter.
func NewMemory() *Memory {
	return &Memory{windows: make(map[string]*window), now: time.Now}
}

// Allow implements [Limiter].
func (m *Memory) Allow(_ context.Context, key string, limit int, d time.Duration) (bool, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if len(m.windows) > pruneThreshold {
		for k, w := range m.windows {
			if !w.resetAt.After(now) {
				delete(m.windows, k)
			}
		}
	}
	w, ok := m.windows[key]
	if !ok || !w.resetAt.After(now) {
		m.windows[key] = &window{count: 1, resetAt: now.Add(d)}
		return 1 <= limit, 0
	}
	w.count++
	if w.count <= limit {
		return true, 0
	}
	return false, w.resetAt.Sub(now)
}

// Exceeded implements [Limiter].
func (m *Memory) Exceeded(_ context.Context, key string, limit int) (bool, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	w, ok := m.windows[key]
	if !ok || !w.resetAt.After(now) || w.count < limit {
		return false, 0
	}
	return true, w.resetAt.Sub(now)
}

// Redis is a limiter shared by every instance using the same Redis.
type Redis struct {
	client   *redis.Client
	fallback *Memory
	log      *slog.Logger
}

// NewRedis returns a Redis-backed limiter that degrades to in-memory
// counting while Redis is unreachable, rather than failing open.
func NewRedis(client *redis.Client, log *slog.Logger) *Redis {
	return &Redis{client: client, fallback: NewMemory(), log: log}
}

// incr counts a hit and sets the window's expiry on its first hit, in one
// round trip and atomically.
var incr = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return {n, redis.call("PTTL", KEYS[1])}
`)

// Allow implements [Limiter].
func (r *Redis) Allow(ctx context.Context, key string, limit int, d time.Duration) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	res, err := incr.Run(ctx, r.client, []string{"rl:" + key}, d.Milliseconds()).Int64Slice()
	if err != nil || len(res) != 2 {
		r.log.Debug("rate limiter falling back to memory", "error", err)
		return r.fallback.Allow(ctx, key, limit, d)
	}
	if res[0] <= int64(limit) {
		return true, 0
	}
	return false, time.Duration(max(res[1], 0)) * time.Millisecond
}

// Exceeded implements [Limiter].
func (r *Redis) Exceeded(ctx context.Context, key string, limit int) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	pipe := r.client.Pipeline()
	count := pipe.Get(ctx, "rl:"+key)
	ttl := pipe.PTTL(ctx, "rl:"+key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return r.fallback.Exceeded(ctx, key, limit)
	}
	n, err := count.Int64()
	if err != nil || n < int64(limit) {
		return false, 0
	}
	return true, max(ttl.Val(), 0)
}
