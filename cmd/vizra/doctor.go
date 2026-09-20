package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yegamble/vizra-core/internal/cache"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
)

// A doctor result. There are four, and the distinction is the whole point:
//
//	OK   — checked, and it passed.
//	WARN — checked, and it is not fatal but an operator should know.
//	FAIL — checked, and it is broken. Exit code 1.
//	SKIP — NOT CHECKED, with the reason. Never counted as a pass.
//
// A doctor that reports OK for something it could not test is worse than no
// doctor, because it converts an unknown into a false assurance.
type result struct {
	name   string
	status string
	detail string
}

const (
	statusOK   = "OK"
	statusWarn = "WARN"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	envFile := fs.String("env", "", "validate this env file instead of the process environment")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var results []result
	add := func(name, status, detail string) {
		results = append(results, result{name, status, detail})
	}

	// ---- configuration -----------------------------------------------------
	// Validated with the SAME code boot runs (ADR-002 § Configuration
	// ownership): LoadFrom. A doctor with its own rules is a doctor that
	// disagrees with boot.
	var cfg *config.Config
	var cfgErr error
	if *envFile != "" {
		env, err := readEnvFile(*envFile)
		if err != nil {
			add("configuration", statusFail, err.Error())
		} else {
			cfg, cfgErr = config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		}
	} else {
		cfg, cfgErr = config.Load()
	}
	switch {
	case cfg == nil && cfgErr == nil:
		// env file unreadable; already reported.
	case cfgErr != nil:
		ve, _ := config.AsValidationError(cfgErr)
		if ve != nil {
			for _, p := range ve.Problems {
				add("config: "+p.Key, statusFail, p.Message)
			}
		} else {
			add("configuration", statusFail, cfgErr.Error())
		}
	default:
		add("configuration", statusOK, fmt.Sprintf("%d keys validated, mode=%s", len(config.AllKeys()), cfg.Mode))
	}

	// ---- compose version ---------------------------------------------------
	// Q-017: the floor is 2.24.4, refused at runtime, failing CLOSED on an
	// unparseable version string, and the parser must accept majors above 2
	// (Compose is at 5.x; the 2.x line ended at 2.40.3).
	add(composeCheck())

	// Everything below needs a valid configuration.
	if cfg == nil {
		return report(results)
	}

	resolver := site.NewResolver(cfg)

	// ---- database ----------------------------------------------------------
	pools, err := db.Open(ctx, resolver)
	if err != nil {
		add("database", statusFail, "could not open a connection pool: "+err.Error())
	} else {
		defer pools.Close()
		if err := db.Ping(ctx, pools.Default()); err != nil {
			add("database", statusFail, "PostgreSQL is unreachable")
		} else {
			add("database", statusOK, "reachable")
			var version string
			if err := pools.Default().QueryRow(ctx, "SHOW server_version").Scan(&version); err == nil {
				add("database version", statusOK, version)
			}
			embedded, verr := migrate.EmbeddedVersion()
			if verr != nil {
				add("schema", statusFail, verr.Error())
			} else {
				st := migrate.Probe(ctx, pools.Default(), embedded)
				switch st.State {
				case migrate.StateCurrent:
					add("schema", statusOK, fmt.Sprintf("version %d", st.AppliedVersion))
				case migrate.StateBehind:
					add("schema", statusFail, fmt.Sprintf("database at %d, binary embeds %d: %s",
						st.AppliedVersion, st.EmbeddedVersion, st.Detail))
				case migrate.StateAhead:
					add("schema", statusFail, fmt.Sprintf("database at %d is NEWER than this binary's %d: %s",
						st.AppliedVersion, st.EmbeddedVersion, st.Detail))
				case migrate.StateDirty:
					add("schema", statusFail, st.Detail)
				default:
					add("schema", statusSkip, "the migration ledger could not be read")
				}
			}
		}
	}

	// ---- cache -------------------------------------------------------------
	// The flavour and version are printed, not just "up": the licence and the
	// supported command set depend on which server answered (ADR-001 Q-004).
	cc, cerr := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
	if cerr != nil {
		add("cache", statusFail, cerr.Error())
	} else {
		defer func() { _ = cc.Close() }()
		if err := cc.Ping(ctx); err != nil {
			add("cache", statusFail, "unreachable; rate limiting would run on the per-process fallback")
		} else {
			info, ierr := cc.Identify(ctx)
			if ierr != nil {
				add("cache", statusWarn, "reachable, but INFO server did not report a version")
			} else {
				add("cache", statusOK, info.String())
				add(cacheFloorCheck(info))
			}
		}
	}

	// ---- search ------------------------------------------------------------
	// Q-001: a configured but unreachable or misconfigured search is a HARD
	// doctor FAIL. `off` is not a fault.
	if cfg.SearchMode == config.SearchOff {
		add("search", statusOK, "off (the M0 default)")
	} else {
		remote := search.NewRemote(cfg.SearchURL, []byte(cfg.SearchHMACKey), cfg.SearchTimeout)
		svc := search.NewService(search.NewSQL(), remote, nil)
		switch svc.Health(ctx) {
		case search.HealthOK:
			add("search", statusOK, string(cfg.SearchMode)+", reachable")
		default:
			add("search", statusFail,
				"SEARCH_MODE="+string(cfg.SearchMode)+" but the service is unreachable or misconfigured; "+
					"results would be served from SQL with readiness degraded")
		}
	}

	return report(results)
}

// composeVersionRe accepts a major above 2 deliberately: Compose is at 5.x and
// a parser anchored on "2." would reject every current installation.
var composeVersionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// composeFloor is Q-017's documented minimum for `!override`.
var composeFloor = [3]int{2, 24, 4}

func composeCheck() (string, string, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		// Not installed is SKIP, not OK: nothing was verified.
		return "docker compose", statusSkip, "docker is not on PATH, so the Compose floor was not checked"
	}
	out, err := exec.Command("docker", "compose", "version", "--short").Output()
	if err != nil {
		return "docker compose", statusFail, "`docker compose version` failed; the Compose plugin may not be installed"
	}
	raw := strings.TrimSpace(string(out))
	m := composeVersionRe.FindStringSubmatch(raw)
	if m == nil {
		// FAIL CLOSED on an unparseable version string (Q-017).
		return "docker compose", statusFail, fmt.Sprintf("could not parse the Compose version %q; refusing to assume it meets the %d.%d.%d floor",
			raw, composeFloor[0], composeFloor[1], composeFloor[2])
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	if compareVersion([3]int{maj, min, patch}, composeFloor) < 0 {
		return "docker compose", statusFail, fmt.Sprintf("%s is below the %d.%d.%d floor required for `!override`",
			raw, composeFloor[0], composeFloor[1], composeFloor[2])
	}
	return "docker compose", statusOK, raw
}

// cacheFloor is ADR-001's EXTERNAL floor: any RESP-compatible server >= 7.2.
var cacheFloor = [3]int{7, 2, 0}

func cacheFloorCheck(info cache.ServerInfo) (string, string, string) {
	m := composeVersionRe.FindStringSubmatch(info.Version)
	if m == nil {
		return "cache version floor", statusFail,
			fmt.Sprintf("could not parse the server version %q; refusing to assume it meets the 7.2 floor", info.Version)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	if compareVersion([3]int{maj, min, patch}, cacheFloor) < 0 {
		return "cache version floor", statusFail,
			fmt.Sprintf("%s is below the 7.2 command-set floor Vizra requires", info.String())
	}
	return "cache version floor", statusOK, fmt.Sprintf("%s meets the 7.2 floor", info.String())
}

func compareVersion(a, b [3]int) int {
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

func report(results []result) error {
	var fails, skips int
	for _, r := range results {
		fmt.Printf("%-6s %-22s %s\n", r.status, r.name, r.detail)
		switch r.status {
		case statusFail:
			fails++
		case statusSkip:
			skips++
		}
	}
	fmt.Println()
	// The summary states skips explicitly. "8 OK" when three checks never ran
	// is the lie this line exists to prevent.
	fmt.Printf("%d check(s): %d failed, %d not run.\n", len(results), fails, skips)
	if fails > 0 {
		return fmt.Errorf("vizra doctor: %d check(s) failed", fails)
	}
	return nil
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
