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

	// MinKeyBytes is the shortest accepted shared secret.
	MinKeyBytes = 32
)

var (
	ErrSignatureMissing = errors.New("search: signature headers are missing or malformed")
	ErrSignatureInvalid = errors.New("search: signature does not verify")
	ErrTimestampSkew    = errors.New("search: timestamp is outside the acceptance window")
	ErrKeyTooShort      = errors.New("search: shared secret is shorter than 32 bytes")
)

// CanonicalString builds the exact bytes that are signed:
//
//	v1\n<METHOD>\n<path>\n<timestamp>\n<nonce>\n<sha256hex(body)>
//
// with no trailing newline. path carries no query string. bodyHash is the
// lowercase hex SHA-256 of the RAW body bytes, hashed before decoding, so a
// verifier cannot be tricked by a re-encoding that changes the bytes.
func CanonicalString(method, path, timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{
		SignatureVersion,
		strings.ToUpper(method),
		path,
		timestamp,
		nonce,
		bodyHash,
	}, "\n")
}

// BodyHash is the lowercase hex SHA-256 of body. An empty body hashes to the
// SHA-256 of the empty string, not to the empty string.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign computes the header value for one request.
func Sign(key []byte, method, path, timestamp, nonce string, body []byte) (string, error) {
	if len(key) < MinKeyBytes {
		return "", ErrKeyTooShort
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
	ts := header.Get(HeaderTimestamp)
	nonce := header.Get(HeaderNonce)
	sig := header.Get(HeaderSignature)
	if ts == "" || nonce == "" || sig == "" {
		return ErrSignatureMissing
	}
	if len(nonce) < 32 { // 16 bytes, hex
		return ErrSignatureMissing
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrSignatureMissing
	}
	if !strings.HasPrefix(sig, SignatureVersion+"=") {
		return ErrSignatureMissing
	}

	delta := now.Sub(time.Unix(secs, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > MaxClockSkew {
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
