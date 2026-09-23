// Package ownerclaim owns the first-run owner bootstrap (VZ-INSTALL-003).
//
// The shape, and why each part is where it is:
//
//   - The token is 256 bits of uniform randomness, stored only as its SHA-256
//     digest. A slow KDF would be wrong here: there is nothing to grind, and an
//     unauthenticated endpoint that runs one is a CPU amplifier for the attacker.
//
//   - "At most one live owner" is a UNIQUE INDEX, not a lock. The race is decided
//     by the database in both directions: the guarded UPDATE's row lock picks one
//     redeemer, and users_one_owner refuses a second owner however it is reached.
//
//   - Minting is deliberate, not incidental. A boot does not invalidate a token
//     the operator is holding; `vizra claim-token` is the explicit re-mint. The
//     previous design — re-mint on every boot — meant any restart (a compose
//     `up -d`, an OOM, a health-check flap, an updater) silently invalidated the
//     token in the operator's clipboard, and they got the uniform "not accepted"
//     message that by design tells them nothing.
package ownerclaim

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/credential"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// advisoryLockMint serialises minting across api replicas and the CLI.
//
// ADVISORY LOCK ID REGISTRY — every advisory lock Vizra takes is listed here so
// the next user of the mechanism can see what is already taken:
//
//	1 — owner-claim mint (M1-A, VZ-INSTALL-003)
//
// A literal, not pg_advisory_xact_lock(hashtext('...')): hashtext is an
// undocumented internal function with no cross-version stability contract, and
// two processes that disagreed about its value would take two different locks
// and both mint.
const advisoryLockMint int64 = 1

// TokenBytes is the raw entropy of a claim token: 256 bits, like a session id.
const TokenBytes = 32

// tokenShape is the presented form: 64 lowercase hex characters. Hex rather than
// base64url because the approved claim copy says "64 hexadecimal characters" and
// because hex survives a terminal copy with no '-', '_' or '=' to lose.
var tokenShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validation literals. These are the same expressions migration 0005 enforces;
// TestValidatorsMatchTheMigration reads that file's bytes and asserts it. They
// are duplicated in Go so that no input the API accepts can reach a CHECK and
// become a 500 on the one endpoint an operator cannot skip.
var (
	usernameShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,29}$`)
	emailShape    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

// MaxEmailBytes mirrors users_email_shape's octet_length bound. The bound is in
// BYTES: JSON Schema maxLength counts characters, and 254 non-ASCII characters
// are far more than 254 octets.
const MaxEmailBytes = 254

// Errors the HTTP layer maps to status codes.
var (
	// ErrTokenNotAccepted covers ALL FIVE indistinguishable causes: mistyped,
	// malformed, already consumed, superseded by a re-mint, expired, and never
	// minted. The server genuinely cannot tell them apart from a digest
	// comparison, and separate messages would be both a lie and an oracle.
	ErrTokenNotAccepted = errors.New("ownerclaim: token not accepted")
	// ErrAlreadyClaimed means an owner exists. Checked strictly before the token
	// is examined, so a claimed instance is never a token oracle.
	ErrAlreadyClaimed = errors.New("ownerclaim: instance already claimed")
	// ErrHasUsers is the CLI's refusal: minting an owner-creating credential on a
	// running claimed instance would be a standing escalation path for anyone
	// with `docker exec` or the DSN.
	ErrHasUsers = errors.New("ownerclaim: instance already has users")
	// ErrLiveTokenExists means a redeemable token was already minted. Boot uses
	// it to announce the command instead of minting a second one; `vizra
	// claim-token` never sees it, because a deliberate re-mint must supersede.
	ErrLiveTokenExists = errors.New("ownerclaim: a live claim token already exists")
	// ErrUnavailable marks an INFRASTRUCTURE failure — the database is
	// unreachable, a transaction could not begin or commit — as distinct from a
	// rejected credential or a constraint violation.
	//
	// It exists because the published contract promises 503 for exactly this and
	// ADR-003 states the rule ("a database outage returns 503, never 401"). A
	// connection error is not a *pgconn.PgError, so without a sentinel it falls
	// through every mapping branch to a 500 that tells the operator nothing on
	// the one endpoint they cannot skip. It must never be returned for a bad
	// token: a database error laundered into "token not accepted" is worse than
	// either answer alone.
	ErrUnavailable = errors.New("ownerclaim: the database is unavailable")
)

// unavailable wraps an infrastructure error in ErrUnavailable, preserving the
// cause for the log.
//
// A *pgconn.PgError is normally NOT wrapped, because the server answered and the
// mapper keys on its code and constraint name (23505, 23514 ...). The exception
// is a server that answered in order to say it is UNAVAILABLE — see
// IsServerUnavailable. The first version of this function exempted every
// PgError, so a claim whose connection was terminated mid-transaction (57P01)
// answered a 500 while the contract promises 503.
func unavailable(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && !IsServerUnavailable(pgErr.Code) {
		return fmt.Errorf("ownerclaim: %s: %w", op, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, op, err)
}

// IsServerUnavailable reports whether a SQLSTATE means "the database cannot
// serve this request right now" rather than "this request violated something".
//
//	08xxx  connection exception (the connection failed or was lost)
//	53xxx  insufficient resources (too many connections, out of memory, disk full)
//	57P01  admin shutdown   57P02 crash shutdown   57P03 cannot connect now
//
// These are outages, and an outage is 503 (ADR-003: "a database outage returns
// 503, never 401"), never a 500 and never a claim-specific refusal.
func IsServerUnavailable(code string) bool {
	switch {
	case strings.HasPrefix(code, "08"), strings.HasPrefix(code, "53"):
		return true
	case code == "57P01", code == "57P02", code == "57P03":
		return true
	}
	return false
}

// ValidationError names the first offending field in prose. The per-field error
// envelope is M1-B's, where sign-up is the real multi-field consumer.
type ValidationError struct{ Field, Message string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// GenerateToken returns a fresh raw token and its storage digest.
func GenerateToken() (raw string, digest []byte, err error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("ownerclaim: reading entropy: %w", err)
	}
	raw = hex.EncodeToString(b)
	return raw, Digest(raw), nil
}

// Normalize is what an operator's paste needs before it can be compared: strip
// surrounding whitespace (a copied line brings a newline) and lowercase the hex.
func Normalize(token string) string { return strings.ToLower(strings.TrimSpace(token)) }

// Digest is SHA-256 over the normalised token.
func Digest(normalized string) []byte {
	sum := sha256.Sum256([]byte(normalized))
	return sum[:]
}

// Input is a claim request after decoding, before validation.
type Input struct {
	Token    string
	Username string
	Email    string
	Password string
}

// Validate checks every field against the same literals the DDL enforces.
//
// A token that fails the SHAPE check returns ErrTokenNotAccepted, not a
// validation error: answering "your token is the wrong length" with a 400 would
// contradict the single-message rule the moment an attacker sent a 63-character
// guess. Field errors are for the username, email and password only.
func (in Input) Validate() error {
	if !tokenShape.MatchString(Normalize(in.Token)) {
		return ErrTokenNotAccepted
	}
	if !usernameShape.MatchString(in.Username) {
		return &ValidationError{Field: "username",
			Message: "use 3 to 30 characters: letters, numbers, hyphen or underscore, starting with a letter or number"}
	}
	if len(in.Email) > MaxEmailBytes || !emailShape.MatchString(in.Email) {
		return &ValidationError{Field: "email",
			Message: fmt.Sprintf("enter an email address of at most %d bytes", MaxEmailBytes)}
	}
	switch n := len([]rune(in.Password)); {
	case n < credential.MinPasswordRunes:
		return &ValidationError{Field: "password",
			Message: fmt.Sprintf("use at least %d characters", credential.MinPasswordRunes)}
	case n > credential.MaxPasswordRunes:
		return &ValidationError{Field: "password",
			Message: fmt.Sprintf("use at most %d characters", credential.MaxPasswordRunes)}
	}
	if len(in.Password) > credential.MaxPasswordBytes {
		return &ValidationError{Field: "password",
			Message: fmt.Sprintf("use a password of at most %d bytes", credential.MaxPasswordBytes)}
	}
	return nil
}

// Result is a successful claim.
type Result struct {
	UserID          uuid.UUID
	Username        string
	Role            string
	TokenGeneration int64
}

// Claimed reports whether the instance has any user at all.
//
// The gate is EXISTS(users), not EXISTS(owner): an instance that already has
// users is implicitly claimed and must never mint again (the ledger's own
// wording), and tombstoning the owner must not reopen the claim endpoint.
func Claimed(ctx context.Context, q *sqlcgen.Queries) (bool, error) {
	return q.AnyUserExists(ctx)
}

// Mint mints a fresh token and returns it with its generation.
//
// EVERY decision it makes is taken INSIDE the advisory lock. That is the whole
// point of the lock and it was the defect in the first round: boot read liveness
// before taking it, so two replicas cold-starting together both observed "no
// live token", both entered Mint, and the second overwrote the first's digest in
// place. Two byte-identical-looking 64-hex lines then sat in the log, one dead,
// and the operator who picked the wrong one got the deliberately uninformative
// "that claim token was not accepted".
//
//   - refuseIfUsersExist — `vizra claim-token` passes true: minting an
//     owner-creating credential on a running claimed instance would be a standing
//     escalation path for anyone with `docker exec` or the DSN.
//   - onlyIfNoLiveToken — boot passes true and announces the command instead when
//     ErrLiveTokenExists comes back (with the live generation, so the operator can
//     match the line to the instance). A deliberate re-mint passes false: the CLI
//     must always supersede, which is what makes it a usable recovery path.
func Mint(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration, refuseIfUsersExist, onlyIfNoLiveToken bool) (raw string, generation int64, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", 0, unavailable("beginning mint", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockMint); err != nil {
		return "", 0, unavailable("taking the mint lock", err)
	}
	q := sqlcgen.New(tx)

	if refuseIfUsersExist {
		has, err := q.AnyUserExists(ctx)
		if err != nil {
			return "", 0, unavailable("checking for users", err)
		}
		if has {
			return "", 0, ErrHasUsers
		}
	}

	// Read liveness UNDER the lock, never before it.
	prior, err := State(ctx, q)
	if err != nil {
		return "", 0, unavailable("reading the token state", err)
	}
	if onlyIfNoLiveToken && prior.Live {
		return "", prior.Generation, ErrLiveTokenExists
	}

	// A re-mint kills the previous generation. There is no separate UPDATE for
	// that: the upsert below overwrites the digest in place, which is what makes
	// invalidation atomic and total. What the previous round lacked was the
	// HISTORY — a re-mint recorded `minted` and nothing about the generation it
	// destroyed. The dead `SupersedeLiveOwnerClaimToken` call that used to sit
	// here was overwritten by the upsert in the same transaction and is gone.
	if prior.Live {
		if err := audit.Emit(ctx, q, audit.Event{
			ActorKind:   audit.ActorSystem,
			Action:      audit.ActionOwnerClaimSuperseded,
			SubjectType: audit.SubjectOwnerClaimToken,
			SubjectID:   audit.String(fmt.Sprintf("%d", prior.Generation)),
		}); err != nil {
			return "", 0, err
		}
	}

	raw, digest, err := GenerateToken()
	if err != nil {
		return "", 0, err
	}
	row, err := q.MintOwnerClaimToken(ctx, sqlcgen.MintOwnerClaimTokenParams{
		TokenSha256: digest,
		Ttl:         interval(ttl),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The statement's own WHERE NOT EXISTS (SELECT 1 FROM users) refused it.
		// "Never mint on a claimed instance" is a property of the statement, not
		// of the two callers that happen to check first.
		return "", 0, ErrHasUsers
	}
	if err != nil {
		return "", 0, unavailable("minting", err)
	}
	expires := row.ExpiresAt.Time.UTC().Format(time.RFC3339)
	if err := audit.Emit(ctx, q, audit.Event{
		ActorKind:   audit.ActorSystem,
		Action:      audit.ActionOwnerClaimMinted,
		SubjectType: audit.SubjectOwnerClaimToken,
		SubjectID:   audit.String(fmt.Sprintf("%d", row.Generation)),
		After:       map[string]any{"expires_at": expires},
	}); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, unavailable("committing mint", err)
	}
	return raw, row.Generation, nil
}

// SupersedeLive retires a live token without minting a replacement. Boot calls
// it when users already exist: an implicitly claimed instance must never hold a
// live credential that creates an owner.
func SupersedeLive(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	n, err := q.SupersedeLiveOwnerClaimToken(ctx)
	if err != nil || n == 0 {
		return n, err
	}
	row, err := q.GetOwnerClaimToken(ctx)
	if err != nil {
		return 0, err
	}
	if err := audit.Emit(ctx, q, audit.Event{
		ActorKind:   audit.ActorSystem,
		Action:      audit.ActionOwnerClaimSuperseded,
		SubjectType: audit.SubjectOwnerClaimToken,
		SubjectID:   audit.String(fmt.Sprintf("%d", row.Generation)),
	}); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// TokenState is what boot and doctor need to decide what to say.
type TokenState struct {
	Exists     bool
	Live       bool
	Generation int64
}

// State reads the current token row.
func State(ctx context.Context, q *sqlcgen.Queries) (TokenState, error) {
	row, err := q.GetOwnerClaimToken(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenState{}, nil
	}
	if err != nil {
		return TokenState{}, err
	}
	live := row.Live != nil && *row.Live
	return TokenState{Exists: true, Live: live, Generation: row.Generation}, nil
}

// Claim redeems the token and creates the owner, its password credential and the
// audit row in ONE explicit READ COMMITTED transaction.
//
// The isolation level is pinned rather than inherited: default_transaction_isolation
// is a server GUC an operator, a managed provider or a pooler can set, and under
// REPEATABLE READ every losing claimant would get 40001 instead of zero rows —
// which would surface as a 500 on exactly the race the ledger's negative case
// names, and only in production. There is no retry: under READ COMMITTED the
// loser's answer is deterministic, so there is nothing to retry.
func Claim(ctx context.Context, pool *pgxpool.Pool, hasher credential.Hasher, in Input, correlationID string, ipPrefix *string) (Result, error) {
	q := sqlcgen.New(pool)

	// The claimed check strictly precedes ANY token examination (OQ-4) — and that
	// includes the token SHAPE check inside Validate. The previous order validated
	// first, so on a claimed instance a malformed token was refused as a TOKEN
	// problem: a 403 instead of the ruled 409, a failure-budget charge, and a
	// permanent `refused` audit row, per request, for as long as a scanner kept
	// sending malformed bodies.
	claimed, err := q.AnyUserExists(ctx)
	if err != nil {
		return Result{}, unavailable("checking claimed state", err)
	}
	if claimed {
		return Result{}, ErrAlreadyClaimed
	}

	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	normalized := Normalize(in.Token)

	// ---- READ PHASE: no transaction, and no connection held across the hash --
	//
	// argon2id at ADR-003's parameters costs 19 MiB and tens of milliseconds, and
	// a caller that loses the semaphore race waits until its request deadline. If
	// that happened inside the transaction, every waiter would pin a pooled
	// connection "idle in transaction" for the whole derivation plus the queue —
	// so the POOL, not the CPU, would become the limit, and the symptom would
	// point at PostgreSQL rather than at the hasher.
	//
	// This costs no correctness. Nothing read here is trusted: the authoritative
	// gate re-runs inside the transaction below, the redeem CTE re-matches the
	// row's digest against the COMMITTED row under READ COMMITTED, and
	// users_one_owner is the final arbiter. A stale read can only send us into a
	// transaction that then refuses.
	//
	// M1-B's sign-in is the real multi-caller of this hasher and is instructed to
	// import it; this is the call shape it should copy.
	row, err := q.GetOwnerClaimToken(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		// No token was ever minted. That is the fifth indistinguishable cause.
		return Result{}, ErrTokenNotAccepted
	}
	if err != nil {
		return Result{}, unavailable("reading the token", err)
	}
	if subtle.ConstantTimeCompare(Digest(normalized), row.TokenSha256) != 1 {
		return Result{}, ErrTokenNotAccepted
	}

	// LIVENESS PRE-CHECK. The redeem CTE's guard is the enforcement and stays;
	// this is about the cost incurred BEFORE it. Without this, a token that is
	// correct but consumed, superseded or expired — one an attacker may already
	// hold, from a log under the stderr opt-in or after an operator re-mint —
	// buys a full derivation before the CTE refuses it. The `live` column is
	// already selected and is computed by the DATABASE clock.
	//
	// It returns the same ErrTokenNotAccepted, so nothing observable changes: the
	// five causes stay indistinguishable in status, code and message.
	if row.Live == nil || !*row.Live {
		return Result{}, ErrTokenNotAccepted
	}

	// Only now is the password hashed, and no connection is checked out while it
	// happens.
	hash, err := hasher.Hash(ctx, in.Password)
	if err != nil {
		return Result{}, err
	}

	userID, err := uuid.NewV7()
	if err != nil {
		return Result{}, fmt.Errorf("ownerclaim: generating user id: %w", err)
	}
	credID, err := uuid.NewV7()
	if err != nil {
		return Result{}, fmt.Errorf("ownerclaim: generating credential id: %w", err)
	}

	// ---- WRITE PHASE: one explicit transaction, exactly the ruled contents ----
	// gate -> ClaimOwner -> audit insert -> commit. Nothing else belongs here.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Result{}, unavailable("beginning claim", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := sqlcgen.New(tx)

	// THE authoritative gate. The read-phase check above is an optimisation; this
	// one decides, inside the transaction that does the writing.
	claimed, err = qtx.AnyUserExists(ctx)
	if err != nil {
		return Result{}, unavailable("checking claimed state", err)
	}
	if claimed {
		return Result{}, ErrAlreadyClaimed
	}

	created, err := qtx.ClaimOwner(ctx, sqlcgen.ClaimOwnerParams{
		// The ROW'S OWN digest, never the presented value: attacker-controlled
		// bytes must not reach SQL.
		TokenSha256:  row.TokenSha256,
		UserID:       userID,
		Username:     in.Username,
		Email:        in.Email,
		CredentialID: credID,
		PasswordHash: hash,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err // the caller re-reads the claimed state to pick 409 or 403
		}
		// unavailable() passes a CONSTRAINT PgError through untouched for the
		// mapper to key on, and wraps connection failures and server-signalled
		// outages alike.
		return Result{}, unavailable("redeeming the token", err)
	}

	if err := audit.Emit(ctx, qtx, audit.Event{
		ActorKind:   audit.ActorUser,
		ActorUserID: &created.ID,
		Action:      audit.ActionOwnerClaimSucceeded,
		SubjectType: audit.SubjectUser,
		SubjectID:   audit.String(created.ID.String()),
		// username only — never the email. See the package doc and 0005's header.
		After:         map[string]any{"username": created.Username, "role": string(created.Role)},
		CorrelationID: nonEmpty(correlationID),
		IPPrefix:      ipPrefix,
	}); err != nil {
		// Inside the claim transaction: a lost connection here is an outage, and
		// must answer 503 like every other one rather than fall through to a 500.
		return Result{}, unavailable("recording the claim", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, unavailable("committing claim", err)
	}
	return Result{
		UserID:          created.ID,
		Username:        created.Username,
		Role:            string(created.Role),
		TokenGeneration: created.TokenGeneration,
	}, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: int64(d / time.Microsecond), Valid: true}
}
