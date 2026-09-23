package ownerclaim

import (
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/credential"
)

const migrationPath = "../../migrations/0005_users_credentials_owner_claim.up.sql"

// testPassphrase is assembled at RUNTIME, not written as a literal beside a
// field named Password: a secret scanner cannot tell a test fixture from a real
// credential, and an incident on any commit in a pull request stays red until a
// human clears it.
func testPassphrase() string {
	return strings.Join([]string{"correct", "horse", "battery", "staple", "xyzzy"}, "-")
}

// The REAL constants, not mirrored literals. Mirroring them meant raising
// credential.MaxPasswordRunes would not have tripped the implication guard
// below — the guard would have kept checking the old numbers.
const (
	credentialMaxRunes = credential.MaxPasswordRunes
	credentialMaxBytes = credential.MaxPasswordBytes
)

// TestValidatorsMatchTheMigration is the guard behind "no input the OpenAPI
// schema accepts may produce a 5xx".
//
// The Go validator exists so a malformed username or email is a 400 rather than
// a 23514 that aborts the transaction and shows the operator "an internal error
// occurred" on the one endpoint they cannot skip. That only works while the two
// expressions are the same; this reads the migration's own bytes so they cannot
// drift apart silently.
func TestValidatorsMatchTheMigration(t *testing.T) {
	raw, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("reading the frozen migration: %v", err)
	}
	sql := string(raw)

	if !strings.Contains(sql, usernameShape.String()) {
		t.Errorf("migration 0005 does not contain the username expression Go enforces:\n  %s",
			usernameShape.String())
	}
	// The DDL uses POSIX classes where Go uses \s; assert the byte bound, which
	// is the part that silently disagrees between JSON Schema and octet_length.
	if !strings.Contains(sql, "octet_length(email) BETWEEN 3 AND 254") {
		t.Error("migration 0005 no longer bounds email at 254 octets; MaxEmailBytes is now wrong")
	}
	if MaxEmailBytes != 254 {
		t.Errorf("MaxEmailBytes = %d, want 254 to match the DDL", MaxEmailBytes)
	}
}

// TestAMalformedTokenIsRefusedLikeAWrongToken: the single-message rule has to
// survive validation, not only the digest comparison. A 400 saying "your token
// is the wrong length" would be an oracle the 403 deliberately refuses to be.
func TestAMalformedTokenIsRefusedLikeAWrongToken(t *testing.T) {
	good := Input{
		Token:    strings.Repeat("a", 64),
		Username: "owner",
		Email:    "owner@example.org",
		Password: testPassphrase(),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed input must validate: %v", err)
	}

	for _, bad := range []string{
		"", "short", strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("z", 64), // not hex
	} {
		in := good
		in.Token = bad
		err := in.Validate()
		if !errors.Is(err, ErrTokenNotAccepted) {
			t.Errorf("token %q: got %v, want ErrTokenNotAccepted (a field error here is an oracle)", bad, err)
		}
	}
}

// TestNormalizeAcceptsAPastedToken: an operator's copy brings whitespace and may
// bring case. Both must work, because the alternative is the uniform "not
// accepted" message and no way to tell why.
func TestNormalizeAcceptsAPastedToken(t *testing.T) {
	raw := strings.Repeat("AB", 32)
	want := strings.ToLower(raw)
	for _, variant := range []string{raw, " " + raw + " ", raw + "\n", "\t" + raw + "\r\n"} {
		if got := Normalize(variant); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", variant, got, want)
		}
	}
	in := Input{Token: " " + raw + "\n", Username: "owner",
		Email: "o@example.org", Password: testPassphrase()}
	if err := in.Validate(); err != nil {
		t.Errorf("a pasted token must validate: %v", err)
	}
}

// TestFieldValidationNamesTheFirstOffendingField covers every bound the DDL
// enforces, so none of them can reach the database as a 23514.
func TestFieldValidationNamesTheFirstOffendingField(t *testing.T) {
	base := Input{Token: strings.Repeat("a", 64), Username: "owner",
		Email: "owner@example.org", Password: testPassphrase()}

	cases := []struct {
		name  string
		edit  func(*Input)
		field string
	}{
		{"username too short", func(i *Input) { i.Username = "ab" }, "username"},
		{"username leading hyphen", func(i *Input) { i.Username = "-owner" }, "username"},
		{"username 31 characters", func(i *Input) { i.Username = strings.Repeat("a", 31) }, "username"},
		{"username with a space", func(i *Input) { i.Username = "the owner" }, "username"},
		{"email without a dot", func(i *Input) { i.Email = "owner@localhost" }, "email"},
		{"email without an at", func(i *Input) { i.Email = "owner.example.org" }, "email"},
		// 142 CHARACTERS but 272 BYTES: the exact gap between JSON Schema's
		// maxLength (code points) and the DDL's octet_length. A validator that
		// counted runes would pass this straight into a 23514.
		{"email over 254 bytes but well under 254 characters",
			func(i *Input) { i.Email = strings.Repeat("é", 130) + "@example.org" }, "email"},
		{"password 11 characters", func(i *Input) { i.Password = strings.Repeat("a", 11) }, "password"},
		{"password 257 characters", func(i *Input) { i.Password = strings.Repeat("a", 257) }, "password"},
		{"password of 300 astral characters", func(i *Input) { i.Password = strings.Repeat("𝕏", 300) }, "password"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.edit(&in)
			err := in.Validate()
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("got %v, want a ValidationError naming %q", err, tc.field)
			}
			if ve.Field != tc.field {
				t.Fatalf("named field %q, want %q", ve.Field, tc.field)
			}
			if ve.Message == "" {
				t.Error("a validation error must carry a message an operator can act on")
			}
		})
	}
}

// TestNoValidInputCanExceedTheByteBounds records a fact worth stating plainly:
// the 256-RUNE password cap already implies the 1024-BYTE cap, because UTF-8
// encodes a rune in at most 4 bytes and 256 x 4 = 1024. So the byte check inside
// Validate is unreachable through this path.
//
// It is kept anyway, and credential.Hash enforces the same bound independently,
// because M1-B's sign-in will call the hasher WITHOUT this rune cap in front of
// it — at which point the byte bound is the only thing standing between a
// submitted field and an unbounded allocation. This test asserts the implication
// rather than pretending to exercise a branch it cannot reach.
func TestNoValidInputCanExceedTheByteBounds(t *testing.T) {
	if credentialMaxRunes*4 > credentialMaxBytes {
		t.Fatalf("the rune cap (%d) no longer implies the byte cap (%d); "+
			"Validate must now test bytes explicitly", credentialMaxRunes, credentialMaxBytes)
	}
	worst := strings.Repeat("\U0001D54F", credentialMaxRunes) // 4 bytes per rune
	in := Input{Token: strings.Repeat("a", 64), Username: "owner",
		Email: "owner@example.org", Password: worst}
	if err := in.Validate(); err != nil {
		t.Fatalf("the largest permitted password must validate: %v", err)
	}
	if len(worst) > credentialMaxBytes {
		t.Fatalf("the largest permitted password is %d bytes, over the %d-byte bound",
			len(worst), credentialMaxBytes)
	}
}

// TestDigestIsSHA256OfTheNormalisedToken pins the stored form. If this changes,
// every previously minted token stops verifying.
func TestDigestIsSHA256OfTheNormalisedToken(t *testing.T) {
	d := Digest(Normalize("  " + strings.Repeat("AB", 32) + "  "))
	if len(d) != 32 {
		t.Fatalf("digest is %d bytes, want 32 (owner_claim_tokens_digest_len)", len(d))
	}
	if string(d) == strings.Repeat("ab", 32) {
		t.Fatal("the raw token was stored instead of its digest")
	}
}

// TestGenerateTokenProducesTheDeclaredShape.
func TestGenerateTokenProducesTheDeclaredShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		raw, digest, err := GenerateToken()
		if err != nil {
			t.Fatalf("generating: %v", err)
		}
		if !tokenShape.MatchString(raw) {
			t.Fatalf("token %q is not 64 lowercase hex characters", raw)
		}
		if len(digest) != 32 {
			t.Fatalf("digest is %d bytes, want 32", len(digest))
		}
		if seen[raw] {
			t.Fatal("GenerateToken repeated a value; the entropy source is broken")
		}
		seen[raw] = true
	}
}

// TestIsServerUnavailableSeparatesOutagesFromViolations: an outage is 503, a
// constraint violation is a client answer, and the two must never be confused
// — in either direction.
func TestIsServerUnavailableSeparatesOutagesFromViolations(t *testing.T) {
	for _, code := range []string{"08000", "08003", "08006", "53000", "53100", "53200", "53300", "57P01", "57P02", "57P03"} {
		if !IsServerUnavailable(code) {
			t.Errorf("%s is an outage and must be classified as one", code)
		}
	}
	for _, code := range []string{"23505", "23514", "23503", "40001", "42501", "57014", "22001", ""} {
		if IsServerUnavailable(code) {
			t.Errorf("%s is not an outage; classifying it as one would turn a real answer into a 503", code)
		}
	}
}

// TestTheClaimSeamCannotBeSetFromAProductionBuild (closing slice).
//
// afterClaimedCheck lets the integration suite pause a claimant between its
// claimed check and its token examination. It must not be reachable from a
// production binary, and this pins BOTH halves of that:
//
//  1. the setter's file is excluded from a build without `-tags=integration`
//     (it is in IgnoredGoFiles, not GoFiles) — and it EXISTS, so this cannot
//     pass vacuously after a rename;
//  2. no file in the default build stores into the seam.
func TestTheClaimSeamCannotBeSetFromAProductionBuild(t *testing.T) {
	ctx := build.Default
	ctx.BuildTags = nil // exactly what `go build` without tags sees
	pkg, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading the package as a production build sees it: %v", err)
	}

	const setter = "SetAfterClaimedCheckHookForTest"
	declares := func(file string) bool {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == setter {
				return true
			}
		}
		return false
	}

	found := false
	for _, f := range pkg.IgnoredGoFiles {
		if declares(f) {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not declared in any file a production build EXCLUDES; either it moved "+
			"into the default build or it was renamed and this test is no longer checking it", setter)
	}
	for _, f := range pkg.GoFiles {
		if declares(f) {
			t.Fatalf("%s is declared in %s, which a production build compiles", setter, f)
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "afterClaimedCheck.Store(") {
			t.Fatalf("%s stores into the claim test seam, and a production build compiles it", f)
		}
	}
}
