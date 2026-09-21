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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/credential"
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
	// claimHardCeiling bounds TOTAL requests to the setup routes, not just
	// failures, so a flood of one-row SELECTs cannot exhaust the pool. It is set
	// far above any operator-plausible use. A flood past this point is a
	// network-level denial of service and is not something an application
	// limiter can answer; saying so is more honest than implying otherwise.
	claimHardCeiling = 600
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
	claimed, err := s.lookupClaimed(c)
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

	// The hard ceiling covers every request, valid or not, and protects the pool.
	if !s.allowClaimRequest(c) {
		return newCodedError(http.StatusTooManyRequests, "rate_limited",
			"too many requests to the setup endpoint; try again shortly")
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
	want := s.deps.Config.PublicOrigin
	if origin := req.Header.Get("Origin"); origin != "" && origin != want {
		// A DISTINCT code, because a wrong VIZRA_PUBLIC_ORIGIN breaks the browser
		// claim page while curl still works, and an operator has to be able to
		// tell that apart from a rejected token.
		return newCodedError(http.StatusForbidden, "origin_mismatch",
			"the request Origin does not match this instance's configured public origin")
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
		s.recordClaimRefusal(c, audit.ReasonAlreadyClaimed)
		return newCodedError(http.StatusConflict, "conflict", claimedMessage)

	case errors.Is(err, ownerclaim.ErrTokenNotAccepted):
		return s.refuseToken(c)

	case errors.Is(err, credential.ErrBusy):
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the server is busy; try again shortly")

	case errors.Is(err, pgx.ErrNoRows):
		// The redeem matched nothing: a concurrent claimer won, or the token was
		// already terminal. Re-read the claimed state to answer the right one.
		if pool, perr := s.poolFor(c); perr == nil {
			if owner, oerr := ownerclaim.LiveOwnerExists(c.Request().Context(), sqlcgen.New(pool)); oerr == nil && owner {
				s.claimed.set(true, s.deps.Now())
				return newCodedError(http.StatusConflict, "conflict", claimedMessage)
			}
		}
		return s.refuseToken(c)
	}

	var ve *ownerclaim.ValidationError
	if errors.As(err, &ve) {
		return newCodedError(http.StatusBadRequest, "bad_request", ve.Message)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
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
				"constraint", pgErr.ConstraintName, "request_id", requestIDOf(c))
			return newCodedError(http.StatusBadRequest, "bad_request",
				"one of the submitted values is not acceptable")
		case pgErr.Code == "40001":
			// Unreachable: the claim transaction pins READ COMMITTED explicitly.
			s.deps.Logger.Error("http: serialization failure on a READ COMMITTED claim",
				"request_id", requestIDOf(c))
		}
	}

	if errors.Is(err, ctxDeadline) || errors.Is(err, ctxCanceled) {
		return newCodedError(http.StatusServiceUnavailable, "unavailable",
			"the request could not be completed in time")
	}
	return err // 500 via the shared handler, with the cause logged there
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
