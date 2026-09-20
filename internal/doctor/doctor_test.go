package doctor_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/doctor"
	"github.com/yegamble/vizra-core/internal/migrate"
)

// These tests exist because three independent mutants of the doctor checks
// survived the entire gate: the schema-drift check reporting OK, the cache
// floor check deleted, and an invalid configuration reporting OK. Each test
// below kills one of those, and each asserts the reported STATUS and the
// resulting EXIT CODE — an operator reads the status, a script reads the exit
// code, and a doctor that prints FAIL and exits 0 is as useless as one that
// prints OK.

func statusOf(t *testing.T, results []doctor.Result, name string) doctor.Status {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r.Status
		}
	}
	t.Fatalf("no check named %q in %+v", name, results)
	return ""
}

// ---------------------------------------------------------------------------
// Schema drift — every drift state is a FAIL
// ---------------------------------------------------------------------------

func TestCheckSchema(t *testing.T) {
	cases := []struct {
		name   string
		st     migrate.Status
		want   doctor.Status
		detail string // a substring the operator needs to see
	}{
		{
			name: "current",
			st:   migrate.Status{State: migrate.StateCurrent, AppliedVersion: 4, EmbeddedVersion: 4},
			want: doctor.StatusOK,
		},
		{
			name:   "behind: the code may be reading columns that do not exist",
			st:     migrate.Status{State: migrate.StateBehind, AppliedVersion: 2, EmbeddedVersion: 4, Detail: "run `vizra migrate`"},
			want:   doctor.StatusFail,
			detail: "vizra migrate",
		},
		{
			name:   "ahead: this binary must not be rolled out",
			st:     migrate.Status{State: migrate.StateAhead, AppliedVersion: 9, EmbeddedVersion: 4, Detail: "refused"},
			want:   doctor.StatusFail,
			detail: "NEWER",
		},
		{
			name:   "dirty: a migration failed part-way",
			st:     migrate.Status{State: migrate.StateDirty, AppliedVersion: 3, EmbeddedVersion: 4, Detail: "resolve it deliberately"},
			want:   doctor.StatusFail,
			detail: "deliberately",
		},
		{
			// Unreadable was NOT checked. It must never be OK.
			name: "unknown: the ledger could not be read",
			st:   migrate.Status{State: migrate.StateUnknown, EmbeddedVersion: 4},
			want: doctor.StatusSkip,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doctor.CheckSchema(tc.st)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (detail %q).\n"+
					"A doctor that reports OK on a drifted schema actively misdirects the investigation.",
					got.Status, tc.want, got.Detail)
			}
			if tc.detail != "" && !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tc.detail)
			}
			if tc.want == doctor.StatusFail && doctor.ExitCode([]doctor.Result{got}) == 0 {
				t.Error("a failed schema check exits 0; a script would treat the instance as healthy")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cache flavour and version floor
// ---------------------------------------------------------------------------

func TestCheckCacheFloor(t *testing.T) {
	cases := []struct {
		version string
		want    doctor.Status
	}{
		{"9.1.2", doctor.StatusOK},   // the pinned Valkey
		{"7.2.16", doctor.StatusOK},  // the Redis matrix leg
		{"7.2.0", doctor.StatusOK},   // exactly at the floor
		{"8.0.11", doctor.StatusOK},  //
		{"10.0.0", doctor.StatusOK},  // a major above the floor must not be rejected
		{"7.1.9", doctor.StatusFail}, // one minor below
		{"6.2.14", doctor.StatusFail},
		{"5.0.0", doctor.StatusFail},
		// Fail CLOSED on anything unparseable, rather than assuming it is new
		// enough.
		{"", doctor.StatusFail},
		{"unknown", doctor.StatusFail},
		{"v-not-a-version", doctor.StatusFail},
		{"seven point two", doctor.StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			got := doctor.CheckCacheFloor(cache.ServerInfo{Flavour: cache.FlavourValkey, Version: tc.version})
			if got.Status != tc.want {
				t.Fatalf("version %q -> %s, want %s (detail %q)", tc.version, got.Status, tc.want, got.Detail)
			}
		})
	}
}

// The flavour must be NAMED, not just "up": the licence and the supported
// command set depend on which server answered (ADR-001 Q-004).
func TestCheckCacheReportsFlavourAndVersion(t *testing.T) {
	results := doctor.CheckCache(cache.ServerInfo{Flavour: cache.FlavourValkey, Version: "9.1.2"}, nil, nil)
	if statusOf(t, results, "cache") != doctor.StatusOK {
		t.Fatalf("a healthy cache did not report OK: %+v", results)
	}
	var detail string
	for _, r := range results {
		if r.Name == "cache" {
			detail = r.Detail
		}
	}
	if !strings.Contains(detail, "valkey") || !strings.Contains(detail, "9.1.2") {
		t.Fatalf("cache detail = %q; it must name the flavour and version", detail)
	}
	// The floor check must be part of the same run — deleting it was a
	// surviving mutant.
	if statusOf(t, results, "cache version floor") != doctor.StatusOK {
		t.Fatalf("CheckCache did not include the version floor check: %+v", results)
	}
}

func TestCheckCacheBelowTheFloorFails(t *testing.T) {
	results := doctor.CheckCache(cache.ServerInfo{Flavour: cache.FlavourRedis, Version: "6.2.0"}, nil, nil)
	if statusOf(t, results, "cache version floor") != doctor.StatusFail {
		t.Fatalf("a 6.2 server passed the 7.2 floor: %+v", results)
	}
	if doctor.ExitCode(results) != 1 {
		t.Fatal("a cache below the floor exits 0")
	}
}

func TestCheckCacheUnreachableFails(t *testing.T) {
	results := doctor.CheckCache(cache.ServerInfo{}, errors.New("connection refused"), nil)
	if statusOf(t, results, "cache") != doctor.StatusFail {
		t.Fatalf("an unreachable cache did not fail: %+v", results)
	}
	if doctor.ExitCode(results) != 1 {
		t.Fatal("an unreachable cache exits 0")
	}
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

func TestCheckDatabase(t *testing.T) {
	if got := doctor.CheckDatabase(nil); got.Status != doctor.StatusOK {
		t.Fatalf("a reachable database reported %s", got.Status)
	}
	got := doctor.CheckDatabase(errors.New(`failed to connect to "postgres://vizra:hunter2@db:5432/vizra"`))
	if got.Status != doctor.StatusFail {
		t.Fatalf("an unreachable database reported %s", got.Status)
	}
	if doctor.ExitCode([]doctor.Result{got}) != 1 {
		t.Fatal("an unreachable database exits 0")
	}
	// A pgx dial error carries the DSN, and the DSN carries the password.
	if strings.Contains(got.Detail, "hunter2") || strings.Contains(got.Detail, "postgres://") {
		t.Fatalf("doctor leaked connection information: %q", got.Detail)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestCheckConfigReportsEveryProblemAndFails(t *testing.T) {
	bad := map[string]string{
		"VIZRA_MODE":                 "production",
		"VIZRA_PUBLIC_ORIGIN":        "http://photos.example.org",
		"DATABASE_URL":               "postgres://vizra@db:5432/vizra",
		"VIZRA_CACHE_URL":            "redis://cache:6379/0",
		"VIZRA_SESSION_SECRET":       "changeme",
		"VIZRA_CORS_ALLOWED_ORIGINS": "*",
		"VIZRA_DEV_DISABLE_AUTH":     "true",
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := bad[k]; return v, ok })
	results := doctor.CheckConfig(cfg, err)

	if len(results) < 4 {
		t.Fatalf("an invalid configuration produced %d result(s); every problem must be reported at once "+
			"so an operator is not fixing one variable per boot: %+v", len(results), results)
	}
	for _, r := range results {
		if r.Status != doctor.StatusFail {
			t.Errorf("%s reported %s for an invalid configuration", r.Name, r.Status)
		}
	}
	if doctor.ExitCode(results) != 1 {
		t.Fatal("an invalid configuration exits 0; `vizra doctor && deploy` would proceed")
	}
	// The problems must name the keys, and must not echo the secret.
	joined := ""
	for _, r := range results {
		joined += r.Name + " " + r.Detail + "\n"
	}
	for _, want := range []string{"VIZRA_SESSION_SECRET", "VIZRA_CORS_ALLOWED_ORIGINS", "VIZRA_DEV_DISABLE_AUTH"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not name %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "changeme") {
		t.Errorf("doctor echoed the refused secret:\n%s", joined)
	}
}

func TestCheckConfigValidReportsOK(t *testing.T) {
	good := map[string]string{
		"VIZRA_MODE":           "production",
		"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
		"DATABASE_URL":         "postgres://vizra@db:5432/vizra?sslmode=require",
		"VIZRA_CACHE_URL":      "redis://cache:6379/0",
		"VIZRA_SESSION_SECRET": strings.Repeat("Vz9Kp4Mw2Ng7", 3)[:32],
		"VIZRA_MFA_KEY_KEK":    strings.Repeat("Vz9Kp4Mw2Ng7", 3)[:32],
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := good[k]; return v, ok })
	results := doctor.CheckConfig(cfg, err)
	if len(results) != 1 || results[0].Status != doctor.StatusOK {
		t.Fatalf("a valid configuration did not report a single OK: %+v", results)
	}
	if doctor.ExitCode(results) != 0 {
		t.Fatal("a valid configuration exits non-zero")
	}
}

// ---------------------------------------------------------------------------
// Compose
// ---------------------------------------------------------------------------

func TestCheckCompose(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		lookErr error
		runErr  error
		want    doctor.Status
	}{
		{name: "at the floor", raw: "2.24.4", want: doctor.StatusOK},
		{name: "above the floor", raw: "2.40.3", want: doctor.StatusOK},
		// Compose is at 5.x; a parser anchored on "2." would reject every
		// current installation.
		{name: "a major above 2", raw: "v5.5.1", want: doctor.StatusOK},
		{name: "below the floor", raw: "2.24.3", want: doctor.StatusFail},
		{name: "well below", raw: "2.18.0", want: doctor.StatusFail},
		// Fail CLOSED on an unparseable string (Q-017).
		{name: "unparseable", raw: "docker-compose version unknown", want: doctor.StatusFail},
		{name: "empty", raw: "", want: doctor.StatusFail},
		{name: "command failed", runErr: errors.New("exit 1"), want: doctor.StatusFail},
		// NOT INSTALLED is SKIP, never OK: nothing was verified.
		{name: "docker absent", lookErr: errors.New("not found"), want: doctor.StatusSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doctor.CheckCompose(tc.raw, tc.lookErr, tc.runErr)
			if got.Status != tc.want {
				t.Fatalf("%q -> %s, want %s (detail %q)", tc.raw, got.Status, tc.want, got.Detail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestCheckSearch(t *testing.T) {
	if got := doctor.CheckSearch(config.SearchOff, false); got.Status != doctor.StatusOK {
		t.Fatalf("SEARCH_MODE=off reported %s; off is the M0 default, not a fault", got.Status)
	}
	// Q-001: a configured but unreachable search is a HARD FAIL. The
	// alternative is a silent fallback in which a broken search looks exactly
	// like a working one.
	for _, mode := range []config.SearchMode{config.SearchManaged, config.SearchExternal} {
		got := doctor.CheckSearch(mode, false)
		if got.Status != doctor.StatusFail {
			t.Fatalf("SEARCH_MODE=%s unreachable reported %s, want FAIL", mode, got.Status)
		}
		if doctor.ExitCode([]doctor.Result{got}) != 1 {
			t.Fatalf("an unreachable configured search exits 0")
		}
		if got := doctor.CheckSearch(mode, true); got.Status != doctor.StatusOK {
			t.Fatalf("SEARCH_MODE=%s reachable reported %s", mode, got.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// Reporting and exit codes
// ---------------------------------------------------------------------------

func TestSkipIsNeverAPassButDoesNotFailTheRun(t *testing.T) {
	results := []doctor.Result{
		{Name: "a", Status: doctor.StatusOK},
		{Name: "b", Status: doctor.StatusSkip, Detail: "docker is not on PATH"},
	}
	if doctor.ExitCode(results) != 0 {
		t.Error("a SKIP failed the run; it was not checked, but it is not broken either")
	}
	c := doctor.Summarise(results)
	if c.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", c.Skipped)
	}

	var sb strings.Builder
	if err := doctor.Report(&sb, results); err != nil {
		t.Fatalf("Report returned an error for a run with no failures: %v", err)
	}
	// The summary must state the skips. "2 checks, 0 failed" while one never
	// ran is the lie this line exists to prevent.
	if !strings.Contains(sb.String(), "1 not run") {
		t.Fatalf("the summary hides the skipped check:\n%s", sb.String())
	}
}

func TestReportReturnsAnErrorWhenAnyCheckFails(t *testing.T) {
	results := []doctor.Result{
		{Name: "a", Status: doctor.StatusOK},
		{Name: "b", Status: doctor.StatusFail, Detail: "broken"},
		{Name: "c", Status: doctor.StatusWarn},
	}
	err := doctor.Report(io.Discard, results)
	if err == nil {
		t.Fatal("Report returned nil with a FAIL present; the process would exit 0")
	}
	if doctor.ExitCode(results) != 1 {
		t.Fatal("ExitCode = 0 with a FAIL present")
	}
	// A WARN alone must not fail the run.
	if doctor.ExitCode([]doctor.Result{{Status: doctor.StatusWarn}}) != 0 {
		t.Error("a WARN failed the run")
	}
}

func TestParseVersionIsAnchored(t *testing.T) {
	// Anchored, so a string that merely contains digits does not parse. The
	// callers fail closed on !ok, and that only helps if !ok is reachable.
	for _, s := range []string{"", "unknown", "version 2.24.4", "abc1.2.3"} {
		if _, ok := doctor.ParseVersion(s); ok {
			t.Errorf("ParseVersion(%q) parsed; the fail-closed branch would be unreachable", s)
		}
	}
	for _, tc := range []struct {
		in   string
		want [3]int
	}{
		{"2.24.4", [3]int{2, 24, 4}},
		{"v5.5.1", [3]int{5, 5, 1}},
		{"9.1", [3]int{9, 1, 0}},
		{"7.2.16 (Debian)", [3]int{7, 2, 16}},
	} {
		got, ok := doctor.ParseVersion(tc.in)
		if !ok || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, true", tc.in, got, ok, tc.want)
		}
	}
}
