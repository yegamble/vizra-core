package cache_test

import (
	"context"
	"net"
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

// deadCacheAddr returns a loopback address nothing listens on: a listener is
// opened on an ephemeral port and closed again, so a dial is refused at once.
func deadCacheAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// L-2 (security review of the core #8 limiter): a request whose context has
// ENDED is not a cache failure. Before the fix, a client that disconnected
// mid-request flipped the limiter to degraded — so /readyz reported "rate
// limiting is running on the in-process fallback" when the cache was fine —
// and charged the in-process counter for a request that was already dead.
//
// The cache here is unreachable on purpose, so the control at the end can prove
// this harness DOES observe a flip; without that, a limiter that never flipped
// for any reason would pass.
func TestACancelledRequestDoesNotFlipTheLimiterToDegraded(t *testing.T) {
	c, err := cache.Open("redis://"+deadCacheAddr(t)+"/0", "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	l := cache.NewFallbackLimiter(c)
	if l.Degraded() {
		t.Fatal("precondition: a limiter with a configured cache starts degraded")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 3 {
		allowed, remaining := l.Allow(cancelled, "k", 1, time.Minute)
		if l.Degraded() {
			t.Errorf("call %d with a cancelled context flipped the limiter to degraded; "+
				"a request that ended is not a cache failure", i)
		}
		if allowed || remaining != 0 {
			t.Errorf("call %d with a cancelled context returned (%v, %d); a dead request is answered (false, 0)",
				i, allowed, remaining)
		}
	}

	// Control, and the fallback's budget: a LIVE request against the same dead
	// cache must flip to degraded and be decided by the in-process counter. Its
	// limit is 1, so it is admitted only if the three cancelled calls above
	// spent none of that counter.
	allowed, remaining := l.Allow(context.Background(), "k", 1, time.Minute)
	if !l.Degraded() {
		t.Fatal("control: a live request against an unreachable cache did not flip to degraded, " +
			"so this test cannot observe a flip at all")
	}
	if !allowed || remaining != 0 {
		t.Fatalf("the first live request got (%v, %d), want (true, 0): the cancelled calls "+
			"charged the in-process fallback for requests that were already dead", allowed, remaining)
	}
}

// The same, for a context that ends WHILE the cache command is in flight — the
// realistic shape of a client disconnecting mid-request. The "cache" accepts the
// connection and never answers, so the command is blocked on the network when
// the context is cancelled.
func TestARequestCancelledMidCommandDoesNotFlipTheLimiterToDegraded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn // held open, never answered
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		close(accepted)
		for conn := range accepted {
			_ = conn.Close()
		}
	})

	c, err := cache.Open("redis://"+ln.Addr().String()+"/0", "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	l := cache.NewFallbackLimiter(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case conn := <-accepted:
			accepted <- conn // keep it for cleanup
			cancel()
		case <-time.After(10 * time.Second):
			cancel()
		}
	}()

	allowed, remaining := l.Allow(ctx, "k", 1, time.Minute)
	if ctx.Err() == nil {
		t.Fatal("precondition: Allow returned before the context was cancelled, so nothing was in flight")
	}
	if l.Degraded() {
		t.Error("a request cancelled while its cache command was in flight flipped the limiter to degraded")
	}
	if allowed || remaining != 0 {
		t.Errorf("a request cancelled mid-command returned (%v, %d), want (false, 0)", allowed, remaining)
	}
}

// With no cache configured the limiter is the in-process counter alone, and a
// dead request must not spend it either: three cancelled calls, then a live one
// at limit 1 is still admitted.
func TestACancelledRequestDoesNotChargeTheInProcessLimiter(t *testing.T) {
	l := cache.NewFallbackLimiter(nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 3 {
		if allowed, remaining := l.Allow(cancelled, "k", 1, time.Minute); allowed || remaining != 0 {
			t.Errorf("call %d with a cancelled context returned (%v, %d), want (false, 0)", i, allowed, remaining)
		}
	}
	if allowed, remaining := l.Allow(context.Background(), "k", 1, time.Minute); !allowed || remaining != 0 {
		t.Fatalf("the first live request got (%v, %d), want (true, 0): cancelled calls spent the in-process budget",
			allowed, remaining)
	}
	if allowed, _ := l.Allow(context.Background(), "k", 1, time.Minute); allowed {
		t.Fatal("control: the second live request at limit 1 was admitted, so the counter is not counting")
	}
}
