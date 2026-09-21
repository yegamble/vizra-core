package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/doctor"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// This file is deliberately thin: it does the I/O — opening pools, running
// `docker compose version`, reading INFO — and every VERDICT comes from
// internal/doctor, which is tested.
//
// The split exists because the verdicts used to live here, cmd/ has no test
// files, and three independent mutants survived the whole gate: the
// schema-drift check reporting OK, the cache floor check deleted, and an
// invalid configuration reporting OK.

// probes are the I/O collect() performs. They exist as fields so a test can run
// the REAL collect() — the same function `vizra doctor` runs — against fakes,
// and assert that every check appears in its output.
//
// Without that, deleting a call site here left every lane green while the check
// silently vanished from an operator's doctor run. A check that can disappear
// without a test noticing is a check that is not there.
type probes struct {
	loadConfig      func() (*config.Config, error)
	compose         func() doctor.Result
	openPools       func(context.Context, *site.Resolver) (pooler, error)
	serverVersion   func(context.Context, pooler) (string, error)
	schema          func(context.Context, pooler) (migrate.Status, error)
	openCache       func(*config.Config) (cacher, error)
	searchReachable func(context.Context, *config.Config) bool
	ownerClaim      func(context.Context, pooler) doctor.OwnerClaimState
}

// pooler and cacher are the narrow views collect() needs, so a fake is three
// methods rather than a database.
type pooler interface {
	Ping(context.Context) error
	Close()
}

type cacher interface {
	Ping(context.Context) error
	Identify(context.Context) (cache.ServerInfo, error)
	Close() error
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	envFile := fs.String("env", "", "validate this env file instead of the process environment")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := collect(ctx, realProbes(*envFile))
	return doctor.Report(os.Stdout, results)
}

// realProbes is the production wiring.
func realProbes(envFile string) probes {
	return probes{
		loadConfig: func() (*config.Config, error) {
			if envFile == "" {
				return config.Load()
			}
			env, err := readEnvFile(envFile)
			if err != nil {
				return nil, err
			}
			return config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		},
		compose: composeCheck,
		openPools: func(ctx context.Context, r *site.Resolver) (pooler, error) {
			p, err := db.Open(ctx, r)
			if err != nil {
				return nil, err
			}
			return poolAdapter{p}, nil
		},
		serverVersion: func(ctx context.Context, p pooler) (string, error) {
			var v string
			err := p.(poolAdapter).pools.Default().QueryRow(ctx, "SHOW server_version").Scan(&v)
			return v, err
		},
		schema: func(ctx context.Context, p pooler) (migrate.Status, error) {
			embedded, err := migrate.EmbeddedVersion()
			if err != nil {
				return migrate.Status{}, err
			}
			return migrate.Probe(ctx, p.(poolAdapter).pools.Default(), embedded), nil
		},
		openCache: func(cfg *config.Config) (cacher, error) {
			c, err := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		ownerClaim: realOwnerClaim,
		searchReachable: func(ctx context.Context, cfg *config.Config) bool {
			if cfg.SearchMode == config.SearchOff {
				return false
			}
			remote := search.NewRemote(cfg.SearchURL, []byte(cfg.SearchHMACKey), cfg.SearchTimeout)
			return search.NewService(search.NewSQL(), remote, nil).Health(ctx) == search.HealthOK
		},
	}
}

// ownerClaim reads the first-run state. It is a REAL check: it queries the
// database, and reports FAIL when it cannot — doctor never reports OK for
// something it did not test.
func realOwnerClaim(ctx context.Context, pool pooler) doctor.OwnerClaimState {
	raw, ok := pool.(poolAdapter)
	if !ok {
		return doctor.OwnerClaimState{LookupErr: errors.New("no database pool")}
	}
	q := sqlcgen.New(raw.pools.Default())
	claimed, err := q.AnyUserExists(ctx)
	if err != nil {
		return doctor.OwnerClaimState{LookupErr: err}
	}
	state, err := ownerclaim.State(ctx, q)
	if err != nil {
		return doctor.OwnerClaimState{LookupErr: err}
	}
	return doctor.OwnerClaimState{
		Claimed:    claimed,
		TokenLive:  state.Live,
		Generation: state.Generation,
	}
}

type poolAdapter struct{ pools *db.Pools }

func (a poolAdapter) Ping(ctx context.Context) error { return db.Ping(ctx, a.pools.Default()) }
func (a poolAdapter) Close()                         { a.pools.Close() }

func collect(ctx context.Context, p probes) []doctor.Result {
	var results []doctor.Result

	// --- configuration ------------------------------------------------------
	// Validated with the SAME code boot runs (ADR-002 § Configuration
	// ownership): config.LoadFrom. A doctor with its own rules is a doctor that
	// disagrees with boot.
	cfg, cfgErr := p.loadConfig()
	results = append(results, doctor.CheckConfig(cfg, cfgErr)...)

	// --- compose ------------------------------------------------------------
	results = append(results, p.compose())

	// Everything below needs a valid configuration.
	if cfg == nil {
		return results
	}
	resolver := site.NewResolver(cfg)

	// --- database and schema ------------------------------------------------
	pool, err := p.openPools(ctx, resolver)
	if err != nil {
		results = append(results, doctor.Result{
			Name: "database", Status: doctor.StatusFail,
			Detail: "could not open a connection pool: " + err.Error(),
		})
	} else {
		defer pool.Close()
		pingErr := pool.Ping(ctx)
		results = append(results, doctor.CheckDatabase(pingErr))
		if pingErr == nil {
			if v, verr := p.serverVersion(ctx, pool); verr == nil {
				results = append(results, doctor.Result{Name: "database version", Status: doctor.StatusOK, Detail: v})
			}
			st, serr := p.schema(ctx, pool)
			if serr != nil {
				results = append(results, doctor.Result{Name: "schema", Status: doctor.StatusFail, Detail: serr.Error()})
			} else {
				results = append(results, doctor.CheckSchema(st))
			}
			// A probe a test did not wire reports SKIP rather than OK: doctor
			// never claims to have checked something it did not.
			// TestRealProbesWiresEveryCheck asserts production wires this one.
			if p.ownerClaim == nil {
				results = append(results, doctor.Result{Name: "owner claim",
					Status: doctor.StatusSkip, Detail: "no owner-claim probe was configured"})
			} else {
				results = append(results, doctor.CheckOwnerClaim(p.ownerClaim(ctx, pool)))
			}
		}
	}

	// --- cache --------------------------------------------------------------
	cc, cerr := p.openCache(cfg)
	if cerr != nil {
		results = append(results, doctor.Result{Name: "cache", Status: doctor.StatusFail, Detail: cerr.Error()})
	} else {
		defer func() { _ = cc.Close() }()
		pingErr := cc.Ping(ctx)
		var info cache.ServerInfo
		var idErr error
		if pingErr == nil {
			info, idErr = cc.Identify(ctx)
		}
		results = append(results, doctor.CheckCache(info, pingErr, idErr)...)
	}

	// --- search -------------------------------------------------------------
	results = append(results, doctor.CheckSearch(cfg.SearchMode, p.searchReachable(ctx, cfg)))

	return results
}

// composeCheck performs the I/O; doctor.CheckCompose decides.
func composeCheck() doctor.Result {
	if _, err := exec.LookPath("docker"); err != nil {
		return doctor.CheckCompose("", err, nil)
	}
	out, err := exec.Command("docker", "compose", "version", "--short").Output()
	return doctor.CheckCompose(string(out), nil, err)
}

// readEnvFile parses a KEY=VALUE file the way an operator writes one. It is
// deliberately strict about nothing except structure: every semantic rule lives
// in config.LoadFrom.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	defer f.Close()

	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, sc.Err()
}
