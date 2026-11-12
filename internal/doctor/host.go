package doctor

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Host checks (VZ-OPS-002). Each verdict here is pure; cmd/vizra does the
// exec/stat/dial and passes the outcome in, so every verdict has a failing
// fixture in host_test.go.

// CheckDockerEngine evaluates `docker version --format {{.Server.Version}}`.
// No engine floor is documented (only the Compose floor is), so this verifies
// the daemon answers with a parseable version.
func CheckDockerEngine(raw string, lookErr, runErr error) Result {
	const name = "docker engine"
	if lookErr != nil {
		return Result{name, StatusBlocked, "docker is not on PATH, so the engine was not checked"}
	}
	if runErr != nil {
		return Result{name, StatusFail, "the Docker daemon did not answer `docker version`"}
	}
	raw = strings.TrimSpace(raw)
	if _, ok := ParseVersion(raw); !ok {
		return Result{name, StatusFail, fmt.Sprintf("could not parse the engine version %q", raw)}
	}
	return Result{name, StatusOK, raw}
}

// CheckEnvFile reports `vizra setup --check` on the env file. problems is
// setup.Check's output, so doctor and setup can never disagree.
func CheckEnvFile(path string, readErr error, problems []string) Result {
	const name = "env file"
	if path == "" {
		return Result{name, StatusBlocked, "no --env file given; run `vizra doctor --env env/production.env`"}
	}
	if readErr != nil {
		return Result{name, StatusFail, readErr.Error()}
	}
	if len(problems) > 0 {
		return Result{name, StatusFail, fmt.Sprintf("%s: %d problem(s): %s", path, len(problems), strings.Join(problems, "; "))}
	}
	return Result{name, StatusOK, path + " passes `vizra setup --check`"}
}

// TopologyInput is the outcome of rendering the env-derived compose chain.
type TopologyInput struct {
	EnvFile     string // the --env file the chain is read from
	ComposeFile string // its COMPOSE_FILE
	Profiles    string // its COMPOSE_PROFILES
	LookErr     error  // docker not on PATH
	RunErr      error  // `docker compose config --quiet` failed
	Stderr      string
}

// CheckTopology reports whether the chain `vizra setup` wrote renders. Bare
// `docker compose` without that chain renders a different stack, so doctor
// never falls back to it.
func CheckTopology(in TopologyInput) Result {
	const name = "rendered topology"
	switch {
	case in.EnvFile == "":
		return Result{name, StatusBlocked, "no --env file given, so the COMPOSE_FILE/COMPOSE_PROFILES chain is unknown"}
	case in.ComposeFile == "":
		return Result{name, StatusBlocked, in.EnvFile + " has no COMPOSE_FILE; run `vizra setup`"}
	case in.LookErr != nil:
		return Result{name, StatusBlocked, "docker is not on PATH, so the chain was not rendered"}
	case in.RunErr != nil:
		msg := strings.TrimSpace(in.Stderr)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return Result{name, StatusFail, "`docker compose config` rejected the chain: " + msg}
	}
	return Result{name, StatusOK, fmt.Sprintf("COMPOSE_FILE=%s COMPOSE_PROFILES=%s", in.ComposeFile, in.Profiles)}
}

// DiskFloorBytes is doctor's own free-space threshold (1 GiB): below it image
// pulls, migrations and uploads start failing.
const DiskFloorBytes uint64 = 1 << 30

// CheckDisk evaluates free space at path. A statfs failure is BLOCKED: the
// space was not measured.
func CheckDisk(path string, free uint64, err error) Result {
	const name = "disk free"
	if err != nil {
		return Result{name, StatusBlocked, fmt.Sprintf("could not measure free space at %s: %v", path, err)}
	}
	gib := float64(free) / (1 << 30)
	if free < DiskFloorBytes {
		return Result{name, StatusFail, fmt.Sprintf("%s has %.2f GiB free, below the 1 GiB floor", path, gib)}
	}
	return Result{name, StatusOK, fmt.Sprintf("%s has %.1f GiB free", path, gib)}
}

// ProbeWritable creates and removes a file in dir: the only honest test of
// "writable" (mode bits lie under ACLs, read-only mounts and user namespaces).
func ProbeWritable(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	f, err := os.CreateTemp(dir, ".vizra-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// CheckStorage reports the storage root probe.
func CheckStorage(root string, probeErr error) Result {
	const name = "storage root"
	if root == "" {
		return Result{name, StatusBlocked, "no VIZRA_STORAGE_ROOT is configured"}
	}
	if probeErr != nil {
		return Result{name, StatusFail, fmt.Sprintf("%s is not writable: %v", root, probeErr)}
	}
	return Result{name, StatusOK, root + " is writable"}
}

// CheckAPIReadiness maps `vizra healthcheck api`'s exit code: 0 ready, 1 not
// ready, anything else (64 usage) means the probe could not run.
func CheckAPIReadiness(code int, msg string) Result {
	const name = "api readiness"
	msg = strings.TrimSpace(msg)
	switch code {
	case 0:
		return Result{name, StatusOK, msg}
	case 1:
		return Result{name, StatusFail, msg}
	}
	return Result{name, StatusBlocked, fmt.Sprintf("the readiness probe could not run (exit %d): %s", code, msg)}
}

// NotYetCheckable are ledger checks whose subject does not exist in this
// release. They are reported BLOCKED so that an operator — and the exit code —
// can see they were not verified.
func NotYetCheckable() []Result {
	return []Result{
		{"dns/tls", StatusBlocked, "no DNS/TLS probe yet: the edge certificate path is not wired into doctor (VZ-OPS-002 follow-up)"},
		{"closed ports", StatusBlocked, "no safe external port probe exists yet; doctor will not scan from the host and call it proof"},
		{"backup freshness", StatusBlocked, "no backup tool exists yet (backup/restore arrive with a later slice), so there is no backup to date"},
	}
}
