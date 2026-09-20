// Package obs holds cross-cutting observability. The redaction layer here is
// the VZ-OPS-005 rule made mechanical: "no process ever logs credentials,
// signed URLs, session ids, API keys or raw private metadata", enforced in the
// logger so a call site that passes a secret cannot put it in a log line.
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

// valuePatterns match a secret embedded in a larger string, which is how a
// credential usually escapes: inside a URL, or inside a tool's stderr.
var valuePatterns = []*regexp.Regexp{
	// userinfo in any URL: scheme://user:password@host
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`),
	// presigned-URL query parameters
	regexp.MustCompile(`(?i)([?&](?:X-Amz-Signature|X-Amz-Credential|X-Amz-Security-Token|Signature|AWSAccessKeyId|token|sig)=)[^&\s"']+`),
	// bearer tokens and Vizra API keys
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bvzk_[A-Za-z0-9._~+/=-]{8,}`),
}

// Redact scrubs a free-text string. It is exported because every subprocess
// stderr capture must pass through it too: a tool that echoes a presigned URL
// must not be able to put it in a log line (ADR-002 § Logging and redaction).
func Redact(s string) string {
	for i, re := range valuePatterns {
		switch i {
		case 0:
			s = re.ReplaceAllString(s, "${1}"+redacted+"@")
		case 1:
			s = re.ReplaceAllString(s, "${1}"+redacted)
		default:
			s = re.ReplaceAllString(s, redacted)
		}
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
