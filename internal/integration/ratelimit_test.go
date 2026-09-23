//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yegamble/vizra-core/internal/cache"
)

// ---------------------------------------------------------------------------
// The cache-backed limiter is a FIXED window (verifier R4-A, PR #8 round 4)
// ---------------------------------------------------------------------------
//
// It used to run INCR then an unconditional EXPIRE on every call, so every
// request — refused ones included — pushed the key's expiry a full window into
// the future. Measured by the verifier through the real claim handler on real
// Valkey: after a 600-request burst, one request every <=15 minutes held
// claim-owner closed indefinitely, and the operator's own retries extended their
// lockout. AGENTS.md, the code comments and the README all said "fixed window".
//
// These tests run against whatever VIZRA_TEST_CACHE_URL points at; CI runs them
// on both matrix images (Valkey and Redis 7.2), and so does the evidence.

// rlEnv is a limiter over the real cache, in a namespace no other test shares.
type rlEnv struct {
	client *cache.Client
	lim    *cache.FallbackLimiter
}

func newRLEnv(t *testing.T) *rlEnv {
	t.Helper()
	c, err := cache.Open(mustEnv(t, "VIZRA_TEST_CACHE_URL"), "rltest-"+uuid.NewString())
	if err != nil {
		t.Fatalf("opening the cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the cache is not reachable, so this test cannot prove anything: %v", err)
	}
	return &rlEnv{client: c, lim: cache.NewFallbackLimiter(c)}
}

// pttl reads the TTL the SERVER holds for the limiter's key. The key is built
// the same way FallbackLimiter builds it: Key("rl", key).
func (e *rlEnv) pttl(t *testing.T, key string) time.Duration {
	t.Helper()
	d, err := e.client.Redis().PTTL(t.Context(), e.client.Key("rl", key)).Result()
	if err != nil {
		t.Fatalf("reading PTTL: %v", err)
	}
	return d
}

func (e *rlEnv) allow(t *testing.T, key string, limit int, window time.Duration) bool {
	t.Helper()
	ok, _ := e.lim.Allow(t.Context(), key, limit, window)
	if e.lim.Degraded() {
		t.Fatal("the limiter fell back to its in-process counter; this test is about the CACHE path")
	}
	return ok
}

// sleepUntil sleeps until the deadline. It is a clock, not a barrier: what it
// orders is the SERVER's TTL against wall time, which is the thing under test.
func sleepUntil(d time.Time) { time.Sleep(time.Until(d)) }

// TestTheCacheLimiterDoesNotRefreshTheTTLWithinTheWindow: a later request in
// the same window must not push the window's end out.
func TestTheCacheLimiterDoesNotRefreshTheTTLWithinTheWindow(t *testing.T) {
	e := newRLEnv(t)
	const window = 10 * time.Second
	e.allow(t, "k", 100, window)
	first := e.pttl(t, "k")
	if first <= 0 || first > window {
		t.Fatalf("after the first request the key's TTL is %v, want (0, %v]", first, window)
	}
	time.Sleep(1500 * time.Millisecond)
	// Refused and allowed requests alike: neither may move the window's end.
	for range 3 {
		e.allow(t, "k", 100, window)
	}
	second := e.pttl(t, "k")
	if second > first-time.Second {
		t.Fatalf("1.5s and three requests later the TTL is %v (it was %v): a request REFRESHED the "+
			"window, so a caller who keeps sending can hold a limited key closed for ever", second, first)
	}
}

// TestTheCacheLimiterReopensWhenTheWindowRolls: once a key is limited, a
// request sent after the window that started with the FIRST request has ended
// is allowed — however many refused requests arrived in between.
func TestTheCacheLimiterReopensWhenTheWindowRolls(t *testing.T) {
	e := newRLEnv(t)
	const window = 3 * time.Second
	if !e.allow(t, "k", 1, window) {
		t.Fatal("the first request of an empty window was refused")
	}
	// The server started the key's TTL no later than this instant.
	opened := time.Now()

	sleepUntil(opened.Add(time.Second))
	if e.allow(t, "k", 1, window) {
		t.Fatal("the second request inside a limit-1 window was allowed")
	}
	if late := time.Since(opened); late > window-500*time.Millisecond {
		t.Fatalf("the in-window request ran %v after the window opened, too close to its end for "+
			"this test to distinguish a fixed window from a refreshed one; the host is too slow", late)
	}

	sleepUntil(opened.Add(window + 300*time.Millisecond))
	if !e.allow(t, "k", 1, window) {
		t.Fatalf("a request %v after the window opened (window %v) was refused: the refused request "+
			"inside the window extended it", time.Since(opened).Round(time.Millisecond), window)
	}
}

// TestTheCacheLimiterGivesATTLToAKeyFoundWithoutOne: a counter key with NO
// expiry is a permanent lockout. Whatever left it that way — an interrupted
// older writer, an operator's SET — the next call must give it one.
func TestTheCacheLimiterGivesATTLToAKeyFoundWithoutOne(t *testing.T) {
	e := newRLEnv(t)
	const window = 10 * time.Second
	if err := e.client.Redis().Set(t.Context(), e.client.Key("rl", "k"), 5, 0).Err(); err != nil {
		t.Fatalf("planting a TTL-less counter: %v", err)
	}
	if d := e.pttl(t, "k"); d != -1 {
		t.Fatalf("the planted key's PTTL is %v, want -1 (no expiry)", d)
	}
	e.allow(t, "k", 100, window)
	if d := e.pttl(t, "k"); d <= 0 || d > window {
		t.Fatalf("after a call the TTL-less key's PTTL is %v, want (0, %v]: a counter with no expiry "+
			"never resets, which is a permanent lockout", d, window)
	}
}

// TestTheCacheAndFallbackLimitersAgreeOnFixedWindowSemantics: one script of
// calls against the three limiters an api process can be running — the cache
// path, the cache-DOWN path of the same type, and the in-process limiter — must
// produce the same decisions. Switching between them may change the blast radius
// (per process vs shared), never the semantics.
//
// Each limiter runs the script on its OWN timeline, in its own goroutine,
// measured from the moment its first call RETURNED (the window opened no later
// than that). That matters because the cache-down path is slow by design: every
// call first fails a pipeline against an unreachable server (measured ~1.3 s
// with the client's retries) before the in-process counter decides, so a shared
// clock would compare decisions taken at different moments.
func TestTheCacheAndFallbackLimitersAgreeOnFixedWindowSemantics(t *testing.T) {
	e := newRLEnv(t)
	down, err := cache.Open("redis://127.0.0.1:1/0", "rltest-down")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = down.Close() })

	limiters := []struct {
		name      string
		l         cache.Limiter
		wantCache bool // Degraded() must be false exactly when this is true
	}{
		{"cache up", e.lim, true},
		{"cache down (FallbackLimiter over an unreachable server)", cache.NewFallbackLimiter(down), false},
		{"in-process MemoryLimiter", cache.NewMemoryLimiter(), false},
	}

	const window = 6 * time.Second
	const limit = 2
	// at: when the call STARTS, as an offset from the first call's return.
	// want: the fixed-window decision.
	script := []struct {
		at   time.Duration
		want bool
	}{
		{0, true},
		{0, true},
		{time.Second, false},                   // over the limit, inside the window
		{2 * time.Second, false},               // still inside; must NOT extend it
		{window + 300*time.Millisecond, true},  // a new window
		{window + 400*time.Millisecond, true},  // second of the new window
		{window + 500*time.Millisecond, false}, // over again
	}
	const lastInWindow = 3

	type result struct {
		got  []bool
		fail string
	}
	results := make([]result, len(limiters))
	var wg sync.WaitGroup
	for i, lim := range limiters {
		wg.Add(1)
		go func(i int, name string, l cache.Limiter, wantCache bool) {
			defer wg.Done()
			key := fmt.Sprintf("agree-%d", i)
			var opened time.Time
			for n, step := range script {
				if n > 0 {
					sleepUntil(opened.Add(step.at))
				}
				got, _ := l.Allow(context.Background(), key, limit, window)
				if n == 0 {
					opened = time.Now()
				}
				if wantCache == l.Degraded() {
					results[i].fail = fmt.Sprintf("[%s] Degraded()=%v: not on the path this row claims to test",
						name, l.Degraded())
					return
				}
				if n == lastInWindow && time.Since(opened) > window-300*time.Millisecond {
					results[i].fail = fmt.Sprintf("[%s] the in-window steps ended %v after the window opened, too "+
						"late to tell a fixed window from a refreshed one; the host is too slow", name, time.Since(opened))
					return
				}
				results[i].got = append(results[i].got, got)
			}
		}(i, lim.name, lim.l, lim.wantCache)
	}
	wg.Wait()

	for i, lim := range limiters {
		if results[i].fail != "" {
			t.Fatal(results[i].fail)
		}
		for n, step := range script {
			if results[i].got[n] != step.want {
				t.Errorf("step %d (+%v): [%s] allowed=%v, want %v (fixed window: %d per %v from the "+
					"FIRST request of the window)", n, step.at, lim.name, results[i].got[n], step.want, limit, window)
			}
		}
	}
}
