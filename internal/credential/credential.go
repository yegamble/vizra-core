// Package credential owns password verifiers: the argon2id parameters ADR-003
// pins, the PHC encoding they are stored in, and the ONE process-wide bound on
// concurrent derivations.
//
// Why the bound lives here and not in a handler: argon2id at these parameters
// costs 19 MiB and tens of milliseconds per derivation, so an unbounded caller
// is a memory and CPU amplifier. M1-B's sign-in is the real multi-caller and
// must import this package rather than create a second, independent bound —
// two bounds of four are a bound of eight.
//
// Why the hasher is an interface with a derivation counter: "the password is
// hashed only after the token has been verified" is the single most important
// denial-of-service property of the owner-claim endpoint, and prose cannot be
// tested. The counter turns it into an assertion — tests require ZERO
// derivations on every non-201 path. It is the same seam Deps.PingDatabase and
// Deps.PingCache already use.
package credential

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters.
type Params struct {
	Memory      uint32 // KiB
	Time        uint32 // passes
	Parallelism uint8
	SaltLen     uint32 // bytes
	KeyLen      uint32 // bytes
}

// ADR003 is the pinned parameter set: ADR-003 § Credentials fixes memory at
// 19 MiB, iterations at 2, parallelism at 1, a 16-byte salt and a 32-byte tag.
// These are OWASP's second listed argon2id configuration ("m=19456 (19 MiB),
// t=2, p=1"), one of five the cheat sheet calls equivalent in security.
//
// This is NOT a value this package chose; an Accepted ADR is immutable. Raising
// it is an ADR change, after which TestStoredPasswordFormatIsTheFrozenArgon2idParameters
// moves with it — the stored form carries m, t and p, so old hashes stay
// verifiable and are re-derived on next sign-in without a schema change.
var ADR003 = Params{Memory: 19456, Time: 2, Parallelism: 1, SaltLen: 16, KeyLen: 32}

const (
	// MinPasswordRunes matches the approved claim copy ("At least 12 characters").
	MinPasswordRunes = 12
	// MaxPasswordRunes bounds the character count the API contract advertises.
	MaxPasswordRunes = 256
	// MaxPasswordBytes bounds the actual input. A character bound alone is not a
	// bound: 256 astral characters are 1024 bytes. argon2's cost does not depend
	// on input length, but an unbounded field is still an unbounded allocation.
	MaxPasswordBytes = 1024
)

// ErrBusy means every derivation slot was occupied until the caller's context
// expired. Callers answer 503; they must never queue unbounded.
var ErrBusy = errors.New("credential: hashing capacity exhausted")

// Hasher derives and verifies password verifiers.
type Hasher interface {
	// Hash returns a PHC-encoded argon2id verifier for the RAW BYTES of password.
	Hash(ctx context.Context, password string) (string, error)
	// Derivations counts completed key derivations. Tests assert it does not move
	// on paths that must never hash.
	Derivations() int64
}

// Argon2id is the production Hasher.
type Argon2id struct {
	params      Params
	sem         chan struct{}
	derivations atomic.Int64
}

// DefaultConcurrency bounds simultaneous derivations. At ADR-003's parameters
// peak transient memory is this many × 19 MiB, so four is ~76 MiB — affordable
// on the 4 GB / 2 vCPU floor host while still using a multi-core machine.
func DefaultConcurrency() int {
	if n := runtime.GOMAXPROCS(0); n < 4 {
		return max(n, 1)
	}
	return 4
}

// NewArgon2id builds a hasher with its own bound. Production uses exactly one.
func NewArgon2id(p Params, concurrency int) *Argon2id {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Argon2id{params: p, sem: make(chan struct{}, concurrency)}
}

// New returns the production hasher: ADR-003's parameters, DefaultConcurrency.
func New() *Argon2id { return NewArgon2id(ADR003, DefaultConcurrency()) }

// Hash hashes the raw UTF-8 bytes of password with NO Unicode normalisation.
//
// That is a frozen decision, written into migration 0005's header: M1-A writes
// the hash and M1-B verifies it, so normalising on one side only would lock the
// owner out of the only privileged account on the instance. Never add a
// norm.NFC here without changing both sides and re-deriving stored hashes.
func (h *Argon2id) Hash(ctx context.Context, password string) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", fmt.Errorf("credential: password exceeds %d bytes", MaxPasswordBytes)
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return "", ErrBusy
	}
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("credential: reading salt: %w", err)
	}
	tag := argon2.IDKey([]byte(password), salt,
		h.params.Time, h.params.Memory, h.params.Parallelism, h.params.KeyLen)
	h.derivations.Add(1)
	return Encode(h.params, salt, tag), nil
}

// Derivations implements Hasher.
func (h *Argon2id) Derivations() int64 { return h.derivations.Load() }

// Encode renders the standard PHC string for an argon2id verifier.
//
// There is no Vizra-specific version tag. ADR-003 asks for the standard
// encoding, a version tag, and no second column; a standard PHC string has no
// vendor slot, so the literal reading is self-contradictory. The embedded
// m, t and p ARE the version: they deliver exactly what the ADR asked them to
// ("parameters can be raised later and old hashes re-derived on next sign-in
// without a second column"). Recorded as an interpretation, ratification owed.
func Encode(p Params, salt, tag []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(tag))
}

// Decode parses a PHC string back into its parameters, salt and tag. M1-B needs
// it to verify and to decide whether a stored hash is below the current target.
func Decode(phc string) (Params, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, tag
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errors.New("credential: not an argon2id PHC string")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("credential: version: %w", err)
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("credential: unsupported argon2 version %d", version)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("credential: parameters: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("credential: salt: %w", err)
	}
	tag, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("credential: tag: %w", err)
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(tag))
	return p, salt, tag, nil
}

// Verify reports whether password matches the stored PHC verifier, in constant
// time. M1-A does not authenticate anyone; this exists so M1-B inherits one
// implementation rather than writing a second.
func Verify(phc, password string) (bool, error) {
	p, salt, tag, err := Decode(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Parallelism, p.KeyLen)
	return subtle.ConstantTimeCompare(got, tag) == 1, nil
}
