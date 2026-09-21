package main

import (
	"context"
	"errors"
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
	"docker compose",
	"database",
	"database version",
	"schema",
	"owner claim",
	"public origin",
	"cache",
	"cache version floor",
	"search",
}

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

// A healthy instance exits 0, and every check reads OK. (Search is `off`, which
// is the M0 default and an OK, not a fault.)
func TestDoctorOnAHealthyInstanceExitsZero(t *testing.T) {
	results := collect(context.Background(), healthyProbes(t))
	for _, r := range results {
		if r.Status != doctor.StatusOK {
			t.Errorf("%s = %s (%s) on a healthy instance", r.Name, r.Status, r.Detail)
		}
	}
	if code := doctor.ExitCode(results); code != 0 {
		t.Fatalf("exit code %d on a healthy instance", code)
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
	if p.loadConfig == nil || p.openPools == nil || p.schema == nil || p.openCache == nil || p.searchReachable == nil {
		t.Error("realProbes left a probe unwired")
	}
}
