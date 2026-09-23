package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/yegamble/vizra-core/internal/obs"
	"github.com/yegamble/vizra-core/internal/site"
)

// headerRequestID is echoed back so an operator can correlate a user's report
// with a log line. It is a random id and never contains anything about the user.
const headerRequestID = "X-Request-Id"

func requestIDMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			id := c.Request().Header.Get(headerRequestID)
			// A client-supplied id is not trusted into logs unmodified: it is
			// attacker-controlled text. Accept it only when it is already a UUID.
			if _, err := uuid.Parse(id); err != nil {
				v, gerr := uuid.NewV7()
				if gerr != nil {
					id = ""
				} else {
					id = v.String()
				}
			}
			c.Response().Header().Set(headerRequestID, id)
			c.Set(string(headerRequestID), id)
			return next(c)
		}
	}
}

// siteMiddleware performs Q-008 checklist item (2): hostname -> site resolution
// in EXACTLY ONE middleware. Everything downstream reads the site from the
// request context, so no handler ever parses a Host header itself and tenancy
// stays a connection-routing concern.
func siteMiddleware(r *site.Resolver, log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			s, err := r.ByHost(req.Host)
			if err != nil {
				// Under tenancy this is a 404. In core the resolver falls back to
				// the single site, so reaching here means a genuinely broken
				// configuration.
				log.Warn("http: no site for host")
				return echo.NewHTTPError(http.StatusNotFound, "unknown site")
			}
			c.SetRequest(req.WithContext(site.NewContext(req.Context(), s)))
			return next(c)
		}
	}
}

// routeAttributeMiddleware is the in-house middleware ADR-001 specifies to sit
// beside otelhttp: it sets `http.route` on the active span from the MATCHED
// route pattern, which otelhttp cannot know on its own.
//
// Without it every span carries the raw path, so `/i/abc123` and `/i/def456`
// become two distinct operations and the trace backend is flooded with
// per-asset cardinality. ADR-001 budgets at most 40 lines for this; it is well
// under.
func routeAttributeMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			// Read the route AFTER the handler: the router fills RouteInfo during
			// matching, so before next() it is empty on Pre middleware and
			// unreliable here.
			if pattern := c.RouteInfo().Path; pattern != "" {
				span := trace.SpanFromContext(c.Request().Context())
				if span.IsRecording() {
					span.SetAttributes(attribute.String("http.route", pattern))
					span.SetName(c.Request().Method + " " + pattern)
				}
			}
			return err
		}
	}
}

// securityHeadersMiddleware sets the response defaults every route gets,
// including the error paths — which is where a per-handler approach always
// misses one.
//
// Cache-Control: no-store is the one that matters on a photo host, and it is a
// VISIBILITY control rather than a header checklist item. The default must be
// no-store so that the public-derivative path opts IN to caching; the reverse —
// caching by default with private routes opting out — is how a private
// derivative ends up in a shared cache. ADR-007 row 21: only public derivatives
// are shared-cacheable, and every key carries visibility_version. A handler that
// serves a public derivative overwrites this header deliberately.
//
// No CSP here: this server returns JSON. CSP and frame-ancestors belong to
// vizra-user's HTML origin, and both setting them would be worse than one.
func securityHeadersMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			h := c.Response().Header()
			// Never let a browser sniff a JSON body into something executable.
			h.Set("X-Content-Type-Options", "nosniff")
			// An API URL can carry an asset's public key; do not send it onward.
			h.Set("Referrer-Policy", "no-referrer")
			// Refuse cross-origin embedding of anything this server returns.
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			// Set before the handler runs, so a handler that legitimately caches
			// (a public derivative, M1) can overwrite it rather than fight it.
			h.Set("Cache-Control", "no-store")
			return next(c)
		}
	}
}

// errorHandler renders every error as the Error schema of api/openapi.yaml and
// never leaks an internal message. A 500's cause goes to the log with the
// request id; the client gets the id and nothing else.
func errorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		if resp, ok := c.Response().(*echo.Response); ok && resp.Committed {
			// The response is already committed; a second write would corrupt it.
			return
		}
		// Echo v5 signals the wire status through the HTTPStatusCoder
		// interface, not through one concrete error type: echo.ErrNotFound and
		// echo.NewHTTPError are different types that both answer StatusCode().
		// Type-asserting one of them would silently render every router 404 as
		// a 500, which is what TestUnknownRouteIsAJSONError guards.
		status := echo.StatusCode(err)
		if status == 0 {
			status = http.StatusInternalServerError
		}
		code := httpCodeName(status)
		message := "an internal error occurred"
		// A codedError carries an APPLICATION code distinct from the one the
		// status implies, so "this 403 is a misconfigured public origin" can be
		// told apart from "this 403 is a rejected claim token" by a client and by
		// an operator reading a support report. The status-derived code stays the
		// default for everything else.
		var ce *codedError
		if errors.As(err, &ce) {
			code = ce.code
			if status < 500 {
				message = ce.message
			}
		} else if status < 500 {
			// 4xx messages are written by us and are safe to return. A 5xx
			// message never is: it can carry an internal detail.
			message = httpCodeName(status)
			var he *echo.HTTPError
			if errors.As(err, &he) && he.Message != "" {
				message = he.Message
			}
		}

		reqID, _ := c.Get(string(headerRequestID)).(string)
		// The request's OWN cancellation — the client hung up and the work was
		// cancelled on its behalf — is not a server failure, and an ERROR line for
		// it is noise on the signal an operator reads during a real outage
		// (sentinel S-0005, RULES R9). Only that is skipped: the cause must be
		// context.Canceled AND the request's context done. A genuine 5xx whose
		// client happened to leave (a reverse proxy timing out on a real defect
		// cancels the request context too) is logged (sentinel PR #14 F-4).
		ownCancellation := c.Request().Context().Err() != nil && errors.Is(err, context.Canceled)
		if status >= 500 && !ownCancellation {
			// Every value is redacted HERE, not left to the handler: Deps.Logger
			// falls back to slog.Default(), which redacts nothing unless the
			// process installed obs.NewLogger. An unmapped error is exactly where
			// a driver error quoting a DSN, or a storage error quoting a presigned
			// URL, arrives (security review of core #8, N-7 / F-1), and the path
			// is attacker-chosen text. TestEveryLogSiteInTheAPIIsRedacted counts
			// this; TestTheErrorHandlerRedactsTheCauseOfA500 drives it.
			log.Error("http: request failed", "error", obs.Redact(err.Error()), "request_id", obs.Redact(reqID),
				"path", obs.Redact(c.Request().URL.Path), "method", obs.Redact(c.Request().Method))
		}
		_ = c.JSON(status, errorBody{Error: errorDetail{Code: code, Message: message, RequestID: reqID}})
	}
}

func httpCodeName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "internal_error"
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// atomicBool is a tiny wrapper so the drain flag reads the same in every file.
type atomicBool struct{ v atomic.Bool }

func (a *atomicBool) Store(b bool) { a.v.Store(b) }
func (a *atomicBool) Load() bool   { return a.v.Load() }
