package obs_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/obs"
)

// VZ-OPS-005 requires the redaction ASSERTED, not documented: "a log call
// carrying each of those value classes is shown to emit the redacted form".
// Each case below is a real way a credential has escaped into a log file.
func TestRedactionOfEveryValueClass(t *testing.T) {
	cases := []struct {
		name   string
		log    func(*slog.Logger)
		secret string
	}{
		{
			name:   "DSN password in a message",
			log:    func(l *slog.Logger) { l.Info("connecting to postgres://vizra:hunter2@db:5432/vizra") },
			secret: "hunter2",
		},
		{
			name:   "cache URL password in an attribute",
			log:    func(l *slog.Logger) { l.Info("cache", "url", "rediss://default:s3cr3tpw@cache:6379/0") },
			secret: "s3cr3tpw",
		},
		{
			name: "presigned URL in a message",
			log: func(l *slog.Logger) {
				l.Info("serving https://bucket.s3.example/obj?X-Amz-Signature=abc123def456&X-Amz-Expires=900")
			},
			secret: "abc123def456",
		},
		{
			name:   "bearer token in an attribute",
			log:    func(l *slog.Logger) { l.Info("upstream", "detail", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9") },
			secret: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name:   "Vizra API key",
			log:    func(l *slog.Logger) { l.Info("request", "detail", "key vzk_LiveKeyMaterial0123456789") },
			secret: "vzk_LiveKeyMaterial0123456789",
		},
		{
			name:   "a secret-named attribute, whatever its value",
			log:    func(l *slog.Logger) { l.Info("boot", "session_id", "plain-looking-value-9f3a") },
			secret: "plain-looking-value-9f3a",
		},
		{
			name:   "a secret attribute attached with With",
			log:    func(l *slog.Logger) { l.With("password", "correcthorse").Info("boot") },
			secret: "correcthorse",
		},
	}

	for _, tc := range cases {
		for _, production := range []bool{false, true} {
			name := tc.name
			if production {
				name += " (production JSON)"
			}
			t.Run(name, func(t *testing.T) {
				var buf bytes.Buffer
				tc.log(obs.NewLogger(&buf, production))
				out := buf.String()
				if out == "" {
					t.Fatal("nothing was logged")
				}
				if strings.Contains(out, tc.secret) {
					t.Fatalf("the log line leaked %q:\n%s", tc.secret, out)
				}
				if !strings.Contains(out, "redacted") {
					t.Fatalf("nothing was redacted, so the value may simply be absent:\n%s", out)
				}
			})
		}
	}
}

// Redact is exported so every subprocess stderr capture passes through it. A
// tool that echoes a presigned URL must not be able to put it in a log line.
func TestRedactIsUsableOnSubprocessOutput(t *testing.T) {
	stderr := "vips: failed fetching https://b.example/o?X-Amz-Signature=deadbeefcafe\n" +
		"retrying with postgres://u:p4ssw0rd@db/vizra\n"
	got := obs.Redact(stderr)
	for _, secret := range []string{"deadbeefcafe", "p4ssw0rd"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact left %q in:\n%s", secret, got)
		}
	}
}

// Redaction must not destroy the diagnostic value of a log line.
func TestRedactionKeepsTheUsefulParts(t *testing.T) {
	var buf bytes.Buffer
	obs.NewLogger(&buf, false).Info("cache unreachable", "url", "rediss://default:pw@cache:6379/0", "attempt", 3)
	out := buf.String()
	for _, want := range []string{"cache unreachable", "cache:6379", "attempt=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("the redacted line lost %q:\n%s", want, out)
		}
	}
}
