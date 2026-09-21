package audit

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// migrationPath is the file whose CHECK constraint this package must never
// violate. Reading its bytes — rather than copying the pattern into the test —
// is the repo idiom (cf. TestTheContractAndTheLoaderNameTheSameVariable).
const migrationPath = "../../migrations/0003_audit_events.up.sql"

// TestIPPrefixGrammarMatchesTheMigration fails if the Go patterns and the frozen
// SQL CHECK ever drift. Without it, the writer could be "total" against a
// grammar the database no longer enforces.
func TestIPPrefixGrammarMatchesTheMigration(t *testing.T) {
	raw, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("reading the frozen migration: %v", err)
	}
	sql := string(raw)
	for _, want := range []string{ipv4PrefixRe.String(), ipv6PrefixRe.String()} {
		// The SQL literal escapes a backslash the same way Go's backquoted
		// string does not, so compare on the un-escaped form.
		needle := strings.ReplaceAll(want, `\.`, `\.`)
		if !strings.Contains(sql, needle) {
			t.Errorf("migration 0003 does not contain the pattern this package enforces:\n  %s", needle)
		}
	}
}

// frozenCheck compiles the acceptance test the DATABASE will apply, from the
// migration's own bytes, so this test cannot pass against a stale copy.
func frozenCheck(t *testing.T) func(string) bool {
	t.Helper()
	raw, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("reading the frozen migration: %v", err)
	}
	found := regexp.MustCompile(`ip_prefix\s*~\s*'([^']+)'`).FindAllStringSubmatch(string(raw), -1)
	if len(found) != 2 {
		t.Fatalf("expected 2 ip_prefix patterns in the migration, found %d", len(found))
	}
	var res []*regexp.Regexp
	for _, m := range found {
		res = append(res, regexp.MustCompile(m[1]))
	}
	return func(s string) bool {
		for _, re := range res {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
}

// TestIPPrefixWriterOutputAlwaysSatisfiesTheFrozenCheck is the test that keeps an
// instance claimable.
//
// The audit insert shares the claim's transaction, so a value the CHECK refuses
// does not lose an audit row — it aborts the claim, and the operator can never
// claim the instance from that address. The cases that matter most are the ones
// a naive writer gets wrong: ::1 and :: both mask to "::/64", which the frozen
// IPv6 pattern REFUSES because it demands a hex group before "::".
func TestIPPrefixWriterOutputAlwaysSatisfiesTheFrozenCheck(t *testing.T) {
	accepts := frozenCheck(t)

	cases := []struct {
		name     string
		addr     string
		wantNil  bool
		wantText string
	}{
		{name: "IPv4 masks to /24", addr: "203.0.113.42:51000", wantText: "203.0.113.0/24"},
		{name: "IPv4 without a port", addr: "198.51.100.7", wantText: "198.51.100.0/24"},
		{name: "IPv6 masks to /64", addr: "[2001:db8:1:2:3:4:5:6]:443", wantText: "2001:db8:1:2::/64"},
		{name: "IPv4-mapped IPv6 is unmapped FIRST and masked to /24",
			addr: "[::ffff:203.0.113.9]:80", wantText: "203.0.113.0/24"},

		// Every one of these must be NULL. Each would otherwise produce a string
		// the frozen CHECK refuses, and abort the claim.
		{name: "IPv6 loopback", addr: "[::1]:8080", wantNil: true},
		{name: "IPv6 unspecified", addr: "[::]:8080", wantNil: true},
		{name: "IPv4 loopback", addr: "127.0.0.1:8080", wantNil: true},
		{name: "IPv4 unspecified", addr: "0.0.0.0:8080", wantNil: true},
		{name: "zoned link-local", addr: "fe80::1%eth0", wantNil: true},
		{name: "an address whose first 64 bits are zero", addr: "::2", wantNil: true},
		{name: "unparseable", addr: "not-an-address", wantNil: true},
		{name: "empty", addr: "", wantNil: true},
		{name: "hostname rather than an address", addr: "api:8080", wantNil: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IPPrefix(tc.addr)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("IPPrefix(%q) = %q, want nil (a non-nil value here can abort a claim)", tc.addr, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("IPPrefix(%q) = nil, want %q", tc.addr, tc.wantText)
			}
			if *got != tc.wantText {
				t.Fatalf("IPPrefix(%q) = %q, want %q", tc.addr, *got, tc.wantText)
			}
			if !accepts(*got) {
				t.Fatalf("IPPrefix(%q) = %q, which the FROZEN 0003 CHECK refuses", tc.addr, *got)
			}
		})
	}
}

// TestIPPrefixIsNeverFinerThanTheContract guards the masking width itself: a /32
// or /128 would be a full address in an immutable table.
func TestIPPrefixIsNeverFinerThanTheContract(t *testing.T) {
	if got := IPPrefix("203.0.113.42:1"); got == nil || !strings.HasSuffix(*got, ".0/24") {
		t.Fatalf("IPv4 must mask to /24, got %v", derefOr(got, "<nil>"))
	}
	if got := IPPrefix("[2001:db8:1:2:3:4:5:6]:1"); got == nil || !strings.HasSuffix(*got, "::/64") {
		t.Fatalf("IPv6 must mask to /64, got %v", derefOr(got, "<nil>"))
	}
}

// TestValidRejectsAnythingOutsideTheGrammar covers the belt-and-braces check the
// emitter runs immediately before the INSERT.
func TestValidRejectsAnythingOutsideTheGrammar(t *testing.T) {
	for _, bad := range []string{
		"::/64",           // the exact value a naive writer produces for ::1
		"invalid Prefix",  // netip's rendering of the zero Prefix
		"203.0.113.42/32", // a full address
		"203.0.113.42",
		"2001:db8::1",
		"",
	} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, but the frozen CHECK refuses it", bad)
		}
	}
	for _, good := range []string{"203.0.113.0/24", "2001:db8:1:2::/64"} {
		if !Valid(good) {
			t.Errorf("Valid(%q) = false, but the frozen CHECK accepts it", good)
		}
	}
}

func derefOr(p *string, alt string) string {
	if p == nil {
		return alt
	}
	return *p
}
