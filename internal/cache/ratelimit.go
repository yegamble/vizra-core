package cache

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter is a fixed-window counter. ADR-003 puts rate-limit counters in the
// cache; this file adds the fallback that ADR requires: "if Valkey is
// unreachable the api falls back to a per-process in-memory limiter and marks
// readiness degraded".
//
// The fallback is deliberately weaker and says so. Per-process counters mean N
// replicas allow N times the limit. That is a real reduction in protection, and
// the correct response is a degraded readiness signal an operator can see — not
// a silent switch that looks identical to the healthy path.
type Limiter interface {
	// Allow reports whether one unit of work may proceed for key within the
	// window, and how many remain.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, remaining int)
	// Degraded reports whether the limiter is currently running on its
	// in-process fallback.
	Degraded() bool
}

// FallbackLimiter uses the cache when it can and an in-process counter when it
// cannot, flipping back automatically when the cache returns.
type FallbackLimiter struct {
	c        *Client
	fallback *MemoryLimiter

	mu       sync.RWMutex
	degraded bool
}

// NewFallbackLimiter builds the limiter. c may be nil, which means "always use
// the in-process limiter" and is permanently degraded.
func NewFallbackLimiter(c *Client) *FallbackLimiter {
	l := &FallbackLimiter{c: c, fallback: NewMemoryLimiter()}
	if c == nil {
		l.degraded = true
	}
	return l
}

func (l *FallbackLimiter) setDegraded(v bool) {
	l.mu.Lock()
	l.degraded = v
	l.mu.Unlock()
}

// Degraded reports whether the most recent decision came from the fallback.
func (l *FallbackLimiter) Degraded() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.degraded
}

// Allow fails OPEN on a cache error, after switching to the in-process counter.
// Failing closed would turn a cache outage into a total outage; the in-process
// counter still bounds a single replica, and readiness reports the weakening.
//
// It is a FIXED window: the TTL is set when the window STARTS and never moved.
// The previous version ran an unconditional EXPIRE on every call, so every
// request — a refused one included — pushed the window's end out by a full
// window. That made it a sliding lockout: after one burst, a single request per
// window held a key closed for ever, and the in-process fallback (a true fixed
// window) behaved differently from the cache path it stands in for.
//
// `EXPIRE key window NX` sets the expiry ONLY when the key has none, which
// covers both the first request of a window (INCR just created the key) and a
// counter found WITHOUT a TTL for any reason — a key with no expiry never resets,
// which is a permanent lockout. INCR and EXPIRE NX go in one MULTI/EXEC, so the
// server applies both or neither: no crash between them can leave a TTL-less
// key behind. NX needs Redis >= 7.0; ADR-001 sets the supported floor at
// Redis/Valkey >= 7.2 and CI runs Valkey 9.1.2 and Redis 7.2.
func (l *FallbackLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, int) {
	if l.c == nil {
		return l.fallback.Allow(ctx, key, limit, window)
	}
	k := l.c.Key("rl", key)
	pipe := l.c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.ExpireNX(ctx, k, window)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		l.setDegraded(true)
		return l.fallback.Allow(ctx, key, limit, window)
	}
	l.setDegraded(false)
	n := int(incr.Val())
	if n > limit {
		return false, 0
	}
	return true, limit - n
}

// MemoryLimiter is the per-process fallback. It is a fixed window, like the
// cache implementation, so switching between them does not change the
// semantics an operator reasons about — only the blast radius.
type MemoryLimiter struct {
	mu      sync.Mutex
	windows map[string]*memWindow
}

type memWindow struct {
	count   int
	expires time.Time
}

// NewMemoryLimiter builds the fallback.
func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{windows: map[string]*memWindow{}}
}

func (m *MemoryLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (bool, int) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	// Bounded memory: drop expired windows on every call rather than growing a
	// map an attacker chooses the keys of.
	if len(m.windows) > 4096 {
		for k, w := range m.windows {
			if now.After(w.expires) {
				delete(m.windows, k)
			}
		}
	}

	w, ok := m.windows[key]
	if !ok || now.After(w.expires) {
		w = &memWindow{expires: now.Add(window)}
		m.windows[key] = w
	}
	w.count++
	if w.count > limit {
		return false, 0
	}
	return true, limit - w.count
}

// Degraded is always true: an in-process limiter is by definition the degraded
// mode.
func (m *MemoryLimiter) Degraded() bool { return true }

var (
	_ Limiter = (*FallbackLimiter)(nil)
	_ Limiter = (*MemoryLimiter)(nil)
)
