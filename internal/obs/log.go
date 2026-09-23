// Package obs holds cross-cutting observability, including the redaction that
// VZ-OPS-005 asks for ("no process ever logs credentials, signed URLs, session
// ids, API keys or raw private metadata"). What is implemented here is narrower
// than that sentence, and this comment says so rather than repeat it:
//
//   - Redact removes the value forms valuePatterns lists — URL userinfo, the
//     Cookie/Set-Cookie headers, credential-named key=value pairs (query
//     parameters, presigned-URL parameters, keyword/value DSNs, session cookie
//     pairs), Bearer tokens and vzk_ API keys — and nothing else. There is NO
//     pattern for raw private metadata.
//   - The handler NewLogger builds replaces the whole value of an attribute
//     whose KEY is in secretKeys, and runs Redact over every other string value
//     and over the message. It protects only what is logged through it: a
//     process that logs through another handler (slog.Default() included) gets
//     no redaction unless the call site applies Redact itself, which is why the
//     worker and internal/httpapi do exactly that and test it.
package obs

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// redacted replaces any value a rule matches.
const redacted = "[redacted]"

// secretKeys are attribute keys whose VALUE is always replaced, whatever it is.
var secretKeys = map[string]bool{
	"password": true, "passwd": true, "secret": true, "token": true,
	"authorization": true, "cookie": true, "set-cookie": true,
	"api_key": true, "apikey": true, "session": true, "session_id": true,
	"dsn": true, "database_url": true, "cache_url": true, "hmac_key": true,
	"kek": true, "mfa_key_kek": true, "signature": true, "private_key": true,
}

// redaction is one pattern and what replaces its match.
type redaction struct {
	re   *regexp.Regexp
	repl string
}

// valuePatterns match a secret embedded in a larger string, which is how a
// credential usually escapes: inside a URL, a connection string, a header, or a
// tool's stderr. This list IS the guarantee Redact gives; a form not matched here
// is not removed. TestRedactCoversEveryCredentialForm has one row per form, and
// TestRedactLeavesLookalikesAlone is the false-positive control.
var valuePatterns = []redaction{
	// URL userinfo: scheme://user:password@ — the username may be EMPTY, which
	// is the Redis requirepass form (redis://:pw@host) that redis.ParseURL
	// accepts. A password containing a raw "/" or "@" (not percent-encoded) is
	// only partly matched: a stated residual.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]*:[^/\s@]+@`), "${1}" + redacted + "@"},
	// Cookie and Set-Cookie header values, to the end of the line.
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie):[ \t]*[^\r\n"]+`), "${1}: " + redacted},
	// key=value pairs whose KEY names a credential, wherever they appear: URL
	// query parameters (?password=, ?api_key=, &key=, ?access_token=,
	// ?claim_token=, presigned X-Amz-*/X-Goog-*/sig= parameters), keyword/value
	// connection strings (password=… or password='…'), and cookie pairs
	// (vizra_session=…). The key must start at a non-word boundary, so
	// "monkey=" and "hotkey=" are left alone; a key merely CONTAINING password,
	// secret, token, session, credential or api_key/apikey is matched, so
	// "db_password=" and "csrf_token=" are too. The value runs to "&",
	// whitespace, a quote or "<>"; a quoted value is taken whole.
	{regexp.MustCompile(`(?i)(^|[^a-z0-9_-])((?:[a-z0-9_-]*(?:password|passwd|secret|token|session|credential|api[_-]?key)[a-z0-9_-]*|key|sig|signature|awsaccesskeyid|x-(?:amz|goog)-signature)=)('[^']*'|"[^"]*"|[^&\s"'<>]+)`), "${1}${2}" + redacted},
	// bearer tokens and Vizra API keys
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`), redacted},
	{regexp.MustCompile(`\bvzk_[A-Za-z0-9._~+/=-]{8,}`), redacted},
}

// Redact scrubs a free-text string of the forms valuePatterns lists, and only
// those. It is exported because every subprocess stderr capture, and every log
// call site that passes error or request text, must pass through it: a tool
// that echoes a presigned URL must not be able to put it in a log line
// (ADR-002 § Logging and redaction).
//
// What it does NOT remove, stated so nobody reads more into it: raw private
// metadata (there is no pattern for EXIF, captions or e-mail addresses), a
// secret under a JSON key ("password":"…"), a URL password containing a raw
// "/" or "@", a credential-shaped value with no recognisable key or prefix, and
// an API key that is not a vzk_ key.
func Redact(s string) string {
	for _, p := range valuePatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// redactingHandler wraps a slog.Handler.
type redactingHandler struct{ inner slog.Handler }

func (h redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = redactAttr(a)
	}
	return redactingHandler{inner: h.inner.WithAttrs(scrubbed)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if secretKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]any, 0, len(attrs))
		for _, g := range attrs {
			out = append(out, redactAttr(g))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if u, ok := a.Value.Any().(*url.URL); ok && u != nil {
			c := *u
			if c.User != nil {
				c.User = url.User(redacted)
			}
			return slog.String(a.Key, c.String())
		}
		return slog.String(a.Key, Redact(a.Value.String()))
	}
	return a
}

// NewLogger builds the process logger. Production emits JSON; development emits
// text. Both go through the redaction layer — a development logger that leaks
// is a development log file that leaks.
func NewLogger(w io.Writer, production bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var inner slog.Handler
	if production {
		inner = slog.NewJSONHandler(w, opts)
	} else {
		inner = slog.NewTextHandler(w, opts)
	}
	return slog.New(redactingHandler{inner: inner})
}
