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

// DefaultPriority: lower runs first.
const DefaultPriority = 100

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
	Priority       int16
	MaxAttempts    int32
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
	raw, err := json.Marshal(payload)
	if err != nil {
		return Enqueued{}, fmt.Errorf("jobs: encoding payload for kind %s: %w", j.Kind, err)
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
