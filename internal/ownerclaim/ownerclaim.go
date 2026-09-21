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
)

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

// Mint supersedes any live token and mints a new one, returning the raw token
// and its generation. It takes the advisory lock so concurrent minters cannot
// interleave and leave the announced token different from the stored one.
//
// refuseIfUsersExist is the CLI's behaviour; boot passes false because it has
// already decided what to do about an implicitly claimed instance.
func Mint(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration, refuseIfUsersExist bool) (raw string, generation int64, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", 0, fmt.Errorf("ownerclaim: beginning mint: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockMint); err != nil {
		return "", 0, fmt.Errorf("ownerclaim: taking the mint lock: %w", err)
	}
	q := sqlcgen.New(tx)

	if refuseIfUsersExist {
		has, err := q.AnyUserExists(ctx)
		if err != nil {
			return "", 0, fmt.Errorf("ownerclaim: checking for users: %w", err)
		}
		if has {
			return "", 0, ErrHasUsers
		}
	}

	if _, err := q.SupersedeLiveOwnerClaimToken(ctx); err != nil {
		return "", 0, fmt.Errorf("ownerclaim: superseding the previous token: %w", err)
	}
	raw, digest, err := GenerateToken()
	if err != nil {
		return "", 0, err
	}
	row, err := q.MintOwnerClaimToken(ctx, sqlcgen.MintOwnerClaimTokenParams{
		TokenSha256: digest,
		Ttl:         interval(ttl),
	})
	if err != nil {
		return "", 0, fmt.Errorf("ownerclaim: minting: %w", err)
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
		return "", 0, fmt.Errorf("ownerclaim: committing mint: %w", err)
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
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	normalized := Normalize(in.Token)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Result{}, fmt.Errorf("ownerclaim: beginning claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// The claimed check strictly precedes any token examination, so a claimed
	// instance never reveals anything about a token.
	claimed, err := q.AnyUserExists(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("ownerclaim: checking claimed state: %w", err)
	}
	if claimed {
		return Result{}, ErrAlreadyClaimed
	}

	row, err := q.GetOwnerClaimToken(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		// No token was ever minted. That is the fifth indistinguishable cause.
		return Result{}, ErrTokenNotAccepted
	}
	if err != nil {
		return Result{}, fmt.Errorf("ownerclaim: reading the token: %w", err)
	}
	if subtle.ConstantTimeCompare(Digest(normalized), row.TokenSha256) != 1 {
		return Result{}, ErrTokenNotAccepted
	}

	// Only now is the password hashed. An attacker without the token never
	// triggers a single 19 MiB derivation; the hasher's counter makes that
	// assertable rather than merely asserted.
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

	claimed2, err := q.ClaimOwner(ctx, sqlcgen.ClaimOwnerParams{
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
		return Result{}, err
	}

	if err := audit.Emit(ctx, q, audit.Event{
		ActorKind:   audit.ActorUser,
		ActorUserID: &claimed2.ID,
		Action:      audit.ActionOwnerClaimSucceeded,
		SubjectType: audit.SubjectUser,
		SubjectID:   audit.String(claimed2.ID.String()),
		// username only — never the email. See the package doc and 0005's header.
		After:         map[string]any{"username": claimed2.Username, "role": string(claimed2.Role)},
		CorrelationID: nonEmpty(correlationID),
		IPPrefix:      ipPrefix,
	}); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("ownerclaim: committing claim: %w", err)
	}
	return Result{
		UserID:          claimed2.ID,
		Username:        claimed2.Username,
		Role:            string(claimed2.Role),
		TokenGeneration: claimed2.TokenGeneration,
	}, nil
}

// LiveOwnerExists distinguishes 409 from 403 when ClaimOwner returns no row.
func LiveOwnerExists(ctx context.Context, q *sqlcgen.Queries) (bool, error) {
	return q.LiveOwnerExists(ctx)
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
