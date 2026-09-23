package obs_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/obs"
)

// The values below are BUILT, not written as literals: a random-looking string
// in a source file is indistinguishable from a leaked credential to a scanner,
// and these tests need neither randomness nor secrecy.
var (
	fakeToken  = "ey" + strings.Repeat("Jh", 9) + "ZyI6MQ"
	fakeAPIKey = "vzk_" + strings.Repeat("Nn4Pp7", 5)
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
			log:    func(l *slog.Logger) { l.Info("upstream", "detail", "Authorization: Bearer "+fakeToken) },
			secret: fakeToken,
		},
		{
			name:   "Vizra API key",
			log:    func(l *slog.Logger) { l.Info("request", "detail", "key "+fakeAPIKey) },
			secret: fakeAPIKey,
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

// fakeSecret builds a distinct credential-shaped value per form without writing
// one as a literal (see the note on fakeToken).
func fakeSecret(tag string) string { return "Zq" + tag + strings.Repeat("x9", 6) }

// credentialForms is every form obs.Redact removes, one row each, as the text
// of an error or a subprocess line would carry it. The B3 verifier (core #12,
// V-1) measured the rows marked "B3" leaking through the shipped 500 handler
// before this table existed.
func credentialForms() []struct{ name, text, secret string } {
	s := fakeSecret
	return []struct{ name, text, secret string }{
		{"URL userinfo", "dial postgres://vizra:" + s("a") + "@db:5432/vizra failed", s("a")},
		{"B3: URL userinfo with an empty username (redis requirepass)", "cache redis://:" + s("b") + "@cache:6379/0 refused", s("b")},
		{"B3: keyword/value DSN password", "connect host=db user=vizra password=" + s("c") + " dbname=vizra", s("c")},
		{"B3: keyword/value DSN password, single-quoted", "connect host=db password='" + s("d") + " with space' dbname=vizra", s("d")},
		{"B3: ?password= query", "open postgres://db/vizra?sslmode=disable&password=" + s("e"), s("e")},
		{"AWS X-Amz-Signature", "GET https://b.s3.example/o?X-Amz-Signature=" + s("f") + "&X-Amz-Expires=900", s("f")},
		{"AWS X-Amz-Credential", "GET https://b.s3.example/o?X-Amz-Credential=" + s("g"), s("g")},
		{"AWS X-Amz-Security-Token", "GET https://b.s3.example/o?X-Amz-Security-Token=" + s("h"), s("h")},
		{"B3: GCS X-Goog-Signature", "GET https://storage.googleapis.example/b/o?X-Goog-Signature=" + s("i") + "&X-Goog-Expires=900", s("i")},
		{"B3: GCS X-Goog-Credential", "GET https://storage.googleapis.example/b/o?X-Goog-Credential=" + s("j"), s("j")},
		{"Azure sig=", "GET https://acct.blob.example/c/o?sv=2024&sig=" + s("k"), s("k")},
		{"B3: ?api_key=", "GET https://api.example/v1?api_key=" + s("l"), s("l")},
		{"B3: &key=", "GET https://maps.example/v1?q=x&key=" + s("m"), s("m")},
		{"B3: ?access_token=", "GET https://graph.example/me?access_token=" + s("n"), s("n")},
		{"B3: ?claim_token=", "POST /api/v1/setup/claim-owner?claim_token=" + s("o"), s("o")},
		{"B3: Cookie header", "request carried Cookie: vizra_session=" + s("p") + "; theme=dark", s("p")},
		{"B3: Set-Cookie header", "upstream sent Set-Cookie: vizra_session=" + s("q") + "; Path=/; HttpOnly", s("q")},
		// A cookie NAME is not a reliable signal (the session cookie's name is
		// M1-B's to choose), so the header rows below carry names that match no
		// credential key: only the Cookie/Set-Cookie header pattern removes them.
		{"B3: Cookie header, whatever the cookie is called", "request carried Cookie: sid=" + s("v") + "; lang=en", s("v")},
		{"B3: Set-Cookie header, whatever the cookie is called", "upstream sent Set-Cookie: __Host-vz=" + s("w") + "; Secure; Path=/", s("w")},
		{"B3: a session cookie value outside a header", "stale vizra_session=" + s("r") + " rejected", s("r")},
		{"Bearer token", "Authorization: Bearer " + s("s"), s("s")},
		{"Vizra API key", "key vzk_" + s("t"), "vzk_" + s("t")},
		{"a secret in JSON-embedded query text", `{"url":"https://h.example/x?token=` + s("u") + `"}`, s("u")},
	}
}

// One row per form, driven through Redact directly — the function every call
// site in internal/httpapi and internal/search uses.
func TestRedactCoversEveryCredentialForm(t *testing.T) {
	for _, tc := range credentialForms() {
		t.Run(tc.name, func(t *testing.T) {
			got := obs.Redact(tc.text)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("Redact left the secret in:\n  in:  %s\n  out: %s", tc.text, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("nothing was marked redacted, so the value may simply be absent:\n%s", got)
			}
		})
	}
}

// The false-positive control: text that merely LOOKS like a key, or names a
// credential without carrying one, must come out byte-for-byte unchanged.
// A redactor that mangles "keyboard" or "monkey=" destroys the log line it is
// supposed to make safe to keep.
func TestRedactLeavesLookalikesAlone(t *testing.T) {
	for _, text := range []string{
		"keyboard layout us; monkey=banana hotkey=F5 turkey=roast",
		`failed SASL auth (FATAL: password authentication failed for user "vizra" (SQLSTATE 28P01))`,
		"token bucket refilled; session store warm; api key rotation scheduled",
		"postgres://db:5432/vizra?sslmode=disable",
		"https://social.example:443/@alice/posts",
		"http: request failed path=/api/v1/setup/claim-status method=GET status=500",
		"cookie jar empty; no cookies were sent",
	} {
		if got := obs.Redact(text); got != text {
			t.Errorf("Redact changed text that carries no secret:\n  in:  %s\n  out: %s", text, got)
		}
	}
}
