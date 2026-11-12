package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/doctor"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/site"
)

// R-1: the verifier deleted the doctor.CheckSchema CALL SITE in this file and
// every lane stayed green — internal/doctor's tests cover the verdicts, and
// nothing covered the WIRING, so the check silently vanished from real
// `vizra doctor` output.
//
// These tests run the real collect() with fakes. Deleting any call site now
// removes a name from the output and turns this red.

// everyCheckDoctorMustReport is the contract: a healthy instance reports all of
// these, in this order. It is written out rather than derived from the code, so
// that removing a call site cannot also remove the expectation.
var everyCheckDoctorMustReport = []string{
	"configuration",
	"env file",
	"docker engine",
	"docker compose",
	"rendered topology",
	"dns/tls",
	"closed ports",
	"backup freshness",
	"database",
	"database version",
	"schema",
	"owner claim",
	"cache",
	"cache version floor",
	"public origin",
	"search",
	"storage root",
	"disk free",
	"api readiness",
}

// notYetCheckable are the ledger checks whose subject does not exist yet. They
// are BLOCKED on every instance, so a healthy instance exits ExitBlocked today.
var notYetCheckable = map[string]bool{"dns/tls": true, "closed ports": true, "backup freshness": true}

type fakePool struct{ pingErr error }

func (f fakePool) Ping(context.Context) error { return f.pingErr }
func (f fakePool) Close()                     {}

type fakeCache struct {
	pingErr error
	info    cache.ServerInfo
}

func (f fakeCache) Ping(context.Context) error { return f.pingErr }
func (f fakeCache) Identify(context.Context) (cache.ServerInfo, error) {
	return f.info, nil
}
func (f fakeCache) Close() error { return nil }

func healthyConfig(t *testing.T) *config.Config {
	t.Helper()
	env := map[string]string{
		"VIZRA_MODE":           "production",
		"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
		"DATABASE_URL":         "postgres://vizra@db:5432/vizra?sslmode=require",
		"VIZRA_CACHE_URL":      "redis://cache:6379/0",
		"VIZRA_SESSION_SECRET": strings.Repeat("Vz9Kp4Mw2Ng7", 3)[:32],
		"VIZRA_MFA_KEY_KEK":    strings.Repeat("Vz9Kp4Mw2Ng7", 3)[:32],
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("the healthy fixture config does not load: %v", err)
	}
	return cfg
}

func healthyProbes(t *testing.T) probes {
	t.Helper()
	cfg := healthyConfig(t)
	return probes{
		loadConfig: func() (*config.Config, error) { return cfg, nil },
		compose: func() doctor.Result {
			return doctor.Result{Name: "docker compose", Status: doctor.StatusOK, Detail: "2.40.3"}
		},
		openPools: func(context.Context, *site.Resolver) (pooler, error) { return fakePool{}, nil },
		serverVersion: func(context.Context, pooler) (string, error) {
			return "18.6 (Debian)", nil
		},
		schema: func(context.Context, pooler) (migrate.Status, error) {
			return migrate.Status{State: migrate.StateCurrent, AppliedVersion: 4, EmbeddedVersion: 4}, nil
		},
		openCache: func(*config.Config) (cacher, error) {
			return fakeCache{info: cache.ServerInfo{Flavour: cache.FlavourValkey, Version: "9.1.2"}}, nil
		},
		searchReachable: func(context.Context, *config.Config) bool { return false },
		ownerClaim: func(context.Context, pooler) doctor.OwnerClaimState {
			return doctor.OwnerClaimState{Claimed: true}
		},
		envFile:      func() doctor.Result { return doctor.CheckEnvFile("prod.env", nil, nil) },
		dockerEngine: func(context.Context) doctor.Result { return doctor.CheckDockerEngine("29.9.2", nil, nil) },
		topology: func(context.Context) doctor.Result {
			return doctor.CheckTopology(doctor.TopologyInput{EnvFile: "prod.env", ComposeFile: "docker-compose.yml"})
		},
		disk:     func(*config.Config) doctor.Result { return doctor.CheckDisk("/data", 50<<30, nil) },
		storage:  func(*config.Config) doctor.Result { return doctor.CheckStorage("/data", nil) },
		apiReady: func(context.Context) doctor.Result { return doctor.CheckAPIReadiness(0, "ready") },
	}
}

func names(results []doctor.Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Name
	}
	return out
}

// The R-1 killer: every check the doctor is supposed to report must actually
// appear in the output of the real collect().
func TestDoctorReportsEveryCheck(t *testing.T) {
	got := names(collect(context.Background(), healthyProbes(t)))
	joined := strings.Join(got, ", ")

	for _, want := range everyCheckDoctorMustReport {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("`vizra doctor` does not report %q.\n"+
				"Its call site in cmd/vizra/doctor.go is missing, so the check has silently "+
				"vanished from what an operator sees — whatever internal/doctor's own tests say.\n"+
				"reported: %s", want, joined)
		}
	}
	if t.Failed() {
		return
	}
	if len(got) != len(everyCheckDoctorMustReport) {
		t.Errorf("doctor reported %d check(s), the contract names %d: %s",
			len(got), len(everyCheckDoctorMustReport), joined)
	}
}

// On a healthy instance every check that CAN run reads OK; the three whose
// subject does not exist yet read BLOCKED, so the run exits ExitBlocked — never
// 0, which would claim DNS/TLS, ports and backups were verified.
func TestDoctorOnAHealthyInstanceIsOKExceptTheUncheckable(t *testing.T) {
	results := collect(context.Background(), healthyProbes(t))
	for _, r := range results {
		want := doctor.StatusOK
		if notYetCheckable[r.Name] {
			want = doctor.StatusBlocked
		}
		if r.Status != want {
			t.Errorf("%s = %s (%s) on a healthy instance, want %s", r.Name, r.Status, r.Detail, want)
		}
	}
	if code := doctor.ExitCode(results); code != doctor.ExitBlocked {
		t.Fatalf("exit code %d on a healthy instance, want ExitBlocked (%d)", code, doctor.ExitBlocked)
	}
}

// A host check that cannot run reports BLOCKED and the run exits 3, not 0.
func TestDoctorHostCheckThatCannotRunIsBlocked(t *testing.T) {
	p := healthyProbes(t)
	p.dockerEngine = func(context.Context) doctor.Result {
		return doctor.CheckDockerEngine("", errors.New("docker: not found"), nil)
	}
	results := collect(context.Background(), p)
	for _, r := range results {
		if r.Name == "docker engine" && r.Status != doctor.StatusBlocked {
			t.Fatalf("docker engine = %s, want BLOCKED", r.Status)
		}
	}
	if code := doctor.ExitCode(results); code != doctor.ExitBlocked {
		t.Fatalf("exit %d, want ExitBlocked", code)
	}
}

// Each of these is a real defect an operator runs doctor to find. The point is
// the exit code: `vizra doctor && deploy` must not proceed.
func TestDoctorFailsAndExitsNonZeroOnEachRealDefect(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*probes)
		wantBad string
	}{
		{
			name: "schema behind",
			mutate: func(p *probes) {
				p.schema = func(context.Context, pooler) (migrate.Status, error) {
					return migrate.Status{State: migrate.StateBehind, AppliedVersion: 2, EmbeddedVersion: 4,
						Detail: "run `vizra migrate`"}, nil
				}
			},
			wantBad: "schema",
		},
		{
			name: "schema dirty",
			mutate: func(p *probes) {
				p.schema = func(context.Context, pooler) (migrate.Status, error) {
					return migrate.Status{State: migrate.StateDirty, Detail: "resolve it deliberately"}, nil
				}
			},
			wantBad: "schema",
		},
		{
			name: "database unreachable",
			mutate: func(p *probes) {
				p.openPools = func(context.Context, *site.Resolver) (pooler, error) {
					return fakePool{pingErr: errors.New("connection refused")}, nil
				}
			},
			wantBad: "database",
		},
		{
			name: "cache below the 7.2 floor",
			mutate: func(p *probes) {
				p.openCache = func(*config.Config) (cacher, error) {
					return fakeCache{info: cache.ServerInfo{Flavour: cache.FlavourRedis, Version: "6.2.0"}}, nil
				}
			},
			wantBad: "cache version floor",
		},
		{
			name: "cache unreachable",
			mutate: func(p *probes) {
				p.openCache = func(*config.Config) (cacher, error) {
					return fakeCache{pingErr: errors.New("connection refused")}, nil
				}
			},
			wantBad: "cache",
		},
		{
			name: "invalid configuration",
			mutate: func(p *probes) {
				p.loadConfig = func() (*config.Config, error) {
					bad := map[string]string{
						"VIZRA_MODE":           "production",
						"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
						"DATABASE_URL":         "postgres://vizra@db:5432/vizra",
						"VIZRA_CACHE_URL":      "redis://cache:6379/0",
						"VIZRA_SESSION_SECRET": "changeme",
					}
					return config.LoadFrom(func(k string) (string, bool) { v, ok := bad[k]; return v, ok })
				}
			},
			wantBad: "config: VIZRA_SESSION_SECRET",
		},
		{
			name: "env file fails setup --check",
			mutate: func(p *probes) {
				p.envFile = func() doctor.Result { return doctor.CheckEnvFile("prod.env", nil, []string{"COMPOSE_FILE: mismatch"}) }
			},
			wantBad: "env file",
		},
		{
			name: "docker daemon down",
			mutate: func(p *probes) {
				p.dockerEngine = func(context.Context) doctor.Result { return doctor.CheckDockerEngine("", nil, errors.New("exit 1")) }
			},
			wantBad: "docker engine",
		},
		{
			name: "compose chain does not render",
			mutate: func(p *probes) {
				p.topology = func(context.Context) doctor.Result {
					return doctor.CheckTopology(doctor.TopologyInput{EnvFile: "prod.env", ComposeFile: "x.yml", RunErr: errors.New("exit 1"), Stderr: "no such file"})
				}
			},
			wantBad: "rendered topology",
		},
		{
			name: "disk nearly full",
			mutate: func(p *probes) {
				p.disk = func(*config.Config) doctor.Result { return doctor.CheckDisk("/data", 10<<20, nil) }
			},
			wantBad: "disk free",
		},
		{
			name: "storage root read-only",
			mutate: func(p *probes) {
				p.storage = func(*config.Config) doctor.Result {
					return doctor.CheckStorage("/data", errors.New("read-only file system"))
				}
			},
			wantBad: "storage root",
		},
		{
			name: "api not ready",
			mutate: func(p *probes) {
				p.apiReady = func(context.Context) doctor.Result { return doctor.CheckAPIReadiness(1, "HTTP 503") }
			},
			wantBad: "api readiness",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyProbes(t)
			tc.mutate(&p)
			results := collect(context.Background(), p)

			var found bool
			for _, r := range results {
				if r.Name == tc.wantBad {
					found = true
					if r.Status != doctor.StatusFail {
						t.Errorf("%s = %s (%s), want FAIL", r.Name, r.Status, r.Detail)
					}
				}
			}
			if !found {
				t.Fatalf("no check named %q in %v", tc.wantBad, names(results))
			}
			if code := doctor.ExitCode(results); code != 1 {
				t.Fatalf("exit code %d; `vizra doctor && deploy` would proceed past a real defect", code)
			}
		})
	}
}

// A configuration that will not load stops the run early — there is nothing to
// connect to — but it must still FAIL rather than report a short green run.
func TestDoctorStopsEarlyOnAnUnloadableConfigButStillFails(t *testing.T) {
	p := healthyProbes(t)
	p.loadConfig = func() (*config.Config, error) { return nil, errors.New("could not read /etc/vizra/.env") }
	results := collect(context.Background(), p)

	if doctor.ExitCode(results) != 1 {
		t.Fatal("an unreadable env file exited 0")
	}
	// It must not pretend to have checked the database.
	for _, r := range results {
		if r.Name == "database" || r.Name == "schema" {
			t.Errorf("doctor reported %q without a configuration to connect with", r.Name)
		}
	}
}

// TestRealProbesWiresEveryCheck is the converse of collect()'s nil guards: the
// guards keep a test fake from panicking, and this keeps that leniency from
// hiding a production check that nobody wired.
func TestRealProbesWiresEveryCheck(t *testing.T) {
	p := realProbes("")
	if p.ownerClaim == nil {
		t.Error("realProbes does not wire ownerClaim, so `vizra doctor` would SKIP the owner-claim check in production")
	}
	if p.dockerEngine == nil || p.envFile == nil || p.topology == nil || p.disk == nil || p.storage == nil || p.apiReady == nil {
		t.Error("realProbes left a VZ-OPS-002 host probe unwired")
	}
	if p.loadConfig == nil || p.openPools == nil || p.schema == nil || p.openCache == nil || p.searchReachable == nil {
		t.Error("realProbes left a probe unwired")
	}
}

// R2-G(d): `vizra doctor --env F` validated F's configuration but compared the
// PROCESS environment's VIZRA_PUBLIC_ORIGIN against it, so the origin check
// judged a value the operator never wrote into F — or reported an empty one.
// Both halves of that check must come from the same source.
func TestDoctorReadsTheRawPublicOriginFromTheSameSourceAsTheConfig(t *testing.T) {
	t.Setenv("VIZRA_PUBLIC_ORIGIN", "https://process.invalid")

	file := t.TempDir() + "/vizra.env"
	if err := os.WriteFile(file, []byte("VIZRA_PUBLIC_ORIGIN=https://Photos.Example.org/\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := realProbes(file)
	if p.rawPublicOrigin == nil {
		t.Fatal("realProbes does not wire rawPublicOrigin")
	}
	if got, want := p.rawPublicOrigin(), "https://Photos.Example.org/"; got != want {
		t.Errorf("--env: raw public origin = %q, want the env file's %q", got, want)
	}

	if got, want := realProbes("").rawPublicOrigin(), "https://process.invalid"; got != want {
		t.Errorf("no --env: raw public origin = %q, want the process environment's %q", got, want)
	}
}
