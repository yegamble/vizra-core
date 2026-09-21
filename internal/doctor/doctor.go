// Package doctor holds the checks `vizra doctor` runs.
//
// It lives here rather than in cmd/vizra so the checks can be tested. They were
// in cmd/, which has no test files, and three independent mutants survived the
// whole gate: the schema-drift check reporting OK, the cache floor check
// deleted outright, and an invalid configuration reporting OK. A diagnostic
// that lies is more dangerous than no diagnostic, because it actively redirects
// the investigation — and doctor is what an operator runs precisely when
// something is already wrong.
//
// Every function here is PURE: it takes the facts and returns a verdict. The
// I/O — opening pools, running `docker compose version`, reading INFO — stays
// in cmd/vizra, so a test can drive every verdict without infrastructure. The
// verdicts are the part that has been wrong.
package doctor

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/migrate"
)

// Status is a check outcome. There are four, and the distinction is the whole
// point:
//
//	OK   — checked, and it passed.
//	WARN — checked, not fatal, but an operator should know.
//	FAIL — checked, and it is broken. Exit code 1.
//	SKIP — NOT CHECKED, with the reason. Never counted as a pass.
//
// A doctor that reports OK for something it could not test converts an unknown
// into a false assurance.
type Status string

const (
	StatusOK   Status = "OK"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one check.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// CheckConfig turns the loader's verdict into results. It reports one FAIL per
// problem, so an operator fixing an env file sees every one at once — the same
// reason config.validate collects rather than returning the first.
//
// It takes the loader's OUTPUT rather than calling it, so there is exactly one
// validator (config.LoadFrom) and doctor cannot drift from boot.
func CheckConfig(cfg *config.Config, err error) []Result {
	if err != nil {
		if ve, ok := config.AsValidationError(err); ok {
			out := make([]Result, 0, len(ve.Problems))
			for _, p := range ve.Problems {
				out = append(out, Result{"config: " + p.Key, StatusFail, p.Message})
			}
			return out
		}
		return []Result{{"configuration", StatusFail, err.Error()}}
	}
	if cfg == nil {
		// Neither a config nor an error is a programming mistake in the caller.
		// SKIP, not OK: nothing was verified.
		return []Result{{"configuration", StatusSkip, "the configuration was not loaded"}}
	}
	return []Result{{"configuration", StatusOK,
		fmt.Sprintf("%d keys validated, mode=%s", len(config.AllKeys()), cfg.Mode)}}
}

// ---------------------------------------------------------------------------
// Database and schema
// ---------------------------------------------------------------------------

// CheckDatabase reports reachability. err is the result of a real Ping.
func CheckDatabase(err error) Result {
	if err != nil {
		// Never include the error text: a pgx dial error carries the DSN, and
		// the DSN carries the password.
		return Result{"database", StatusFail, "PostgreSQL is unreachable"}
	}
	return Result{"database", StatusOK, "reachable"}
}

// CheckSchema turns a migration-ledger probe into a verdict.
//
// EVERY drift state is a FAIL, not a warning. A schema behind the binary means
// the code is executing against columns that may not exist; ahead means this
// binary must not be rolled out at all (ADR-002 § Rollback floor); dirty means a
// migration failed part-way and the database is in an unknown shape. An
// operator running doctor because something is wrong needs each of those to be
// the answer, not a footnote.
func CheckSchema(st migrate.Status) Result {
	switch st.State {
	case migrate.StateCurrent:
		return Result{"schema", StatusOK, fmt.Sprintf("version %d", st.AppliedVersion)}
	case migrate.StateBehind:
		return Result{"schema", StatusFail, fmt.Sprintf(
			"database at %d, this binary embeds %d: %s", st.AppliedVersion, st.EmbeddedVersion, st.Detail)}
	case migrate.StateAhead:
		return Result{"schema", StatusFail, fmt.Sprintf(
			"database at %d is NEWER than this binary's %d: %s", st.AppliedVersion, st.EmbeddedVersion, st.Detail)}
	case migrate.StateDirty:
		return Result{"schema", StatusFail, st.Detail}
	default:
		// Unreadable is SKIP: it was not checked. It is not OK.
		return Result{"schema", StatusSkip, "the migration ledger could not be read"}
	}
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// CacheFloor is ADR-001's EXTERNAL floor: any RESP-compatible server >= 7.2.
var CacheFloor = [3]int{7, 2, 0}

// CheckCache reports reachability and identity. The FLAVOUR and VERSION are
// printed, not just "up": the licence and the supported command set depend on
// which server answered (ADR-001 Q-004), and `doctor` is where an operator
// finds that out.
func CheckCache(info cache.ServerInfo, pingErr, identifyErr error) []Result {
	if pingErr != nil {
		return []Result{{"cache", StatusFail,
			"unreachable; rate limiting would run on the per-process fallback and readiness would report degraded"}}
	}
	if identifyErr != nil {
		return []Result{{"cache", StatusWarn, "reachable, but INFO server did not report a version"}}
	}
	return []Result{
		{"cache", StatusOK, info.String()},
		CheckCacheFloor(info),
	}
}

// CheckCacheFloor refuses a server below the 7.2 command set, and FAILS CLOSED
// on a version string it cannot parse rather than assuming it is new enough.
func CheckCacheFloor(info cache.ServerInfo) Result {
	v, ok := ParseVersion(info.Version)
	if !ok {
		return Result{"cache version floor", StatusFail, fmt.Sprintf(
			"could not parse the server version %q; refusing to assume it meets the %d.%d floor",
			info.Version, CacheFloor[0], CacheFloor[1])}
	}
	if CompareVersion(v, CacheFloor) < 0 {
		return Result{"cache version floor", StatusFail, fmt.Sprintf(
			"%s is below the %d.%d command-set floor Vizra requires",
			info.String(), CacheFloor[0], CacheFloor[1])}
	}
	return Result{"cache version floor", StatusOK, fmt.Sprintf(
		"%s meets the %d.%d floor", info.String(), CacheFloor[0], CacheFloor[1])}
}

// ---------------------------------------------------------------------------
// Compose
// ---------------------------------------------------------------------------

// ComposeFloor is Q-017's documented minimum for `!override`.
var ComposeFloor = [3]int{2, 24, 4}

// CheckCompose evaluates the Compose version.
//
// raw is the output of `docker compose version --short`. lookErr is non-nil
// when docker is not on PATH; runErr when the command failed.
//
// Q-017: fail CLOSED on an unparseable version string, and accept majors above
// 2 — Compose is at 5.x and the 2.x line ended at 2.40.3, so a parser anchored
// on "2." would reject every current installation.
func CheckCompose(raw string, lookErr, runErr error) Result {
	if lookErr != nil {
		// Not installed is SKIP, not OK: nothing was verified.
		return Result{"docker compose", StatusSkip, "docker is not on PATH, so the Compose floor was not checked"}
	}
	if runErr != nil {
		return Result{"docker compose", StatusFail,
			"`docker compose version` failed; the Compose plugin may not be installed"}
	}
	raw = strings.TrimSpace(raw)
	v, ok := ParseVersion(raw)
	if !ok {
		return Result{"docker compose", StatusFail, fmt.Sprintf(
			"could not parse the Compose version %q; refusing to assume it meets the %d.%d.%d floor",
			raw, ComposeFloor[0], ComposeFloor[1], ComposeFloor[2])}
	}
	if CompareVersion(v, ComposeFloor) < 0 {
		return Result{"docker compose", StatusFail, fmt.Sprintf(
			"%s is below the %d.%d.%d floor required for `!override`",
			raw, ComposeFloor[0], ComposeFloor[1], ComposeFloor[2])}
	}
	return Result{"docker compose", StatusOK, raw}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// CheckSearch reports the search boundary.
//
// Q-001: `off` is not a fault — it is the M0 default. A CONFIGURED but
// unreachable or misconfigured search is a HARD FAIL, because the alternative
// is a silent fallback in which a broken search looks exactly like a working
// one.
func CheckSearch(mode config.SearchMode, reachable bool) Result {
	if mode == config.SearchOff {
		return Result{"search", StatusOK, "off (the M0 default)"}
	}
	if !reachable {
		return Result{"search", StatusFail, "VIZRA_SEARCH_MODE=" + string(mode) +
			" but the service is unreachable or misconfigured; results would be served from SQL " +
			"with readiness degraded"}
	}
	return Result{"search", StatusOK, string(mode) + ", reachable"}
}

// ---------------------------------------------------------------------------
// Version parsing
// ---------------------------------------------------------------------------

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion extracts major.minor.patch from the START of s. Anchored, so a
// string that merely contains digits somewhere does not parse — the callers
// fail closed on !ok and that only works if !ok is reachable.
func ParseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	patch := 0
	if m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	return [3]int{maj, min, patch}, true
}

// CompareVersion returns -1, 0 or 1.
func CompareVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// Counts summarises a run.
type Counts struct{ Total, Failed, Skipped, Warned int }

// Summarise counts the outcomes.
func Summarise(results []Result) Counts {
	c := Counts{Total: len(results)}
	for _, r := range results {
		switch r.Status {
		case StatusFail:
			c.Failed++
		case StatusSkip:
			c.Skipped++
		case StatusWarn:
			c.Warned++
		}
	}
	return c
}

// ExitCode is 1 when any check FAILED, 0 otherwise. A SKIP does not fail the
// run — it was not checked — but it is always reported in the summary so
// "8 OK" can never be printed when three checks never ran.
func ExitCode(results []Result) int {
	if Summarise(results).Failed > 0 {
		return 1
	}
	return 0
}

// Report writes the results and returns a non-nil error when any check FAILED,
// which is what makes the process exit non-zero.
func Report(w io.Writer, results []Result) error {
	for _, r := range results {
		fmt.Fprintf(w, "%-6s %-22s %s\n", r.Status, r.Name, r.Detail)
	}
	c := Summarise(results)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d check(s): %d failed, %d not run.\n", c.Total, c.Failed, c.Skipped)
	if c.Failed > 0 {
		return fmt.Errorf("vizra doctor: %d check(s) failed", c.Failed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Owner claim (VZ-INSTALL-003)
// ---------------------------------------------------------------------------

// OwnerClaimState is what the caller observed about the first-run bootstrap.
// It carries no secret: the generation is an ordinal, never the token or its
// digest, and doctor must never be a way to read a credential back out.
type OwnerClaimState struct {
	// LookupErr is set when the state could not be read at all.
	LookupErr error
	// Claimed reports whether any account exists.
	Claimed bool
	// TokenLive reports whether a redeemable token is currently minted.
	TokenLive bool
	// Generation is the current token's ordinal, 0 when none was ever minted.
	Generation int64
}

// CheckOwnerClaim tells an operator whether they can still get into their own
// instance, and exactly what to run.
//
// An unclaimed instance is NOT a failure — it is the normal state of a freshly
// installed one — so it reports WARN with the command rather than FAIL. What is
// a failure is being unable to read the state at all, because then the operator
// cannot tell an unclaimed instance from a broken one.
func CheckOwnerClaim(st OwnerClaimState) Result {
	const name = "owner claim"
	switch {
	case st.LookupErr != nil:
		return Result{Name: name, Status: StatusFail,
			Detail: "could not read the owner-claim state; the database may be unreachable or the schema may be behind (`vizra migrate`)"}
	case st.Claimed:
		return Result{Name: name, Status: StatusOK, Detail: "this instance has an owner"}
	case st.TokenLive:
		return Result{Name: name, Status: StatusWarn,
			Detail: fmt.Sprintf("unclaimed; a claim token is live (generation %d). "+
				"`vizra claim-token` mints a new one and invalidates it", st.Generation)}
	default:
		return Result{Name: name, Status: StatusWarn,
			Detail: "unclaimed and no token is live; run `vizra claim-token` to mint one"}
	}
}
