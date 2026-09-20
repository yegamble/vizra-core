package search

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yegamble/vizra-core/internal/authz"
)

// Built, not a literal, for the same reason as elsewhere: a 32-character
// random-looking string in a source file reads as a leaked credential. The HMAC
// scheme needs a key of at least 32 bytes and nothing else.
var testKey = []byte(strings.Repeat("Aa1Bb2Cc3Dd4", 3)[:32])

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// HMAC scheme
// ---------------------------------------------------------------------------

type vector struct {
	Name          string            `json:"name"`
	RejectBecause string            `json:"reject_because"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Timestamp     string            `json:"timestamp"`
	Nonce         string            `json:"nonce"`
	BodyUTF8      string            `json:"body_utf8"`
	BodyHash      string            `json:"body_sha256_hex"`
	Canonical     string            `json:"canonical_string"`
	Signature     string            `json:"signature"`
	MustReject    bool              `json:"must_reject"`
	ExtraHeaders  map[string]string `json:"extra_headers"`
}

type vectorFile struct {
	Scheme          string   `json:"scheme"`
	KeyUTF8         string   `json:"key_utf8"`
	VerifierNowUnix int64    `json:"verifier_now_unix"`
	Vectors         []vector `json:"vectors"`
	NegativeVectors []vector `json:"negative_vectors"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("../../api/search-hmac-testvectors.json")
	if err != nil {
		t.Fatalf("the shared HMAC test vectors are missing: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	return vf
}

// The vectors are a committed artifact in api/, shared with vizra-search. If
// this test and that file disagree, the two repositories would sign different
// bytes and every internal call would 401 in production.
func TestHMACTestVectors(t *testing.T) {
	vf := loadVectors(t)
	if vf.Scheme != SignatureVersion {
		t.Fatalf("vector file scheme = %q, code implements %q", vf.Scheme, SignatureVersion)
	}
	if len(vf.Vectors) == 0 {
		t.Fatal("the vector file has no vectors")
	}
	key := []byte(vf.KeyUTF8)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			body := []byte(v.BodyUTF8)
			if got := BodyHash(body); got != v.BodyHash {
				t.Fatalf("body hash = %s, vector says %s", got, v.BodyHash)
			}
			if got := CanonicalString(v.Method, v.Path, v.Timestamp, v.Nonce, v.BodyHash); got != v.Canonical {
				t.Fatalf("canonical string mismatch\n got: %q\nwant: %q", got, v.Canonical)
			}
			got, err := Sign(key, v.Method, v.Path, v.Timestamp, v.Nonce, body)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if got != v.Signature {
				t.Fatalf("signature = %s, vector says %s", got, v.Signature)
			}
		})
	}
}

func TestEmptyBodyHashesToSHA256OfEmptyString(t *testing.T) {
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := BodyHash(nil); got != want {
		t.Fatalf("BodyHash(nil) = %s, want %s", got, want)
	}
	if got := BodyHash([]byte{}); got != want {
		t.Fatalf("BodyHash([]) = %s, want %s", got, want)
	}
}

func signedHeader(t *testing.T, method, path string, body []byte, at time.Time) http.Header {
	t.Helper()
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Sign(testKey, method, path, ts, nonce, body)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, sig)
	return h
}

func TestVerifyRoundTrip(t *testing.T) {
	now := time.Now()
	body := []byte(`{"query":"sunset"}`)
	h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now)
	if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != nil {
		t.Fatalf("a signature we just produced did not verify: %v", err)
	}
}

// Each of these is a way a signature must fail. A verifier that passes any of
// them is not authenticating anything.
func TestVerifyRejects(t *testing.T) {
	now := time.Now()
	body := []byte(`{"query":"sunset"}`)
	base := func() http.Header { return signedHeader(t, http.MethodPost, "/internal/v1/search", body, now) }

	t.Run("tampered body", func(t *testing.T) {
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", base(), []byte(`{"query":"sunrise"}`), now); err == nil {
			t.Fatal("a changed body verified")
		}
	})
	t.Run("different path", func(t *testing.T) {
		if err := Verify(testKey, http.MethodPost, "/internal/v1/events", base(), body, now); err == nil {
			t.Fatal("a replay onto another path verified")
		}
	})
	t.Run("different method", func(t *testing.T) {
		if err := Verify(testKey, http.MethodGet, "/internal/v1/search", base(), body, now); err == nil {
			t.Fatal("a replay with another method verified")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		other := []byte(strings.Repeat("Zz9Yy8Xx7Ww6", 3)[:32])
		if err := Verify(other, http.MethodPost, "/internal/v1/search", base(), body, now); err == nil {
			t.Fatal("the wrong key verified")
		}
	})
	t.Run("stale timestamp", func(t *testing.T) {
		h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now.Add(-MaxClockSkew-time.Second))
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != ErrTimestampSkew {
			t.Fatalf("stale timestamp: err = %v, want %v", err, ErrTimestampSkew)
		}
	})
	t.Run("future timestamp", func(t *testing.T) {
		h := signedHeader(t, http.MethodPost, "/internal/v1/search", body, now.Add(MaxClockSkew+time.Second))
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err != ErrTimestampSkew {
			t.Fatalf("future timestamp: err = %v, want %v", err, ErrTimestampSkew)
		}
	})
	t.Run("missing headers", func(t *testing.T) {
		for _, drop := range []string{HeaderTimestamp, HeaderNonce, HeaderSignature} {
			h := base()
			h.Del(drop)
			if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err == nil {
				t.Fatalf("verified with %s missing", drop)
			}
		}
	})
	t.Run("short nonce", func(t *testing.T) {
		h := base()
		h.Set(HeaderNonce, "abcd")
		if err := Verify(testKey, http.MethodPost, "/internal/v1/search", h, body, now); err == nil {
			t.Fatal("a 2-byte nonce verified")
		}
	})
	t.Run("short key", func(t *testing.T) {
		if err := Verify([]byte("tooshort"), http.MethodPost, "/internal/v1/search", base(), body, now); err != ErrKeyTooShort {
			t.Fatalf("err = %v, want %v", err, ErrKeyTooShort)
		}
	})
}

// ---------------------------------------------------------------------------
// The fallback semantics Q-001 forbids collapsing
// ---------------------------------------------------------------------------

// fakeSearch is a vizra-search that verifies signatures and answers however the
// test tells it to.
func fakeSearch(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := Verify(testKey, r.Method, r.URL.EscapedPath(), r.Header, raw, time.Now()); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"signature_rejected","message":"rejected"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func newService(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	var remote *RemoteClient
	if srv != nil {
		remote = NewRemote(srv.URL, testKey, 2*time.Second)
	}
	return NewService(NewSQL(), remote, quietLogger())
}

func TestSearchOffUsesSQLAndReportsOff(t *testing.T) {
	s := newService(t, nil)
	res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || len(res.Results) != 0 {
		t.Fatalf("SQL path returned %+v; want an ok status with no results", res)
	}
	if got := s.Health(context.Background()); got != HealthOff {
		t.Fatalf("health = %s, want off", got)
	}
}

// not_indexed is a DECISION, not a fault: fall back to SQL and stay healthy.
func TestNotIndexedFallsBackToSQLAndStaysHealthy(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"not_indexed","results":[],"total":0}`)
	defer srv.Close()
	s := newService(t, srv)

	res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("not_indexed must not surface as an error: %v", err)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %s, want the SQL path's ok", res.Status)
	}
	if got := s.Health(context.Background()); got != HealthOK {
		t.Fatalf("health = %s after not_indexed, want ok: not_indexed is not a fault", got)
	}
}

// A fault is NEVER silent: SQL still answers, but health goes degraded.
func TestRemoteFaultFallsBackToSQLAndDegradesHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"500", http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"boom"}}`},
		{"503", http.StatusServiceUnavailable, `{"error":{"code":"unavailable","message":"db down"}}`},
		{"unparseable 200", http.StatusOK, `this is not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeSearch(t, tc.status, tc.body)
			defer srv.Close()
			s := newService(t, srv)

			res, err := s.Search(context.Background(), SearchRequest{Query: "x"})
			if err != nil {
				t.Fatalf("reads must still be served from SQL during a fault: %v", err)
			}
			if res.Status != StatusOK {
				t.Fatalf("status = %s, want the SQL path's ok", res.Status)
			}
			if got := s.Health(context.Background()); got != HealthDegraded {
				t.Fatalf("health = %s after a %s fault, want degraded: Q-001 forbids a silent fallback", got, tc.name)
			}
		})
	}
}

func TestUnreachableSearchDegradesHealth(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":0}`)
	srv.Close() // now refusing connections
	s := newService(t, srv)

	if _, err := s.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatalf("an unreachable search must still be answered from SQL: %v", err)
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s, want degraded", got)
	}
}

// Health must recover without a restart once the service comes back.
func TestHealthRecoversAfterAFault(t *testing.T) {
	mode := "fault"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "fault" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[],"total":0}`))
	}))
	defer srv.Close()
	s := newService(t, srv)

	_, _ = s.Search(context.Background(), SearchRequest{Query: "x"})
	if s.Health(context.Background()) != HealthDegraded {
		t.Fatal("expected degraded after the fault")
	}
	mode = "ok"
	_, _ = s.Search(context.Background(), SearchRequest{Query: "x"})
	if got := s.Health(context.Background()); got != HealthOK {
		t.Fatalf("health = %s after recovery, want ok without a restart", got)
	}
}

// Publish is driven by a durable job, so a fault must be an error the job can
// retry — not a swallowed side-write.
func TestPublishFaultIsAnErrorSoTheJobRetries(t *testing.T) {
	srv := fakeSearch(t, http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"boom"}}`)
	defer srv.Close()
	s := newService(t, srv)

	_, err := s.Publish(context.Background(), EventBatch{Events: []Event{{EventID: "e1"}}})
	if err == nil {
		t.Fatal("a failed publish was swallowed; the durable job would mark itself succeeded and the event would be lost")
	}
}

func TestPublishNotIndexedSucceedsSoTheJobDoesNotRetryForever(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"not_indexed","accepted":0,"duplicates":0}`)
	defer srv.Close()
	s := newService(t, srv)

	res, err := s.Publish(context.Background(), EventBatch{Events: []Event{{EventID: "e1"}}})
	if err != nil {
		t.Fatalf("not_indexed must not fail the job: %v", err)
	}
	if res.Status != StatusNotIndexed {
		t.Fatalf("status = %s, want not_indexed", res.Status)
	}
}

// The client signs correctly against a verifier that implements the contract.
func TestRemoteRequestsAreSignedAcceptably(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":3}`)
	defer srv.Close()
	c := NewRemote(srv.URL, testKey, 2*time.Second)

	res, err := c.Search(context.Background(), SearchRequest{Query: "sunset", Viewer: Viewer{IsAnonymous: true, Role: "anonymous"}})
	if err != nil {
		t.Fatalf("a correctly signed request was rejected: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
}

// A wrong key is a fault, not a not_indexed: a misconfigured shared secret must
// be visible in readiness, not look like an empty index.
func TestWrongKeyIsAFaultNotAFallback(t *testing.T) {
	srv := fakeSearch(t, http.StatusOK, `{"status":"ok","results":[],"total":1}`)
	defer srv.Close()
	bad := NewRemote(srv.URL, []byte(strings.Repeat("Zz9Yy8Xx7Ww6", 3)[:32]), 2*time.Second)
	s := NewService(NewSQL(), bad, quietLogger())

	if _, err := s.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatalf("reads must still be answered: %v", err)
	}
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s with a wrong HMAC key, want degraded", got)
	}
}

// A transport error must not carry the search URL into a log line: a
// misconfigured VIZRA_SEARCH_URL can hold userinfo.
func TestTransportErrorDoesNotLeakCredentials(t *testing.T) {
	c := NewRemote("http://user:sup3rsecret@127.0.0.1:1/", testKey, 200*time.Millisecond)
	_, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "sup3rsecret") {
		t.Fatalf("the transport error leaked a credential: %q", err.Error())
	}
}

func TestViewerFromSubjectMarksAnonymous(t *testing.T) {
	v := ViewerFrom(authz.Subject{Role: authz.RoleAnonymous}, authz.ScopeSite)
	if !v.IsAnonymous || v.UserID != "" || v.Role != authz.RoleAnonymous {
		t.Fatalf("anonymous viewer projected as %+v", v)
	}
	// A subject with an id is never projected as anonymous: vizra-search trusts
	// this field for its projection.
	v = ViewerFrom(authz.Subject{UserID: "u1", Role: authz.RoleMember}, authz.ScopeOwnLibrary)
	if v.IsAnonymous || v.Scope != authz.ScopeOwnLibrary {
		t.Fatalf("member viewer projected as %+v", v)
	}
}

// ---------------------------------------------------------------------------
// Timestamp window — the ONLY replay bound at M0
// ---------------------------------------------------------------------------

// Finding 1 of docs/evidence/warroom/2026-09-20-vizra-search-pr1-minimal-service-SECURITY.md:
// computing the skew by converting the header to a time.Time, subtracting, and
// folding the sign does not behave as the code assumes at the ends of the
// representable range. time.Unix() with a huge seconds value wraps, and negating
// a Duration of math.MinInt64 is a no-op — so a validly signed request with an
// out-of-range timestamp never expires.
//
// Until vizra-search owns storage there is no nonce replay store, so this window
// is the whole replay bound. A window that does not close is no bound at all.
func TestTimestampWindowAcrossTheMagnitudeRange(t *testing.T) {
	body := []byte(`{"query":"sunset"}`)
	const path = "/internal/v1/search"
	nowSecs := int64(1789000000) // a fixed "now" so the table is deterministic
	now := time.Unix(nowSecs, 0)
	skew := int64(MaxClockSkew / time.Second)

	cases := []struct {
		name   string
		ts     int64
		accept bool
	}{
		{"exactly now", nowSecs, true},
		{"one second stale", nowSecs - 1, true},
		{"one second ahead", nowSecs + 1, true},
		{"at the stale edge", nowSecs - skew, true},
		{"at the future edge", nowSecs + skew, true},
		{"one second past the stale edge", nowSecs - skew - 1, false},
		{"one second past the future edge", nowSecs + skew + 1, false},
		{"an hour stale", nowSecs - 3600, false},
		{"an hour ahead", nowSecs + 3600, false},
		{"a year stale", nowSecs - 31536000, false},
		{"a year ahead", nowSecs + 31536000, false},

		// The extremes. Each of these is a validly SIGNED request: the attacker
		// controls the timestamp and signs over it, so the signature verifies.
		// Only the window can refuse them.
		{"zero (the epoch)", 0, false},
		{"one", 1, false},
		{"negative one (before the epoch)", -1, false},
		{"math.MinInt64", math.MinInt64, false},
		{"math.MaxInt64", math.MaxInt64, false},
		{"math.MaxInt64 - 1", math.MaxInt64 - 1, false},
		{"math.MinInt64 + 1", math.MinInt64 + 1, false},
		// Seconds values that overflow a time.Duration when subtracted:
		// max Duration is about 292 years of nanoseconds, so anything beyond
		// roughly 9.2e9 seconds from now overflows the derived duration.
		{"just past the Duration overflow, ahead", nowSecs + 9_300_000_000, false},
		{"just past the Duration overflow, stale", nowSecs - 9_300_000_000, false},
		{"far beyond the Duration overflow", nowSecs + 1_000_000_000_000, false},
		{"year 10000", 253402300799, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := strconv.FormatInt(tc.ts, 10)
			nonce, err := NewNonce()
			if err != nil {
				t.Fatal(err)
			}
			sig, err := Sign(testKey, http.MethodPost, path, ts, nonce, body)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			h := http.Header{}
			h.Set(HeaderTimestamp, ts)
			h.Set(HeaderNonce, nonce)
			h.Set(HeaderSignature, sig)

			err = Verify(testKey, http.MethodPost, path, h, body, now)
			if tc.accept && err != nil {
				t.Fatalf("timestamp %s (%+d s from now) was REFUSED: %v", ts, tc.ts-nowSecs, err)
			}
			if !tc.accept && err == nil {
				t.Fatalf("timestamp %s was ACCEPTED with a valid signature. "+
					"The timestamp window is the only replay bound at M0; this request never expires.", ts)
			}
		})
	}
}

// The timestamp header must be bare decimal digits. Anything a parser would
// "helpfully" accept is a canonicalisation divergence: core signs the header
// string verbatim, so a verifier that rebuilds it through ParseInt/FormatInt
// would accept forms core refuses, and the two implementations would disagree
// about which requests are valid.
func TestTimestampHeaderMustBeBareDecimalDigits(t *testing.T) {
	body := []byte(`{}`)
	const path = "/internal/v1/search"
	nowSecs := int64(1789000000)
	now := time.Unix(nowSecs, 0)

	for _, raw := range []string{
		" 1789000000",  // leading space
		"1789000000 ",  // trailing space
		" 1789000000 ", // both
		"+1789000000",  // explicit sign
		"01789000000",  // leading zero
		"0001789000000",
		"1_789_000_000",   // Go-style separators, which ParseInt with base 0 accepts
		"0x6A9B9A80",      // hex
		"1789000000.0",    // decimal point
		"1789000000\n",    // trailing newline
		"1789000000,",     //
		"",                // empty
		"not-a-timestamp", //
	} {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			nonce, err := NewNonce()
			if err != nil {
				t.Fatal(err)
			}
			// Signed over the raw header exactly as sent, so the signature is
			// genuinely valid and only the format rule can refuse it.
			sig, err := Sign(testKey, http.MethodPost, path, raw, nonce, body)
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			h.Set(HeaderTimestamp, raw)
			h.Set(HeaderNonce, nonce)
			h.Set(HeaderSignature, sig)

			if err := Verify(testKey, http.MethodPost, path, h, body, now); err == nil {
				t.Fatalf("timestamp header %q was accepted; it is not bare decimal digits, "+
					"so vizra-search and vizra-core would disagree about whether this request is valid", raw)
			}
		})
	}
}

// The method is signed verbatim and must already be uppercase. Normalising it
// in one implementation and not the other is the same divergence class.
func TestMethodMustAlreadyBeUppercase(t *testing.T) {
	body := []byte(`{}`)
	const path = "/internal/v1/search"
	now := time.Unix(1789000000, 0)

	for _, m := range []string{"post", "Post", "pOsT"} {
		t.Run(m, func(t *testing.T) {
			if _, err := Sign(testKey, m, path, "1789000000", "00112233445566778899aabbccddeeff", body); err == nil {
				t.Fatalf("Sign accepted the method %q; it must be uppercase on the wire", m)
			}
			// And a hand-rolled signature over the lowercase method must not verify.
			ts, nonce := "1789000000", "00112233445566778899aabbccddeeff"
			mac := hmacHex(testKey, CanonicalString(m, path, ts, nonce, BodyHash(body)))
			h := http.Header{}
			h.Set(HeaderTimestamp, ts)
			h.Set(HeaderNonce, nonce)
			h.Set(HeaderSignature, SignatureVersion+"="+mac)
			if err := Verify(testKey, m, path, h, body, now); err == nil {
				t.Fatalf("Verify accepted the method %q", m)
			}
		})
	}
}

func hmacHex(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// The nonce must be lowercase hex. Uppercase would sign differently on the two
// sides if either normalised it.
func TestNonceMustBeLowercaseHex(t *testing.T) {
	body := []byte(`{}`)
	const path = "/internal/v1/search"
	now := time.Unix(1789000000, 0)
	ts := "1789000000"

	for _, nonce := range []string{
		"00112233445566778899AABBCCDDEEFF", // uppercase
		"00112233445566778899aabbccddeegg", // not hex
		"00112233445566778899aabbccddee",   // 15 bytes, one short
	} {
		t.Run(nonce, func(t *testing.T) {
			sig, err := Sign(testKey, http.MethodPost, path, ts, nonce, body)
			if err != nil {
				return // Sign refusing it is also correct
			}
			h := http.Header{}
			h.Set(HeaderTimestamp, ts)
			h.Set(HeaderNonce, nonce)
			h.Set(HeaderSignature, sig)
			if err := Verify(testKey, http.MethodPost, path, h, body, now); err == nil {
				t.Fatalf("nonce %q was accepted", nonce)
			}
		})
	}
}

// The REJECT half of the shared vectors. Agreeing on what is accepted while
// disagreeing on what is rejected is how two implementations of one scheme
// diverge, and that had already happened: vizra-search rebuilt the timestamp
// through ParseInt/FormatInt, so " 1789000000 ", "+1789000000" and
// "01789000000" verified there and were refused here.
//
// Every negative vector carries a GENUINE signature over exactly the fields as
// sent, so only the stated rule can refuse it.
//
// This test is what makes the vector file load-bearing: a change to the scheme
// without a matching change to the file turns it red.
func TestHMACNegativeTestVectors(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.NegativeVectors) == 0 {
		t.Fatal("the vector file has no negative vectors; the reject set is unspecified")
	}
	if vf.VerifierNowUnix == 0 {
		t.Fatal("the vector file does not fix the verifier's clock, so the window cases are unjudgeable")
	}
	key := []byte(vf.KeyUTF8)
	now := time.Unix(vf.VerifierNowUnix, 0)

	for _, v := range vf.NegativeVectors {
		t.Run(v.Name, func(t *testing.T) {
			if !v.MustReject {
				t.Fatal("a vector in negative_vectors is not marked must_reject")
			}
			if v.RejectBecause == "" {
				t.Fatal("a negative vector states no reason; both implementations need the rule, not just the outcome")
			}
			h := http.Header{}
			h.Set(HeaderTimestamp, v.Timestamp)
			h.Set(HeaderNonce, v.Nonce)
			h.Set(HeaderSignature, v.Signature)
			for k, extra := range v.ExtraHeaders {
				h.Add(k, extra)
			}
			if err := Verify(key, v.Method, v.Path, h, []byte(v.BodyUTF8), now); err == nil {
				t.Fatalf("ACCEPTED a vector that must be rejected.\n  rule: %s", v.RejectBecause)
			}
		})
	}
}

// Every vector's signature must be genuine, or a negative vector would "pass"
// because the signature was wrong rather than because the rule bit.
func TestNegativeVectorSignaturesAreGenuine(t *testing.T) {
	vf := loadVectors(t)
	key := []byte(vf.KeyUTF8)
	for _, v := range vf.NegativeVectors {
		t.Run(v.Name, func(t *testing.T) {
			canonical := CanonicalString(v.Method, v.Path, v.Timestamp, v.Nonce, BodyHash([]byte(v.BodyUTF8)))
			if canonical != v.Canonical {
				t.Fatalf("canonical string mismatch\n got: %q\nwant: %q", canonical, v.Canonical)
			}
			want := SignatureVersion + "=" + hmacHex(key, canonical)
			if want != v.Signature {
				t.Fatalf("the vector's signature is not a genuine signature over its own fields; "+
					"it would be rejected for the wrong reason\n got: %s\nwant: %s", v.Signature, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Security Finding 6 — no redirect following on the internal hop
// ---------------------------------------------------------------------------

// Go strips Authorization, WWW-Authenticate and Cookie on a cross-host
// redirect. It knows nothing about X-Vizra-Signature, X-Vizra-Timestamp or
// X-Vizra-Nonce, so before the CheckRedirect policy those were forwarded to
// whatever host the redirect named — handing a valid signature to a third party
// and turning core into a server-side fetch an attacker steers.
func TestARedirectIsNeverFollowedAndNoSignatureLeaks(t *testing.T) {
	var (
		mu       sync.Mutex
		second   int
		sawVizra []string
	)
	// The host a compromised or misconfigured search service would send us to.
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		second++
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-vizra-") {
				sawVizra = append(sawVizra, k)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[],"total":99}`))
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	s := NewService(NewSQL(), NewRemote(redirector.URL, testKey, 2*time.Second), quietLogger())

	// The read is still served — from SQL.
	res, err := s.Search(context.Background(), SearchRequest{Query: "sunset"})
	if err != nil {
		t.Fatalf("a redirecting search service must still be answered from SQL: %v", err)
	}
	if res.Total == 99 {
		t.Fatal("core used the redirected host's answer")
	}

	mu.Lock()
	gotSecond, gotHeaders := second, append([]string(nil), sawVizra...)
	mu.Unlock()
	if gotSecond != 0 {
		t.Fatalf("core made %d request(s) to the redirect target; it must make none", gotSecond)
	}
	if len(gotHeaders) > 0 {
		t.Fatalf("signature headers reached a second host: %v", gotHeaders)
	}
	// And the fault is visible, not silent.
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s after a redirect, want degraded", got)
	}
}

// The liveness ping must carry the same policy: it is the call Probe makes on a
// cold client, so a redirect there would be the first request core ever sends.
func TestPingDoesNotFollowRedirects(t *testing.T) {
	var second int
	var mu sync.Mutex
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		second++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer attacker.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/healthz", http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	s := NewService(NewSQL(), NewRemote(redirector.URL, testKey, 2*time.Second), quietLogger())
	if got := s.Health(context.Background()); got != HealthDegraded {
		t.Fatalf("health = %s for a search service that 301s its probe, want degraded", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if second != 0 {
		t.Fatalf("ping followed the redirect %d time(s)", second)
	}
}
