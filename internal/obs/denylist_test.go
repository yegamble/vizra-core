package obs

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestDenylistEveryKeyClass drives the REAL logger the binaries build
// (NewLogger, production JSON) and asserts every VZ-OPS-005 key class is
// redacted at the top level, inside a group, and under WithGroup, and that
// every output line is exactly one JSON object with the documented fields.
func TestDenylistEveryKeyClass(t *testing.T) {
	const v = "SENSITIVE-VALUE-42"
	keys := []string{
		"password", "token", "secret", "authorization", "cookie", "set-cookie",
		"session", "claim_token", "x-amz-signature", "signature", "signed_url",
		"dsn", "database_url", "api_key", "Authorization", "Set-Cookie",
	}
	for _, k := range keys {
		var buf bytes.Buffer
		l := NewLogger(&buf, true)
		l.Info("top", k, v)
		l.Info("nested", slog.Group("req", slog.String(k, v)))
		l.WithGroup("g").Info("withgroup", k, v)
		l.With(k, v).Info("with")
		assertJSONLines(t, buf.String(), 4)
		if strings.Contains(buf.String(), v) {
			t.Errorf("key %q leaked: %s", k, buf.String())
		}
	}
}

func TestDenylistErrorStrings(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, true)
	l.Error("db", "error", errors.New("dial postgres://vizra:pgpass9@db:5432/x failed"))
	l.Error("s3", "error", "GET https://b.s3.amazonaws.com/o?X-Amz-Signature=sigval9&X-Amz-Credential=credval9 403")
	l.Error("gcs", "err", "https://x/o?Signature=gsig9&token=tokval9")
	assertJSONLines(t, buf.String(), 3)
	for _, s := range []string{"pgpass9", "sigval9", "credval9", "gsig9", "tokval9"} {
		if strings.Contains(buf.String(), s) {
			t.Errorf("%q leaked: %s", s, buf.String())
		}
	}
}

func assertJSONLines(t *testing.T, out string, want int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != want {
		t.Fatalf("want %d lines, got %d: %q", want, len(lines), out)
	}
	for _, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line is not one JSON object: %q: %v", ln, err)
		}
		for _, f := range []string{"time", "level", "msg"} {
			if _, ok := m[f]; !ok {
				t.Fatalf("line lacks %q: %q", f, ln)
			}
		}
	}
}
