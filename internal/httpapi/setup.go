package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/credential"
	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// Route paths. Declared once so the allowlist, the router and the contract test
// cannot drift apart.
const (
	apiV1Prefix            = "/api/v1"
	pathSetupClaimStatus   = "/api/v1/setup/claim-status"
	pathSetupClaimOwner    = "/api/v1/setup/claim-owner"
	claimBodyLimitBytes    = 8 << 10 // 8 KiB
	claimRateWindow        = 15 * time.Minute
	claimFailuresPerOrigin = 10
	claimFailuresGlobal    = 60
	// The two setup routes carry SEPARATE hard ceilings, and that separation is
	// the point: sharing one counter meant 600 body-less anonymous GETs — no
	// token, no body, no content type, no origin header — exhausted the window
	// and the operator's POST carrying the CORRECT token was answered 429 for the
	// next fifteen minutes, repeatable indefinitely at ~40 requests a minute, on
	// the very endpoint that advertises `claimed:false` to a scanner. That is
	// denial of claim by a stranger: the exact failure the failure-keyed limiter
	// was adopted to remove, reached more cheaply, and only during the unclaimed
	// window, which is the one window where it matters.
	//
	// The ruling said the ceiling may refuse a valid token. It never said the
	// read surface should compete for the write surface's budget.
	//
	// claimOwnerCeiling bounds POSTs. Each one costs a body read, a strict
	// decode, and — only past the token compare — an argon2id derivation.
	claimOwnerCeiling = 600
	// claimStatusCeiling bounds the read. It is higher because the work is
	// smaller: once an instance is claimed the answer comes from the monotonic
	// in-process cache and touches no database at all, and while it is unclaimed
	// it is one trivial indexed EXISTS.
	claimStatusCeiling = 3000
)

// codedError carries an application error code distinct from the one the status
// implies. The base handler derives `code` from the status alone, which is right
// for generic failures but cannot express "this 403 is a misconfigured origin,
// not a rejected token" — a distinction an operator needs to diagnose a claim
// page that fails while curl works.
type codedError struct {
	status  int
	code    string
	message string
}

func (e *codedError) Error() string   { return e.message }
func (e *codedError) StatusCode() int { return e.status }

func newCodedError(status int, code, message string) error {
	return &codedError{status: status, code: code, message: message}
}

// claimedCache memoises "does this instance have an owner yet".
//
// The bit is MONOTONIC: it goes false -> true exactly once and can never go
// back, because the gate is EXISTS(users) and no path deletes the last user
// (audit_events_actor_user_fk RESTRICTs it). So once true it is cached forever,
// and the guard costs nothing for the entire life of the instance. While false
// it is cached only briefly, because an unclaimed instance is short-lived by
// construction and a stale false merely delays the guard opening.
type claimedCache struct {
	claimed atomic.Bool
	mu      atomic.Int64 // unix nanos of the last negative lookup
}

const claimedNegativeTTL = time.Second

func (cc *claimedCache) get(now time.Time) (claimed bool, fresh bool) {
	if cc.claimed.Load() {
		return true, true
	}
	last := cc.mu.Load()
	if last != 0 && now.UnixNano()-last < int64(claimedNegativeTTL) {
		return false, true
	}
	return false, false
}

func (cc *claimedCache) set(claimed bool, now time.Time) {
	if claimed {
		cc.claimed.Store(true)
		return
	}
	cc.mu.Store(now.UnixNano())
}

// claimExemptRoutes is the allowlist: what an UNCLAIMED instance will serve.
//
// Everything else answers 403 until an owner exists, including the router's 404
// path. That cannot be wrong at M1-A, because nothing can legitimately exist
// before an owner does. M1-B adds sign-in here.
var claimExemptRoutes = map[string]bool{
	"GET /healthz":                true,
	"GET /readyz":                 true,
	"GET /version":                true,
	"GET /schemaz":                true,
	"GET " + pathSetupClaimStatus: true,
	"POST " + pathSetupClaimOwner: true,
}

// claimGuardedRoutes is the other half of the classification. Every route this
// server serves must appear in exactly one of the two sets, and
// TestEveryRouteIsEitherUnclaimedAllowlistedOrGuarded enforces it.
//
// Why two hand-maintained sets rather than one: a guard a future author must
// remember to attach is fail-open, and "every signup path answers 403 while
// unclaimed" is an acceptance bullet. With this shape, M1-B's
// POST /api/v1/auth/register fails the test until its author classifies it
// deliberately. It is empty today because everything M1-A serves is exempt.
var claimGuardedRoutes = map[string]bool{}

// requireClaimedMiddleware is the structural guard.
func (s *Server) requireClaimedMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			key := c.Request().Method + " " + c.Request().URL.Path
			if claimExemptRoutes[key] {
				return next(c)
			}
			claimed, err := s.instanceClaimed(c)
			if err != nil {
				// Never 403 and never allow on a lookup failure: a database
				// outage is 503, exactly as ADR-003 requires for identity reads.
				return newCodedError(http.StatusServiceUnavailable, "unavailable",
					"the instance state could not be read")
			}
			if !claimed {
				return newCodedError(http.StatusForbidden, "instance_unclaimed",
					"this instance has no owner yet; every other route is refused until it is claimed")
			}
			return next(c)
		}
	}
}

func (s *Server) instanceClaimed(c *echo.Context) (bool, error) {
	now := s.deps.Now()
	if claimed, fresh := s.claimed.get(now); fresh {
		return claimed, nil
	}
	claimed, err := s.lookupClaimed(c)
	if err != nil {
		return false, err
	}
	s.claimed.set(claimed, now)
	return claimed, nil
}

func (s *Server) lookupClaimed(c *echo.Context) (bool, error) {
	if s.deps.InstanceClaimed != nil {
		return s.deps.InstanceClaimed(c.Request().Context())
	}
	pool, err := s.poolFor(c)
	if err != nil {
		return false, err
	}
	return ownerclaim.Claimed(c.Request().Context(), sqlcgen.New(pool))
}

func (s *Server) poolFor(c *echo.Context) (*pgxpool.Pool, error) {
	st, ok := site.FromContext(c.Request().Context())
	if !ok {
		return nil, errors.New("httpapi: no site on the request context")
	}
	if s.deps.Pools == nil {
		return nil, errors.New("httpapi: no database pools configured")
	}
	pool, ok := s.deps.Pools.For(st)
	if !ok || pool == nil {
		return nil, errors.New("httpapi: no pool for this site")
	}
	return pool, nil
}

// handleClaimStatus answers exactly one bit.
//
// Not minted_at, not expires_at, not the generation, not whether a token is
// live, not a user count, not the owner's name. The claim page needs one bit to
// choose between the form and the "already claimed" screen; every other field
// would be secret material, a freshness oracle for a token-guessing attacker, or
// an enumeration aid. `claimed: false` does advertise a target, but so does the
// public /setup/claim page and an empty gallery — concealment there is illusory
// while the token remains the real control.
func (s *Server) handleClaimStatus(c *echo.Context) error {
	// Under the hard ceiling like the POST: this is the only unauthenticated read
	// surface the product has, it carries Cache-Control: no-store so nothing
	// upstream absorbs a flood, and post-claim the answer is a constant.
	if !s.allowSetupRequest(c, "ceiling.status", claimStatusCeiling) {
		return newCodedError(http.StatusTooManyRequests, "rate_limited",
			"too many requests to the setup endpoint; try again shortly")
	}
	// instanceClaimed, not lookupClaimed: the monotonic cache means that once an
	// instance is claimed this performs no database work, ever again.
	claimed, err := s.instanceClaimed(c)
	if err != nil {
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the instance state could not be read")
	}
	s.claimed.set(claimed, s.deps.Now())
	return c.JSON(http.StatusOK, map[string]bool{"claimed": claimed})
}

type claimOwnerRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type claimOwnerResponse struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// handleClaimOwner redeems the one-time token for THE owner account.
//
// The order of work is load-bearing and is asserted by tests, not just written
// here: every cheap refusal happens before anything expensive, and the password
// is hashed only after the token digest has compared equal. An unauthenticated
// endpoint that runs argon2id at 19 MiB per call before checking a credential is
// a memory and CPU amplifier handed to the attacker.
func (s *Server) handleClaimOwner(c *echo.Context) error {
	req := c.Request()
	ctx := req.Context()

	// This route's OWN hard ceiling. A claim-status flood cannot spend it.
	if !s.allowSetupRequest(c, "ceiling.claim", claimOwnerCeiling) {
		return newCodedError(http.StatusTooManyRequests, "rate_limited",
			"too many requests to the setup endpoint; try again shortly")
	}

	// Refuse a CLAIMED instance before parsing anything.
	//
	// After day one every instance is claimed, so this is the path every
	// anonymous POST takes for the rest of the product's life. Without it each
	// request cost database work AND could write a permanent row into a table
	// migration 0005 makes undeletable: no DELETE, no TRUNCATE, no retention path
	// yet. The hard ceiling bounds requests per window, but a per-window bound
	// integrates to unbounded over the life of an instance.
	//
	// instanceClaimed, not a peek at the cache: on a COLD cache — a freshly
	// restarted process over an instance claimed long ago — a peek found nothing,
	// so the request went on to parse the body and examine the token, and a
	// malformed one wrote a permanent `refused` row per request. This reads the
	// claimed state (from the cache, or once from the database, WARMING the cache
	// for every later request) before the body is even read, so a claimed
	// instance answers 409 and writes nothing whatever the request carries.
	//
	// The bit only ever goes false->true (the gate is EXISTS(users) and no path
	// deletes the last user), so a cached true is never wrong. The in-transaction
	// AnyUserExists inside Claim, and the redeem statement's own users guard,
	// stay exactly where they are: this is a short-circuit, not the gate.
	claimed, err := s.instanceClaimed(c)
	if err != nil {
		return s.unavailable(c, "checking claimed state", err)
	}
	if claimed {
		return newCodedError(http.StatusConflict, "conflict", claimedMessage)
	}

	// 1. Media type. Echo's binder would accept urlencoded and multipart and
	//    silently skip fields lacking a `form` tag, so c.Bind is never used here.
	if err := requireJSONContentType(req); err != nil {
		return err
	}

	// 2. Bound the body itself, not Content-Length: a chunked request has no
	//    Content-Length to check.
	body := http.MaxBytesReader(c.Response(), req.Body, claimBodyLimitBytes)
	defer func() { _ = body.Close() }()

	// 3. Strict decode: DisallowUnknownFields honours the spec's
	//    additionalProperties: false, which is otherwise only documentation.
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var in claimOwnerRequest
	if err := dec.Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return newCodedError(http.StatusRequestEntityTooLarge, "payload_too_large",
				"the request body is too large")
		}
		if errors.Is(err, io.EOF) {
			return newCodedError(http.StatusBadRequest, "bad_request", "the request body is empty")
		}
		return newCodedError(http.StatusBadRequest, "bad_request",
			"the request body is not the expected JSON object")
	}
	if dec.More() {
		return newCodedError(http.StatusBadRequest, "bad_request",
			"the request body must contain exactly one JSON object")
	}

	// 4. Origin posture. Absence of both headers is allowed: the credential is in
	//    the body, not ambient, so curl and the CLI must keep working. Present
	//    and cross-site is refused, which is what stops a browser on a reachable
	//    network being used as a confused deputy by someone holding a leaked
	//    token but no route to the instance.
	if err := s.checkOrigin(req); err != nil {
		return err
	}

	pool, err := s.poolFor(c)
	if err != nil {
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the instance state could not be read")
	}

	result, err := ownerclaim.Claim(ctx, pool, s.deps.Hasher, ownerclaim.Input{
		Token:    in.Token,
		Username: in.Username,
		Email:    in.Email,
		Password: in.Password,
	}, requestIDOf(c), clientIPPrefix(req))
	if err != nil {
		return s.mapClaimError(c, err)
	}

	s.claimed.set(true, s.deps.Now())
	return c.JSON(http.StatusCreated, claimOwnerResponse{
		Username: result.Username,
		Role:     result.Role,
	})
}

func requireJSONContentType(req *http.Request) error {
	ct := req.Header.Get("Content-Type")
	if ct == "" {
		return newCodedError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"send application/json")
	}
	mt, _, err := mime.ParseMediaType(ct)
	// Parameters such as `; charset=utf-8` are permitted: they do not change how
	// the body parses. Any other media type is refused outright.
	if err != nil || !strings.EqualFold(mt, "application/json") {
		return newCodedError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"send application/json")
	}
	return nil
}

// checkOrigin enforces ADR-003's same-origin posture for the one request class
// its table has no row for: an unauthenticated state-changing POST whose only
// credential is in the body. The chair recorded that interpretation; an amending
// ADR adding the row is owed before M1-B merges.
func (s *Server) checkOrigin(req *http.Request) error {
	// Compare NORMALISED origins, not strings. Config.PublicOrigin was normalised
	// once at boot; the request's Origin is normalised the same way here, so a
	// trailing slash, an uppercase host, an explicitly written default port or a
	// trailing dot on either side cannot turn a same-origin browser claim into a
	// 403 that only curl escapes.
	want := s.deps.Config.PublicOrigin
	if origin := req.Header.Get("Origin"); origin != "" {
		gotN, wantN := config.NormalizeOrigin(origin), config.NormalizeOrigin(want)
		// DENY when either side is unnormalisable. NormalizeOrigin returns "" for
		// anything it cannot render as a browser origin — `Origin: null` from a
		// sandboxed iframe or a data: document, and a configured value that is
		// not an origin at all. Comparing them for equality would make two
		// failures agree: "" == "" would ALLOW. Boot refuses such a configuration
		// today, so this is unreachable — but a comparison that fails open is not
		// something to leave standing on the strength of a guard elsewhere.
		if gotN == "" || wantN == "" || gotN != wantN {
			// A DISTINCT code, because a wrong VIZRA_PUBLIC_ORIGIN breaks the browser
			// claim page while curl still works, and an operator has to be able to
			// tell that apart from a rejected token.
			return newCodedError(http.StatusForbidden, "origin_mismatch",
				"the request Origin does not match this instance's configured public origin")
		}
	}
	switch sfs := req.Header.Get("Sec-Fetch-Site"); sfs {
	case "", "same-origin", "none":
	default:
		return newCodedError(http.StatusForbidden, "origin_mismatch",
			"this request appears to come from another site")
	}
	return nil
}

// mapClaimError is the complete mapping. Every branch is covered by
// TestClaimErrorMapping over synthetic *pgconn.PgError values, because the SQL
// guard proves the database behaves and says nothing about what the handler does
// with what it returns.
func (s *Server) mapClaimError(c *echo.Context, err error) error {
	switch {
	case errors.Is(err, ownerclaim.ErrAlreadyClaimed):
		// Deliberately NO audit row. A refusal on a permanently closed endpoint
		// is not a security event: the answer is a constant, so the first row is
		// already indistinguishable from the ten-thousandth, and the table it
		// would grow cannot be pruned. The claimed bit is cached here so the
		// next request never reaches the database at all.
		s.claimed.set(true, s.deps.Now())
		return newCodedError(http.StatusConflict, "conflict", claimedMessage)

	case errors.Is(err, ownerclaim.ErrTokenNotAccepted):
		// Claim returns this only from ownerclaim.classifyRefusal, AFTER a fresh
		// read found the instance still unclaimed. The 409-vs-403 decision is
		// made there and nowhere else — this layer used to carry a second copy
		// of it for the redeem's empty result, while the read phase's own
		// refusals were never classified at all.
		return s.refuseToken(c)

	case errors.Is(err, credential.ErrBusy):
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the server is busy; try again shortly")

	case errors.Is(err, ownerclaim.ErrUnavailable):
		// The published contract promises 503 for a database outage and ADR-003
		// states the rule. Without this branch a connection error — which is not
		// a *pgconn.PgError — falls through every mapping below to a 500 that
		// tells the operator nothing on the one endpoint they cannot skip.
		return s.unavailable(c, "claim", err)

	}

	var ve *ownerclaim.ValidationError
	if errors.As(err, &ve) {
		return newCodedError(http.StatusBadRequest, "bad_request", ve.Message)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case ownerclaim.IsServerUnavailable(pgErr.Code):
			// The server answered, but to say it cannot serve: an outage.
			return s.unavailable(c, "claim", err)
		case pgErr.Code == "23505" && pgErr.ConstraintName == "users_one_owner":
			// The index fired: an owner was created out of band between the
			// claimed check and the insert. One owner still exists, the token is
			// unconsumed, and no credential row was orphaned — the whole
			// statement rolled back.
			s.claimed.set(true, s.deps.Now())
			return newCodedError(http.StatusConflict, "conflict", claimedMessage)
		case pgErr.Code == "23505":
			// username_fold / email_fold: a duplicate identifier, never a 500.
			return newCodedError(http.StatusConflict, "conflict",
				"that username or email address is already taken")
		case pgErr.Code == "23514":
			// Backstop only. The Go validator compiles the same literals the DDL
			// enforces, so reaching here means the two drifted — which is a 400
			// for the caller and a defect for us.
			s.deps.Logger.Error("http: a CHECK constraint refused a request the validator accepted",
				"constraint", obs.Redact(pgErr.ConstraintName), "request_id", obs.Redact(requestIDOf(c)))
			return newCodedError(http.StatusBadRequest, "bad_request",
				"one of the submitted values is not acceptable")
		case pgErr.Code == "40001":
			// Unreachable: the claim transaction pins READ COMMITTED explicitly.
			s.deps.Logger.Error("http: serialization failure on a READ COMMITTED claim",
				"request_id", obs.Redact(requestIDOf(c)))
		}
	}

	if errors.Is(err, ctxDeadline) || errors.Is(err, ctxCanceled) {
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the request could not be completed in time")
	}
	return err // 500 via the shared handler, with the cause logged there
}

// unavailable answers 503 and logs the CAUSE exactly once, redacted.
//
// The canned client message is deliberate — it tells an attacker nothing — but
// the first version of this branch dropped the wrapped pgconn error entirely, so
// a database outage on the one endpoint an operator cannot skip produced a 503
// whose only trace was "the instance state could not be read". That is not
// diagnosable. The cause goes to the log with the request id, through
// obs.Redact, so a DSN or password embedded in a driver error cannot reach a log
// line (AGENTS.md) while the operator still gets something to act on.
func (s *Server) unavailable(c *echo.Context, where string, cause error) error {
	s.deps.Logger.Error("http: the claim endpoint could not reach the database",
		"where", obs.Redact(where),
		"error", obs.Redact(cause.Error()),
		"request_id", obs.Redact(requestIDOf(c)))
	return newCodedError(http.StatusServiceUnavailable, "unavailable",
		"the instance state could not be read")
}

const claimedMessage = "this instance already has an owner"

// tokenRefusedMessage is deliberately ONE message for all five causes — a
// mistyped token, an already-redeemed one, one superseded by a re-mint, an
// expired one, and an instance that never minted one. The server cannot tell
// them apart from a digest comparison, and four messages would be both a lie and
// an oracle.
const tokenRefusedMessage = "that claim token was not accepted"

func (s *Server) refuseToken(c *echo.Context) error {
	// Only a REJECTED attempt consumes failure budget. A request carrying the
	// correct token is never answered 429 by this limiter, which is what stops a
	// stranger sending junk until the operator is locked out of their own
	// instance.
	if limited := s.consumeClaimFailure(c); limited {
		// Deliberately NO `refused` audit row here: a 429 answers requests that
		// by definition have no upper bound, so auditing each one would make the
		// limiter an unbounded writer into a table nothing can delete. Exactly
		// one `rate_limited` row is written, on the transition into the limited
		// state.
		if s.claimLimitTransition(c) {
			s.recordClaimRateLimited(c, "failure")
		}
		return newCodedError(http.StatusTooManyRequests, "rate_limited",
			"too many failed attempts; try again shortly")
	}
	s.recordClaimRefusal(c, audit.ReasonTokenNotAccepted)
	return newCodedError(http.StatusForbidden, "forbidden", tokenRefusedMessage)
}
