package search

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The HMAC scheme of api/search-internal.openapi.yaml § securitySchemes. It is
// implemented here in core, which owns the contract, and reimplemented in
// vizra-search. api/search-hmac-testvectors.json pins the exact bytes, so the
// two implementations are checked against the same vectors rather than against
// each other's prose.

const (
	HeaderTimestamp = "X-Vizra-Timestamp"
	HeaderNonce     = "X-Vizra-Nonce"
	HeaderSignature = "X-Vizra-Signature"

	// SignatureVersion prefixes both the canonical string and the header value,
	// so a future scheme change is distinguishable rather than ambiguous.
	SignatureVersion = "v1"

	// MaxClockSkew is the acceptance window in either direction.
	MaxClockSkew = 300 * time.Second

	// MinTimestamp and MaxTimestamp bound what a timestamp header may claim,
	// in unix seconds, BEFORE any arithmetic is done on it.
	//
	// This exists because the skew check used to convert the header into a
	// time.Time and subtract. At the ends of the representable range that does
	// not behave as it reads: time.Unix() with a huge seconds value wraps, and
	// a Duration of math.MinInt64 is its own negation, so folding the sign is a
	// no-op. A validly signed request with a timestamp of 253402300799 was
	// accepted and never expired. Until vizra-search owns a nonce store the
	// window is the ONLY replay bound, so it has to actually close.
	//
	// 2001-09-09 and 2100-01-01: no honest Vizra deployment signs outside this,
	// and both ends are ~4e9, far below anything that can overflow an int64
	// subtraction or a Duration.
	MinTimestamp int64 = 1_000_000_000
	MaxTimestamp int64 = 4_102_444_800

	// MinKeyBytes is the shortest accepted shared secret.
	MinKeyBytes = 32
)

var (
	ErrSignatureMissing   = errors.New("search: signature headers are missing or malformed")
	ErrMethodNotCanonical = errors.New("search: the HTTP method must be uppercase")
	ErrSignatureInvalid   = errors.New("search: signature does not verify")
	ErrTimestampSkew      = errors.New("search: timestamp is outside the acceptance window")
	ErrKeyTooShort        = errors.New("search: shared secret is shorter than 32 bytes")
)

// CanonicalString builds the exact bytes that are signed:
//
//	v1\n<METHOD>\n<path>\n<timestamp>\n<nonce>\n<sha256hex(body)>
//
// with no trailing newline. path carries no query string. bodyHash is the
// lowercase hex SHA-256 of the RAW body bytes, hashed before decoding, so a
// verifier cannot be tricked by a re-encoding that changes the bytes.
//
// EVERY FIELD IS USED VERBATIM. Nothing here uppercases, trims, reparses or
// reformats anything, and that is deliberate: this function is implemented
// twice, once in vizra-core and once in vizra-search. Any normalisation one
// side performs and the other does not is a pair of implementations that
// disagree about which requests are valid — which had already happened:
// vizra-search rebuilt the timestamp through ParseInt/FormatInt, so
// " 1789000000 ", "+1789000000" and "01789000000" verified there and were
// refused here. The shape rules are enforced by ValidateMethod,
// ValidateTimestamp and ValidateNonce instead, so a non-canonical field is
// REFUSED rather than quietly rewritten.
func CanonicalString(method, path, timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{
		SignatureVersion,
		method,
		path,
		timestamp,
		nonce,
		bodyHash,
	}, "\n")
}

// ValidateMethod requires an already-uppercase method.
func ValidateMethod(method string) error {
	if method == "" {
		return ErrMethodNotCanonical
	}
	for i := 0; i < len(method); i++ {
		if method[i] < 'A' || method[i] > 'Z' {
			return ErrMethodNotCanonical
		}
	}
	return nil
}

// ValidateTimestamp parses the timestamp header.
//
// The header MUST be unix seconds as bare decimal digits: no sign, no leading
// zero, no whitespace, no separators, no other base. strconv.ParseInt with
// base 10 already rejects most of those, but not a leading "+" and not leading
// zeros.
//
// The magnitude is checked against MinTimestamp/MaxTimestamp BEFORE the value
// is used in any arithmetic.
func ValidateTimestamp(raw string) (int64, error) {
	if raw == "" {
		return 0, ErrSignatureMissing
	}
	// Bare decimal digits, first digit non-zero. A value of "0" is out of range
	// anyway, so refusing a leading zero costs nothing.
	if raw[0] < '1' || raw[0] > '9' {
		return 0, ErrSignatureMissing
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, ErrSignatureMissing
		}
	}
	// Length bound before ParseInt: 19 digits is the most an int64 holds.
	if len(raw) > 19 {
		return 0, ErrTimestampSkew
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, ErrSignatureMissing
	}
	if secs < MinTimestamp || secs > MaxTimestamp {
		return 0, ErrTimestampSkew
	}
	return secs, nil
}

// ValidateNonce requires at least 16 bytes of LOWERCASE hex.
func ValidateNonce(nonce string) error {
	if len(nonce) < 32 || len(nonce)%2 != 0 || len(nonce) > 128 {
		return ErrSignatureMissing
	}
	for i := 0; i < len(nonce); i++ {
		c := nonce[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return ErrSignatureMissing
		}
	}
	return nil
}

// WithinWindow reports whether a validated timestamp is inside the acceptance
// window. Both operands are already bounded by ValidateTimestamp, so the
// subtraction is on plain int64 seconds and cannot overflow or wrap — which is
// the whole reason the comparison is not done on a derived time.Duration.
func WithinWindow(secs int64, now time.Time) bool {
	if secs < MinTimestamp || secs > MaxTimestamp {
		return false
	}
	nowSecs := now.Unix()
	if nowSecs < MinTimestamp || nowSecs > MaxTimestamp {
		// The verifier's own clock is nonsense. Fail closed: accepting on an
		// unusable clock would disable the only replay bound there is.
		return false
	}
	delta := nowSecs - secs
	if delta < 0 {
		delta = -delta
	}
	return delta <= int64(MaxClockSkew/time.Second)
}

// BodyHash is the lowercase hex SHA-256 of body. An empty body hashes to the
// SHA-256 of the empty string, not to the empty string.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign computes the header value for one request. It refuses a non-canonical
// method rather than normalising one, so a caller cannot produce a signature
// this package's own verifier would reject.
func Sign(key []byte, method, path, timestamp, nonce string, body []byte) (string, error) {
	if len(key) < MinKeyBytes {
		return "", ErrKeyTooShort
	}
	if err := ValidateMethod(method); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(CanonicalString(method, path, timestamp, nonce, BodyHash(body))))
	return SignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil)), nil
}

// NewNonce returns 16 bytes of randomness as lowercase hex.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// SignRequest sets all three headers on req. body must be the exact bytes that
// will be transmitted.
func SignRequest(req *http.Request, key []byte, body []byte, now time.Time) error {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	path := req.URL.EscapedPath()
	sig, err := Sign(key, req.Method, path, ts, nonce, body)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, sig)
	return nil
}

// Verify checks a received request's headers against body.
//
// It is exported so vizra-search can be tested against the same code path core
// signs with, and so core can verify its own vectors. Verification order is
// deliberate: the shape of the headers, then the clock window, then the
// constant-time signature compare. The returned error is for the server's own
// logs; the wire response is always 401 with `signature_rejected` and never
// says which header was wrong.
func Verify(key []byte, method, path string, header http.Header, body []byte, now time.Time) error {
	if len(key) < MinKeyBytes {
		return ErrKeyTooShort
	}
	if err := ValidateMethod(method); err != nil {
		return err
	}

	ts := header.Get(HeaderTimestamp)
	nonce := header.Get(HeaderNonce)
	sig := header.Get(HeaderSignature)
	if ts == "" || nonce == "" || sig == "" {
		return ErrSignatureMissing
	}
	// A duplicated header is ambiguous: Header.Get returns the first, another
	// stack might take the last. Refuse rather than pick.
	if len(header.Values(HeaderTimestamp)) != 1 ||
		len(header.Values(HeaderNonce)) != 1 ||
		len(header.Values(HeaderSignature)) != 1 {
		return ErrSignatureMissing
	}
	if err := ValidateNonce(nonce); err != nil {
		return err
	}
	if !strings.HasPrefix(sig, SignatureVersion+"=") {
		return ErrSignatureMissing
	}

	// Magnitude first, then the window, both on plain int64 seconds.
	secs, err := ValidateTimestamp(ts)
	if err != nil {
		return err
	}
	if !WithinWindow(secs, now) {
		return ErrTimestampSkew
	}

	want, err := Sign(key, method, path, ts, nonce, body)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return ErrSignatureInvalid
	}
	return nil
}
