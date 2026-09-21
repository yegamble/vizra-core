package httpapi

import (
	"context"
	"errors"

	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/yegamble/vizra-core/internal/audit"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

var (
	ctxDeadline = context.DeadlineExceeded
	ctxCanceled = context.Canceled
)

func requestIDOf(c *echo.Context) string {
	id, _ := c.Get(headerRequestID).(string)
	return id
}

// clientIPPrefix is the ONE place a client's network identity is derived.
//
// Attribution rule: when a forwarded header is PRESENT and no trusted-proxy
// configuration exists, there is no honest per-client identity, so the answer is
// nil. Writing the proxy's own private prefix would be worse than writing
// nothing twice over — every audit row would attribute the attempt to the proxy
// (permanently, in a table nothing can delete), and the per-origin rate-limit
// bucket would collapse into one bucket shared by the entire internet.
//
// VIZRA_TRUSTED_PROXIES is M1-B's. Until it exists this degrades to "unknown"
// rather than lying, which is the same answer migration 0003's header prescribes
// for "no usable address".
//
// Every caller — the audit writer and the limiter both — must go through here.
// Deriving it separately in one of them is how the proxy's address ends up in
// the audit table while the limiter is doing the right thing.
func clientIPPrefix(req *http.Request) *string {
	if req.Header.Get("X-Forwarded-For") != "" || req.Header.Get("Forwarded") != "" {
		return nil
	}
	return audit.IPPrefix(req.RemoteAddr)
}

// claimLimiterKeys returns the failure-budget keys for this request.
func (s *Server) claimLimiterKeys(c *echo.Context) (perOrigin string, global string) {
	st, _ := site.FromContext(c.Request().Context())
	global = st.CacheKey("rl", "setup.claim", "all")
	if p := clientIPPrefix(c.Request()); p != nil {
		return st.CacheKey("rl", "setup.claim", *p), global
	}
	return "", global
}

// allowClaimRequest applies the hard ceiling to EVERY request to the setup
// routes. It is deliberately far above operator-plausible use: its job is to
// keep a flood of one-row SELECTs from exhausting the connection pool, not to
// police credentials. Unlike the failure budget, this one can refuse a request
// carrying a valid token — a fact stated rather than hidden.
func (s *Server) allowClaimRequest(c *echo.Context) bool {
	if s.deps.Limiter == nil {
		return true
	}
	st, _ := site.FromContext(c.Request().Context())
	ok, _ := s.deps.Limiter.Allow(c.Request().Context(),
		st.CacheKey("rl", "setup.claim", "ceiling"), claimHardCeiling, claimRateWindow)
	return ok
}

// consumeClaimFailure charges one unit of failure budget and reports whether the
// caller has now exceeded it. Only rejected attempts reach here.
//
// The limiter fails OPEN to a per-process counter when the cache is down
// (ADR-003), and that is right here: the endpoint can succeed exactly once in
// the lifetime of the instance, enforced by users_one_owner, so a limiter outage
// costs request volume and never a second owner. Failing closed would turn a
// cache outage into "the operator cannot claim their new instance".
func (s *Server) consumeClaimFailure(c *echo.Context) (nowLimited bool) {
	if s.deps.Limiter == nil {
		return false
	}
	ctx := c.Request().Context()
	perOrigin, global := s.claimLimiterKeys(c)

	limited := false
	if perOrigin != "" {
		if ok, _ := s.deps.Limiter.Allow(ctx, perOrigin, claimFailuresPerOrigin, claimRateWindow); !ok {
			limited = true
		}
	}
	if ok, _ := s.deps.Limiter.Allow(ctx, global, claimFailuresGlobal, claimRateWindow); !ok {
		limited = true
	}
	return limited
}

// recordClaimRefusal writes at most one audit row per refusal, and NONE for a
// rate-limited request.
//
// A 429 is by construction the answer to requests that exceed the limiter — that
// is, requests with no upper bound. Writing an audit row for each would hand an
// unauthenticated attacker an unbounded append path into a table that, after
// migration 0005, nothing in the product can delete: the limiter would cause the
// write instead of bounding it. Rate-limit rejections are a metric; exactly one
// `rate_limited` row is written on the TRANSITION into the limited state, so the
// trail records that a flood happened without recording each packet of it.
func (s *Server) recordClaimRefusal(c *echo.Context, reason string) {
	ctx := c.Request().Context()
	pool, err := s.poolFor(c)
	if err != nil {
		return
	}
	req := c.Request()
	ev := audit.Event{
		ActorKind:     audit.ActorAnonymous,
		Action:        audit.ActionOwnerClaimRefused,
		SubjectType:   audit.SubjectOwnerClaimToken,
		After:         map[string]any{"reason": reason},
		CorrelationID: nonEmptyString(requestIDOf(c)),
		IPPrefix:      clientIPPrefix(req),
	}
	if err := audit.Emit(ctx, sqlcgen.New(pool), ev); err != nil && !errors.Is(err, ctxCanceled) {
		// A refusal that cannot be audited is logged and dropped: it must not
		// turn a 403 into a 500 for the caller, and there is no business
		// transaction here to protect.
		s.deps.Logger.Warn("http: could not record a claim refusal", "request_id", requestIDOf(c))
	}
}

// claimLimitTransition reports true at most once per window, by spending a
// one-unit budget of its own. That is what turns "audit the rate limiting" from
// an unbounded write into a single row per bucket per window.
func (s *Server) claimLimitTransition(c *echo.Context) bool {
	if s.deps.Limiter == nil {
		return false
	}
	st, _ := site.FromContext(c.Request().Context())
	ok, _ := s.deps.Limiter.Allow(c.Request().Context(),
		st.CacheKey("rl", "setup.claim", "audited"), 1, claimRateWindow)
	return ok
}

// recordClaimRateLimited writes the single transition row.
func (s *Server) recordClaimRateLimited(c *echo.Context, bucket string) {
	pool, err := s.poolFor(c)
	if err != nil {
		return
	}
	_ = audit.Emit(c.Request().Context(), sqlcgen.New(pool), audit.Event{
		ActorKind:     audit.ActorAnonymous,
		Action:        audit.ActionOwnerClaimRateLimited,
		SubjectType:   audit.SubjectOwnerClaimToken,
		After:         map[string]any{"bucket": bucket},
		CorrelationID: nonEmptyString(requestIDOf(c)),
		IPPrefix:      clientIPPrefix(c.Request()),
	})
}

func nonEmptyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
