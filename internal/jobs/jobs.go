// Package jobs is the one durable work mechanism (ADR-004). Every external
// effect — search events, federation delivery, CDN purge, IPFS pin, webhooks,
// email — is a Kind in one table, so there is one depth gauge, one retention
// policy and one dead-letter view.
//
// The invariant this package exists to make unbreakable: a job row is written
// in the SAME transaction as the business mutation. Enqueue takes a pgx.Tx and
// nothing else, so an enqueue outside a transaction DOES NOT COMPILE. That is
// checked by TestEnqueueOutsideATransactionDoesNotCompile, which builds a
// program that tries it and asserts the build fails — because a rule that is
// only written down is a rule that gets forgotten under deadline.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// Kind names a handler. M0 ships exactly one, `noop`, so the loop is exercised
// end to end before any real effect depends on it.
type Kind string

// KindNoop does nothing and succeeds. It exists so the claim/lease/heartbeat/
// sweep path is testable without a media pipeline.
const KindNoop Kind = "noop"

// DefaultMaxAttempts is the ladder length for a retryable failure.
const DefaultMaxAttempts = 5

// Priority values. LOWER RUNS FIRST.
//
// 0 is the Go zero value and therefore means "unset", not "most urgent" — a
// caller who writes Priority: 0 intending "jump the queue" would otherwise get
// the default silently, and 0 looks like the most urgent value in a
// lower-runs-first scheme. PriorityUrgent exists so the top of the queue is
// reachable without relying on that.
const (
	// PriorityUrgent is the highest priority a caller may set. Reserved for
	// work a person is waiting on — M1's upload-finalize path is the first.
	PriorityUrgent int16 = 1
	// DefaultPriority is applied when Priority is left unset (0).
	DefaultPriority int16 = 100
	// PriorityBackground is for sweeps, reconciles and retention.
	PriorityBackground int16 = 200
)

// NewJob is what a caller enqueues.
type NewJob struct {
	Kind Kind
	// Payload is marshalled to jsonb. It must never contain a credential, a
	// signed URL or raw private metadata: job rows are readable by anyone with
	// database access and are shown in /admin/jobs.
	Payload any
	// IdempotencyKey, when set, makes a second enqueue while one is still
	// queued or leased a no-op that returns the in-flight job. Derive it from
	// the mutation, not from time.
	IdempotencyKey string
	// Priority: LOWER RUNS FIRST. Zero means UNSET and resolves to
	// DefaultPriority; use PriorityUrgent for the top of the queue.
	Priority    int16
	MaxAttempts int32
	// RunAfter delays the first attempt. Zero means now.
	RunAfter time.Time
	// CorrelationID ties the job to the request that caused it. Required: a job
	// nobody can trace back to a cause is a job nobody can debug.
	CorrelationID string
}

// Enqueued describes the row that exists after Enqueue.
type Enqueued struct {
	ID       uuid.UUID
	Kind     Kind
	State    string
	RunAfter time.Time
	// AlreadyInFlight is true when an identical live job existed and this call
	// returned it instead of creating a second one.
	AlreadyInFlight bool
}

// ErrNoCorrelationID is returned rather than defaulting, because a generated
// correlation id would be untraceable and look fine.
var ErrNoCorrelationID = errors.New("jobs: CorrelationID is required")

// MaxPayloadBytes mirrors the jobs_payload_bounded CHECK in migration 0002.
// The database is the real control; this exists so a caller gets a named Go
// error naming the size, rather than a constraint violation from three layers
// down.
const MaxPayloadBytes = 65536

// MaxKindBytes and MaxCorrelationIDBytes mirror their CHECK constraints.
const (
	MaxKindBytes          = 64
	MaxCorrelationIDBytes = 128
)

// ErrPayloadTooLarge is returned when a payload exceeds MaxPayloadBytes. A job
// needing more should reference a row rather than carry it: ClaimJob RETURNs
// payload on every claim, so a fat row is pulled over the wire and through
// TOAST each time it is looked at, and it inflates every backup.
var ErrPayloadTooLarge = errors.New("jobs: payload exceeds the 64 KiB limit; reference a row instead of carrying it")

// ErrKindTooLong and ErrCorrelationIDTooLong mirror the remaining bounds.
var (
	ErrKindTooLong          = errors.New("jobs: kind exceeds 64 characters")
	ErrCorrelationIDTooLong = errors.New("jobs: correlation id exceeds 128 characters")
)

// Enqueue writes the job row.
//
// The first argument after ctx is a pgx.Tx, NOT a pool and NOT an interface a
// pool satisfies. *pgxpool.Pool does not implement pgx.Tx, so a caller who has
// only a pool cannot call this, and the ADR-004 rule — "a committed mutation
// can never lose its side effect" — is enforced by the compiler rather than by
// review.
func Enqueue(ctx context.Context, tx pgx.Tx, j NewJob) (Enqueued, error) {
	if j.Kind == "" {
		return Enqueued{}, errors.New("jobs: Kind is required")
	}
	if j.CorrelationID == "" {
		return Enqueued{}, ErrNoCorrelationID
	}
	payload := j.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if len(j.Kind) > MaxKindBytes {
		return Enqueued{}, fmt.Errorf("%w (kind is %d characters)", ErrKindTooLong, len(j.Kind))
	}
	if len(j.CorrelationID) > MaxCorrelationIDBytes {
		return Enqueued{}, fmt.Errorf("%w (correlation id is %d characters)", ErrCorrelationIDTooLong, len(j.CorrelationID))
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Enqueued{}, fmt.Errorf("jobs: encoding payload for kind %s: %w", j.Kind, err)
	}
	// Checked against the MARSHALLED bytes, which is what the column stores and
	// what jobs_payload_bounded measures.
	if len(raw) > MaxPayloadBytes {
		return Enqueued{}, fmt.Errorf("%w (payload is %d bytes, limit %d)", ErrPayloadTooLarge, len(raw), MaxPayloadBytes)
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = DefaultMaxAttempts
	}
	if j.Priority == 0 {
		j.Priority = DefaultPriority
	}
	runAfter := j.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Now()
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Enqueued{}, err
	}

	q := sqlcgen.New(tx)
	var idem *string
	if j.IdempotencyKey != "" {
		idem = &j.IdempotencyKey
	}

	row, err := q.EnqueueJob(ctx, sqlcgen.EnqueueJobParams{
		ID:             id,
		Kind:           string(j.Kind),
		Payload:        raw,
		IdempotencyKey: idem,
		Priority:       j.Priority,
		MaxAttempts:    j.MaxAttempts,
		RunAfter:       toTimestamptz(runAfter),
		CorrelationID:  j.CorrelationID,
	})
	if err == nil {
		return Enqueued{ID: row.ID, Kind: Kind(row.Kind), State: row.State, RunAfter: fromTimestamptz(row.RunAfter)}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Enqueued{}, fmt.Errorf("jobs: enqueueing %s: %w", j.Kind, err)
	}

	// ON CONFLICT DO NOTHING returned no row: an identical live job exists.
	// Return it rather than a second one — this is the Vidra precedent for
	// idempotent finalize, and the reason the unique index is partial over live
	// states only.
	if idem == nil {
		return Enqueued{}, fmt.Errorf("jobs: enqueueing %s: insert affected no row and no idempotency key was set", j.Kind)
	}
	live, err := q.GetLiveJobByIdempotencyKey(ctx, sqlcgen.GetLiveJobByIdempotencyKeyParams{
		Kind:           string(j.Kind),
		IdempotencyKey: idem,
	})
	if err != nil {
		return Enqueued{}, fmt.Errorf("jobs: enqueueing %s: a conflicting live job exists but could not be read: %w", j.Kind, err)
	}
	return Enqueued{
		ID: live.ID, Kind: Kind(live.Kind), State: live.State,
		RunAfter: fromTimestamptz(live.RunAfter), AlreadyInFlight: true,
	}, nil
}

// Terminal marks an error that must bypass the retry ladder entirely (ADR-004:
// `leased` -> `failed`). Wrap an error with it when retrying cannot possibly
// help — a malformed payload, a deleted subject, a permanently rejected input.
type Terminal struct{ Err error }

func (t Terminal) Error() string { return "terminal: " + t.Err.Error() }
func (t Terminal) Unwrap() error { return t.Err }

// IsTerminal reports whether err is terminal.
func IsTerminal(err error) bool {
	var t Terminal
	return errors.As(err, &t)
}

// RetryLadder is the backoff schedule for a retryable failure. It is capped:
// an unbounded ladder means a poisoned job holds a slot forever.
//
// Values are the M0 setting of ADR-004's `[to confirm in M0]` retry ladder,
// pinned here: 30s, 2m, 10m, 1h, 6h, then 6h for any further attempt.
var RetryLadder = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	time.Hour,
	6 * time.Hour,
}

// Backoff returns the delay before attempt number n (1-based), with jitter
// applied by the caller.
func Backoff(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	i := int(attempt) - 1
	if i >= len(RetryLadder) {
		i = len(RetryLadder) - 1
	}
	return RetryLadder[i]
}
