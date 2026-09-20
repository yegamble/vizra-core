package jobs_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/jobs"
)

// TestEnqueueRequiresATransaction is the compile-time proof ADR-004 demands:
// "every Enqueue takes a pgx.Tx, and because the API accepts only a transaction
// type, an enqueue outside a transaction does not compile."
//
// A comment asserting that is not evidence. This test builds two real programs:
// a positive control that passes a pgx.Tx and must compile, and the negative
// case that passes a *pgxpool.Pool and must not. The positive control exists so
// a broken harness cannot make the negative case pass for the wrong reason.
func TestEnqueueRequiresATransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles two programs; skipped under -short")
	}

	t.Run("a transaction compiles", func(t *testing.T) {
		out, err := exec.Command("go", "build", "-o", devNull(t), "./testdata/enqueue/with_tx.go").CombinedOutput()
		if err != nil {
			t.Fatalf("the positive control failed to build, so this test proves nothing:\n%s", out)
		}
	})

	t.Run("a pool does not compile", func(t *testing.T) {
		out, err := exec.Command("go", "build", "-o", devNull(t), "./testdata/enqueue/without_tx.go").CombinedOutput()
		if err == nil {
			t.Fatal("Enqueue accepted a *pgxpool.Pool. ADR-004's transaction-only rule is no longer enforced by the compiler, " +
				"so a committed mutation can now lose its side effect.")
		}
		if !strings.Contains(string(out), "does not implement pgx.Tx") {
			t.Fatalf("the build failed for a different reason than the type rule:\n%s", out)
		}
	})
}

func devNull(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/out"
}

func TestBackoffLadderIsCapped(t *testing.T) {
	// Attempt 1 is the shortest wait; the ladder rises and then holds, so a
	// poisoned job does not push its own retry into the far future while still
	// occupying a slot.
	first := jobs.Backoff(1)
	if first != 30*time.Second {
		t.Fatalf("Backoff(1) = %v, want 30s", first)
	}
	last := jobs.Backoff(int32(len(jobs.RetryLadder)))
	for _, a := range []int32{50, 500, 5000} {
		if got := jobs.Backoff(a); got != last {
			t.Fatalf("Backoff(%d) = %v, want the capped %v", a, got, last)
		}
	}
	// Monotonic non-decreasing.
	prev := time.Duration(0)
	for i := 1; i <= len(jobs.RetryLadder); i++ {
		got := jobs.Backoff(int32(i))
		if got < prev {
			t.Fatalf("the ladder decreases at attempt %d: %v after %v", i, got, prev)
		}
		prev = got
	}
	// A zero or negative attempt must not index out of range or return zero,
	// which would be a hot retry loop.
	if jobs.Backoff(0) <= 0 || jobs.Backoff(-3) <= 0 {
		t.Fatal("Backoff returned a non-positive delay for a non-positive attempt")
	}
}

func TestTerminalBypassesTheLadder(t *testing.T) {
	plain := errTest("nope")
	if jobs.IsTerminal(plain) {
		t.Fatal("a plain error was treated as terminal")
	}
	wrapped := jobs.Terminal{Err: plain}
	if !jobs.IsTerminal(wrapped) {
		t.Fatal("a Terminal error was not recognised")
	}
	if !strings.Contains(wrapped.Error(), "nope") {
		t.Fatalf("Terminal.Error() lost the cause: %q", wrapped.Error())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestEnqueueRefusesAnUntraceableJob(t *testing.T) {
	// A missing correlation id is refused rather than generated: a generated one
	// is untraceable and looks fine.
	_, err := jobs.Enqueue(t.Context(), nil, jobs.NewJob{Kind: jobs.KindNoop})
	if err == nil {
		t.Fatal("a job with no correlation id was accepted")
	}
	_, err = jobs.Enqueue(t.Context(), nil, jobs.NewJob{CorrelationID: "c1"})
	if err == nil {
		t.Fatal("a job with no kind was accepted")
	}
}
