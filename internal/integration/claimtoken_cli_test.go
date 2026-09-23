//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `vizra claim-token` as a REAL BINARY.
//
// The first round had zero coverage of cmd/vizra: the CLI refusal test called
// the LIBRARY, so flipping `refuseIfUsersExist` to false in the CLI — the S-12
// escalation path, where the command manufactures a live owner-creating
// credential on a running claimed instance — left the whole suite green. A
// library test cannot observe the flag the command passes.
//
// Everything below drives the shipped binary as a separate process and asserts
// what an operator actually sees: the exit code, which stream carries what, and
// whether a credential ever reaches the structured logger.

var hex64Line = regexp.MustCompile(`(?m)^[0-9a-f]{64}$`)

type cliResult struct {
	code   int
	stdout string
	stderr string
}

// runClaimToken executes the shipped binary against the given DSN.
func runClaimToken(t *testing.T, dsn string, extraEnv ...string) cliResult {
	t.Helper()
	cmd := exec.Command(filepath.Join(binaries(t), "vizra"), "claim-token")
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"VIZRA_MODE=development",
		"VIZRA_PUBLIC_ORIGIN=http://localhost:8080",
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()

	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running vizra claim-token: %v", err)
		}
		code = ee.ExitCode()
	}
	r := cliResult{code: code, stdout: out.String(), stderr: errb.String()}
	// Never log the token itself; log its shape.
	t.Logf("vizra claim-token -> exit %d, stdout %d bytes (%d 64-hex lines), stderr %d bytes",
		r.code, len(r.stdout), len(hex64Line.FindAllString(r.stdout, -1)), len(r.stderr))
	return r
}

func (r cliResult) token(t *testing.T) string {
	t.Helper()
	lines := hex64Line.FindAllString(r.stdout, -1)
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d 64-hex lines, want exactly 1", len(lines))
	}
	return lines[0]
}

// TestClaimTokenCLIOnAnUnclaimedInstance: the documented primary path.
func TestClaimTokenCLIOnAnUnclaimedInstance(t *testing.T) {
	e := newClaimEnv(t)

	r := runClaimToken(t, e.cfg.DatabaseURL)
	if r.code != 0 {
		t.Fatalf("exit %d on an unclaimed instance, want 0. stderr=%s", r.code, r.stderr)
	}

	// stdout is the token ALONE, so `vizra claim-token | pbcopy` carries only it.
	if strings.TrimSpace(r.stdout) != r.token(t) {
		t.Fatalf("stdout is not the token alone: %d bytes", len(r.stdout))
	}
	// The explanation, including the generation, goes to stderr.
	if !strings.Contains(r.stderr, "generation 1") {
		t.Errorf("stderr does not name the generation: %q", r.stderr)
	}
	if hex64Line.MatchString(r.stderr) {
		t.Error("stderr repeated the token; it belongs on stdout only")
	}
	// Nothing went through the structured logger.
	for _, stream := range []struct{ name, body string }{{"stdout", r.stdout}, {"stderr", r.stderr}} {
		if strings.Contains(stream.body, `"level":`) || strings.Contains(stream.body, "level=") {
			t.Errorf("%s carries structured-logger output: %q", stream.name, stream.body)
		}
	}
	// And the token the operator was handed actually works.
	code, body := e.post(t, validBody(r.token(t)), nil)
	if code != 201 {
		t.Fatalf("the CLI's token was refused by the API: %d %v", code, body)
	}
}

// TestClaimTokenCLIRemintSupersedesThePrevious: re-minting is the recovery path,
// so the previous token must stop working.
func TestClaimTokenCLIRemintSupersedesThePrevious(t *testing.T) {
	e := newClaimEnv(t)

	first := runClaimToken(t, e.cfg.DatabaseURL).token(t)
	second := runClaimToken(t, e.cfg.DatabaseURL)
	if second.code != 0 {
		t.Fatalf("the second mint exited %d, want 0", second.code)
	}
	if !strings.Contains(second.stderr, "generation 2") {
		t.Errorf("the second mint does not name generation 2: %q", second.stderr)
	}
	if second.token(t) == first {
		t.Fatal("a re-mint returned the same token")
	}

	// The old token is now refused by the API, with the single uniform message.
	code, body := e.post(t, validBody(first), nil)
	if code != 403 {
		t.Fatalf("the superseded token = %d, want 403. body=%v", code, body)
	}
	if bodyCode(body) != "forbidden" {
		t.Errorf("code = %q, want forbidden", bodyCode(body))
	}
	// The new one works.
	if code, _ := e.post(t, validBody(second.token(t)), nil); code != 201 {
		t.Fatalf("the current token = %d, want 201", code)
	}
}

// TestClaimTokenCLIRefusesOnAClaimedInstanceAsABinary (verifier FINDING 2).
//
// THIS is the test that catches the S-12 escalation path. Flipping
// refuseIfUsersExist in cmd/vizra left the previous suite green because the only
// coverage called the library directly.
func TestClaimTokenCLIRefusesOnAClaimedInstanceAsABinary(t *testing.T) {
	e := newClaimEnv(t)
	token, _ := e.mint(t)
	if code, _ := e.post(t, validBody(token), nil); code != 201 {
		t.Fatal("setup claim failed")
	}
	genBefore := e.count(t, `SELECT generation FROM owner_claim_tokens`)

	r := runClaimToken(t, e.cfg.DatabaseURL)
	if r.code == 0 {
		t.Fatalf("vizra claim-token exited 0 on a CLAIMED instance — it minted an "+
			"owner-creating credential on a running system. stdout=%q", r.stdout)
	}
	if n := len(hex64Line.FindAllString(r.stdout+r.stderr, -1)); n != 0 {
		t.Fatalf("a claimed instance produced %d token-shaped lines, want 0", n)
	}
	if !strings.Contains(r.stderr, "already claimed") {
		t.Errorf("the refusal does not say why: %q", r.stderr)
	}
	if got := e.count(t, `SELECT generation FROM owner_claim_tokens`); got != genBefore {
		t.Fatalf("generation moved %d -> %d; the refusal must mint nothing", genBefore, got)
	}
	if got := e.count(t, `SELECT count(*) FROM owner_claim_tokens WHERE consumed_at IS NULL`); got != 0 {
		t.Fatalf("%d live token rows after a refused mint, want 0", got)
	}
}

// TestClaimTokenCLIWithAnUnreachableDatabase: a failure must be legible and must
// not print the DSN, which carries the database password.
func TestClaimTokenCLIWithAnUnreachableDatabase(t *testing.T) {
	const dsn = "postgres://vizra:sekrit@127.0.0.1:1/vizra_test?sslmode=disable&connect_timeout=1"
	r := runClaimToken(t, dsn)

	if r.code == 0 {
		t.Fatalf("exit 0 against an unreachable database; stdout=%q", r.stdout)
	}
	if n := len(hex64Line.FindAllString(r.stdout+r.stderr, -1)); n != 0 {
		t.Fatalf("%d token-shaped lines on a failure path, want 0", n)
	}
	both := r.stdout + r.stderr
	for _, secret := range []string{"sekrit", dsn} {
		if strings.Contains(both, secret) {
			t.Errorf("the output carries the DSN or its password: %q", both)
		}
	}
	if strings.TrimSpace(both) == "" {
		t.Error("a failure with no message leaves the operator nothing to act on")
	}
}

// TestVizraUsageNamesClaimToken: the command has to be discoverable, because it
// is the documented primary way to obtain a token.
func TestVizraUsageNamesClaimToken(t *testing.T) {
	cmd := exec.Command(filepath.Join(binaries(t), "vizra"), "help")
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("vizra help: %v", err)
	}
	if !strings.Contains(out.String(), "claim-token") {
		t.Fatalf("`vizra help` does not mention claim-token:\n%s", out.String())
	}
	for _, other := range []string{"doctor", "migrate", "healthcheck"} {
		if !strings.Contains(out.String(), other) {
			t.Errorf("`vizra help` lost %q", other)
		}
	}
	_ = fmt.Sprint()
}
