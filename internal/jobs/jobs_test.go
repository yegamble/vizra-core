package jobs_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// ---------------------------------------------------------------------------
// Security Finding 5 — last_error must be redacted BEFORE it is truncated
// ---------------------------------------------------------------------------

// jobs.last_error is queryable, is rendered in /admin/jobs, and travels in any
// pg_dump an operator shares. ADR-002 says secrets do not enter a queryable
// table. A Go HTTP error is a *url.Error that formats as
// `Post "https://user:pw@host/path": ...`, so an M1 handler doing an S3 put, a
// federation delivery or a webhook call produces exactly that text.
//
// The value classes are deliberately the same ones internal/obs/log_test.go
// uses, so the two stay in step.
func TestLastErrorIsRedactedBeforeItIsStored(t *testing.T) {
	cases := []struct {
		name   string
		err    string
		secret string
	}{
		{"DSN password", `Post "postgres://vizra:hunter2@db:5432/vizra": dial tcp: refused`, "hunter2"},
		{"presigned S3 URL", `Get "https://b.example/o?X-Amz-Signature=abc123def456&X-Amz-Expires=900": timeout`, "abc123def456"},
		{"bearer token", "upstream rejected: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9"},
		{"vizra API key", "webhook auth failed for key vzk_LiveKeyMaterial0123456789", "vzk_LiveKeyMaterial0123456789"},
		{"cache URL password", `dial rediss://default:s3cr3tpw@cache:6379/0: refused`, "s3cr3tpw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jobs.SafeErrorForTest(tc.err)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("last_error would have stored %q:\n%s", tc.secret, got)
			}
			if !strings.Contains(got, "redacted") {
				t.Fatalf("nothing was redacted, so the secret may simply be absent:\n%s", got)
			}
		})
	}
}

// The ORDER is the point. Truncating first would cut a credential in half and
// store the first half, which is still a leak; redacting first replaces it
// before the cut is made.
func TestASecretStraddlingTheTruncationBoundaryIsRedacted(t *testing.T) {
	// Place a presigned URL so that it spans the 2000-byte cut.
	const secret = "X-Amz-Signature=SuperSecretSignatureMaterial0123456789"
	prefix := strings.Repeat("a", 1980)
	raw := "failed fetching https://b.example/o?" + prefix + "&" + secret + " after 3 tries"

	got := jobs.SafeErrorForTest(raw)
	if strings.Contains(got, "SuperSecretSignatureMaterial") {
		t.Fatalf("a secret straddling the truncation boundary was stored:\n%s", got)
	}
	// And the whole point of truncating still holds.
	if len(got) > 2100 {
		t.Fatalf("the bound was lost: %d bytes", len(got))
	}
}

// ---------------------------------------------------------------------------
// Backend Finding 4 — the payload bound, in Go
// ---------------------------------------------------------------------------

func TestEnqueueRefusesAnOversizedPayload(t *testing.T) {
	big := map[string]string{"blob": strings.Repeat("x", jobs.MaxPayloadBytes)}
	_, err := jobs.Enqueue(t.Context(), nil, jobs.NewJob{
		Kind: jobs.KindNoop, CorrelationID: "c1", Payload: big,
	})
	if !errors.Is(err, jobs.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge. ClaimJob RETURNs payload on every claim, "+
			"so an unbounded one is pulled over the wire each time the row is looked at.", err)
	}
	// The accept side needs a real transaction, so it lives in the integration
	// suite (TestEnqueueRefusesAnUnboundedPayload), which also proves the bound
	// exists in the DATABASE and not only in this caller.
}

func TestEnqueueRefusesAnOversizedKindOrCorrelationID(t *testing.T) {
	_, err := jobs.Enqueue(t.Context(), nil, jobs.NewJob{
		Kind: jobs.Kind(strings.Repeat("k", jobs.MaxKindBytes+1)), CorrelationID: "c1",
	})
	if !errors.Is(err, jobs.ErrKindTooLong) {
		t.Fatalf("err = %v, want ErrKindTooLong", err)
	}
	_, err = jobs.Enqueue(t.Context(), nil, jobs.NewJob{
		Kind: jobs.KindNoop, CorrelationID: strings.Repeat("c", jobs.MaxCorrelationIDBytes+1),
	})
	if !errors.Is(err, jobs.ErrCorrelationIDTooLong) {
		t.Fatalf("err = %v, want ErrCorrelationIDTooLong", err)
	}
}

// ---------------------------------------------------------------------------
// Backend Finding 9 — priority 0 means "unset", and urgent is reachable
// ---------------------------------------------------------------------------

func TestPriorityOrdering(t *testing.T) {
	if !(jobs.PriorityUrgent < jobs.DefaultPriority && jobs.DefaultPriority < jobs.PriorityBackground) {
		t.Fatalf("priorities are not ordered urgent < default < background: %d %d %d",
			jobs.PriorityUrgent, jobs.DefaultPriority, jobs.PriorityBackground)
	}
	// Zero must not be usable as "most urgent": it is the Go zero value and
	// therefore means unset. PriorityUrgent is how a caller reaches the top.
	if jobs.PriorityUrgent == 0 {
		t.Fatal("PriorityUrgent is 0, which is indistinguishable from unset")
	}
}

// ---------------------------------------------------------------------------
// truncate must not cut a UTF-8 rune in half
// ---------------------------------------------------------------------------

// last_error is a `text` column. PostgreSQL rejects an invalid UTF-8 byte
// sequence outright, so a truncation that slices mid-rune does not merely store
// a mangled string — the whole write FAILS, the row stays `leased`, and the
// real cause of the failure is never recorded. The job is then swept and
// re-run, losing the one piece of evidence an operator needed.
//
// A multibyte error message is not exotic: a filename, a photo title, a remote
// server's error body, or an em dash in our own text all reach 2 KiB.
func TestSafeErrorNeverCutsARuneInHalf(t *testing.T) {
	// Drive the cut across every byte offset a multibyte rune can straddle, so
	// the test does not depend on guessing the exact boundary.
	multibyte := []string{
		"é",    // 2 bytes
		"日",    // 3 bytes
		"🙂",    // 4 bytes
		"—",    // 3 bytes, the em dash our own messages use
		"ñé日🙂", // mixed
	}
	for _, r := range multibyte {
		for pad := 1990; pad <= 2010; pad++ {
			in := strings.Repeat("a", pad) + strings.Repeat(r, 40) + " tail"
			got := jobs.SafeErrorForTest(in)
			if !utf8.ValidString(got) {
				t.Fatalf("pad=%d rune=%q produced invalid UTF-8; PostgreSQL would reject the "+
					"last_error write and the job would stay leased with its cause unrecorded.\n"+
					"last 16 bytes: % x", pad, r, got[max(0, len(got)-16):])
			}
		}
	}
}

// The bound must still hold, and the message must still be useful.
func TestSafeErrorStillBoundsAndStillReads(t *testing.T) {
	in := "connecting: " + strings.Repeat("日", 5000)
	got := jobs.SafeErrorForTest(in)
	if !utf8.ValidString(got) {
		t.Fatal("invalid UTF-8")
	}
	if len(got) > 2100 {
		t.Fatalf("the bound was lost: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "connecting: ") {
		t.Fatalf("the beginning of the message was lost: %q", got[:40])
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("a truncated message must say so: %q", got[len(got)-40:])
	}
}

// Redaction happens BEFORE truncation, and the rune-safety must not reopen the
// straddling hole the previous round closed.
func TestSafeErrorRedactsThenTruncatesEvenWithMultibyteText(t *testing.T) {
	const secret = "X-Amz-Signature=SuperSecretSignatureMaterial0123456789"
	in := "失敗 fetching https://b.example/o?" + strings.Repeat("日", 660) + "&" + secret + " after 3 tries"
	got := jobs.SafeErrorForTest(in)
	if strings.Contains(got, "SuperSecretSignatureMaterial") {
		t.Fatalf("a secret straddling the truncation boundary was stored:\n%s", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("redaction plus truncation produced invalid UTF-8")
	}
}
