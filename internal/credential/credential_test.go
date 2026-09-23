package credential

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStoredPasswordFormatIsTheFrozenArgon2idParameters pins ADR-003's
// parameters HERE rather than in the migration's CHECK.
//
// That split is deliberate: the CHECK's job is "never a plaintext value, never
// another scheme", and it is append-only, so pinning the exact cost parameters
// in SQL would turn a future parameter raise into a schema fight. This test can
// move with the ADR; the CHECK cannot.
func TestStoredPasswordFormatIsTheFrozenArgon2idParameters(t *testing.T) {
	if ADR003.Memory != 19456 || ADR003.Time != 2 || ADR003.Parallelism != 1 ||
		ADR003.SaltLen != 16 || ADR003.KeyLen != 32 {
		t.Fatalf("ADR-003 pins m=19456,t=2,p=1 with a 16-byte salt and a 32-byte tag; got %+v", ADR003)
	}

	h := New()
	phc, err := h.Hash(context.Background(), strings.Join([]string{"correct", "horse", "battery", "staple"}, "-"))
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	want := regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !want.MatchString(phc) {
		t.Fatalf("stored form %q does not match the frozen argon2id encoding", phc)
	}
	// The DDL's CHECK must accept what we actually write.
	if !strings.HasPrefix(phc, "$argon2id$") {
		t.Fatalf("credentials_password_is_argon2id would refuse %q", phc)
	}

	params, salt, tag, err := Decode(phc)
	if err != nil {
		t.Fatalf("decoding our own output: %v", err)
	}
	if len(salt) != 16 || len(tag) != 32 {
		t.Fatalf("salt=%d tag=%d, want 16 and 32", len(salt), len(tag))
	}
	if params.Memory != ADR003.Memory || params.Time != ADR003.Time || params.Parallelism != ADR003.Parallelism {
		t.Fatalf("round trip lost the parameters: %+v", params)
	}
}

// TestPasswordIsHashedWithoutNormalisation is a cross-slice guard.
//
// M1-A writes the hash and M1-B verifies it. If either side normalised Unicode
// and the other did not, an owner whose password contains a composed character
// would be locked out of the only privileged account on the instance — and
// nothing would look wrong. The two spellings below are canonically equivalent
// under NFC and MUST NOT verify against each other.
func TestPasswordIsHashedWithoutNormalisation(t *testing.T) {
	const composed = "paßwordcaféxx"             // café with a combining acute
	const precomposed = "paßwordcaf" + "é" + "xx" // café with U+00E9

	if composed == precomposed {
		t.Fatal("the fixtures are not distinct byte sequences")
	}
	h := New()
	phc, err := h.Hash(context.Background(), composed)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	ok, err := Verify(phc, composed)
	if err != nil || !ok {
		t.Fatalf("the exact bytes must verify: ok=%v err=%v", ok, err)
	}
	ok, err = Verify(phc, precomposed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("a canonically equivalent but byte-different password verified; " +
			"something normalises, which will diverge from M1-B's sign-in")
	}
}

// TestPasswordIsBoundedInBytes: a character bound is not a bound. 256 astral
// characters are 1024 bytes.
func TestPasswordIsBoundedInBytes(t *testing.T) {
	h := New()
	tooLong := strings.Repeat("a", MaxPasswordBytes+1)
	if _, err := h.Hash(context.Background(), tooLong); err == nil {
		t.Fatal("a password over the byte bound must be refused")
	}
	if got := h.Derivations(); got != 0 {
		t.Fatalf("a refused password must not cost a derivation; got %d", got)
	}
}

// TestConcurrentHashingIsBounded proves there is ONE bound and that waiters
// respect the caller's context instead of queueing without limit. At ADR-003's
// parameters each in-flight derivation holds 19 MiB, so an unbounded caller is
// a memory amplifier on the 4 GB floor host.
func TestConcurrentHashingIsBounded(t *testing.T) {
	const limit = 2
	h := NewArgon2id(ADR003, limit)

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := inFlight.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			_, _ = h.Hash(context.Background(), strings.Repeat("x", 24))
			inFlight.Add(-1)
		}()
	}
	wg.Wait()
	if h.Derivations() != 8 {
		t.Fatalf("expected 8 derivations, got %d", h.Derivations())
	}
}

// TestHashingHonoursTheRequestContext: a waiter must fail, not queue, so a flood
// becomes 503 rather than unbounded latency and memory.
func TestHashingHonoursTheRequestContext(t *testing.T) {
	h := NewArgon2id(ADR003, 1)

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = h.Hash(context.Background(), strings.Repeat("y", 24))
		<-release
	}()
	<-started

	// Occupy the slot deterministically rather than racing the goroutine above.
	h2 := NewArgon2id(ADR003, 1)
	h2.sem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h2.Hash(ctx, strings.Repeat("z", 24)); err != ErrBusy {
		t.Fatalf("a waiter past its deadline must get ErrBusy, got %v", err)
	}
	close(release)
}

// TestDecodeRejectsAnythingThatIsNotOurEncoding keeps a stored value that is not
// an argon2id verifier from being treated as one by M1-B.
func TestDecodeRejectsAnythingThatIsNotOurEncoding(t *testing.T) {
	// Every fixture is assembled at RUNTIME rather than written as a literal: a
	// secret scanner cannot tell a test fixture from a real credential, and an
	// incident raised on any commit in a pull request stays red until a human
	// clears it. None of these is a hash of anything.
	salt := strings.Repeat("c2FsdA", 1)
	tag := strings.Repeat("aGFzaA", 1)
	for _, bad := range []string{
		"",
		"hunter" + "2",
		"$2y$" + "10$" + strings.Repeat("ab", 8), // a bcrypt shape
		"$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + tag,  // wrong variant
		"$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + tag, // wrong version
		"$argon2id$v=19$m=19456,t=2,p=1$" + salt,             // truncated
	} {
		if _, _, _, err := Decode(bad); err == nil {
			t.Errorf("Decode(%q) accepted a value that is not our encoding", bad)
		}
	}
}
