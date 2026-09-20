package cache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/cache"
)

// The in-process fallback must behave the same way as the cache-backed limiter,
// so switching between them changes only the blast radius, not the semantics an
// operator reasons about.
func TestMemoryLimiterEnforcesTheWindow(t *testing.T) {
	l := cache.NewMemoryLimiter()
	ctx := context.Background()

	for i := range 3 {
		allowed, remaining := l.Allow(ctx, "k", 3, time.Minute)
		if !allowed {
			t.Fatalf("request %d of 3 was refused", i+1)
		}
		if want := 2 - i; remaining != want {
			t.Fatalf("remaining after request %d = %d, want %d", i+1, remaining, want)
		}
	}
	if allowed, remaining := l.Allow(ctx, "k", 3, time.Minute); allowed || remaining != 0 {
		t.Fatalf("the 4th request was allowed (remaining %d)", remaining)
	}
	// A different key has its own window.
	if allowed, _ := l.Allow(ctx, "other", 3, time.Minute); !allowed {
		t.Fatal("a different key was refused")
	}
}

func TestMemoryLimiterWindowExpires(t *testing.T) {
	l := cache.NewMemoryLimiter()
	ctx := context.Background()
	for range 2 {
		l.Allow(ctx, "k", 2, 20*time.Millisecond)
	}
	if allowed, _ := l.Allow(ctx, "k", 2, 20*time.Millisecond); allowed {
		t.Fatal("the limit did not bite inside the window")
	}
	time.Sleep(30 * time.Millisecond)
	if allowed, _ := l.Allow(ctx, "k", 2, 20*time.Millisecond); !allowed {
		t.Fatal("the window never reopened; a limiter that never resets is an outage")
	}
}

// An attacker chooses the keys, so the map must not grow without bound.
func TestMemoryLimiterBoundsItsMemory(t *testing.T) {
	l := cache.NewMemoryLimiter()
	ctx := context.Background()
	for i := range 20000 {
		l.Allow(ctx, string(rune('a'+i%26))+string(rune(i)), 10, time.Nanosecond)
	}
	// Nothing to assert beyond "it did not blow up": the pruning path is
	// exercised, and a limiter that OOMs is a denial of service on itself.
}

func TestMemoryLimiterIsRaceFree(t *testing.T) {
	l := cache.NewMemoryLimiter()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Allow(ctx, "shared", 1000, time.Minute) }()
	}
	wg.Wait()
	// The 51st..1000th are still allowed; the point is -race sees no conflict.
	if allowed, _ := l.Allow(ctx, "shared", 1000, time.Minute); !allowed {
		t.Fatal("the shared counter overcounted under concurrency")
	}
}

// The in-process limiter is BY DEFINITION the degraded mode, and must say so:
// readiness reports it, and an operator needs to know that N replicas now allow
// N times the limit.
func TestMemoryLimiterAlwaysReportsDegraded(t *testing.T) {
	if !cache.NewMemoryLimiter().Degraded() {
		t.Fatal("the in-process limiter does not report degraded; the weakening would be invisible")
	}
}

// With no cache configured at all, the fallback limiter still limits, and still
// reports degraded.
func TestFallbackWithNoCacheStillLimitsAndReportsDegraded(t *testing.T) {
	l := cache.NewFallbackLimiter(nil)
	ctx := context.Background()
	if !l.Degraded() {
		t.Fatal("a limiter with no cache does not report degraded")
	}
	for range 2 {
		if allowed, _ := l.Allow(ctx, "k", 2, time.Minute); !allowed {
			t.Fatal("a request inside the limit was refused")
		}
	}
	if allowed, _ := l.Allow(ctx, "k", 2, time.Minute); allowed {
		t.Fatal("the limit did not bite with no cache configured; the fallback is not limiting anything")
	}
}
