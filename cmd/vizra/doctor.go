package main

import (
	"bufio"
	"context"
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
	"github.com/yegamble/vizra-core/internal/search"
	"github.com/yegamble/vizra-core/internal/site"
)

// This file is deliberately thin: it does the I/O — opening pools, running
// `docker compose version`, reading INFO — and every VERDICT comes from
// internal/doctor, which is tested.
//
// The split exists because the verdicts used to live here, cmd/ has no test
// files, and three independent mutants survived the whole gate: the
// schema-drift check reporting OK, the cache floor check deleted, and an
// invalid configuration reporting OK.

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	envFile := fs.String("env", "", "validate this env file instead of the process environment")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := collect(ctx, *envFile)
	return doctor.Report(os.Stdout, results)
}

func collect(ctx context.Context, envFile string) []doctor.Result {
	var results []doctor.Result

	// --- configuration ------------------------------------------------------
	// Validated with the SAME code boot runs (ADR-002 § Configuration
	// ownership): config.LoadFrom. A doctor with its own rules is a doctor that
	// disagrees with boot.
	var cfg *config.Config
	var cfgErr error
	if envFile != "" {
		env, err := readEnvFile(envFile)
		if err != nil {
			return append(results, doctor.Result{Name: "configuration", Status: doctor.StatusFail, Detail: err.Error()})
		}
		cfg, cfgErr = config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	} else {
		cfg, cfgErr = config.Load()
	}
	results = append(results, doctor.CheckConfig(cfg, cfgErr)...)

	// --- compose ------------------------------------------------------------
	results = append(results, composeCheck())

	// Everything below needs a valid configuration.
	if cfg == nil {
		return results
	}
	resolver := site.NewResolver(cfg)

	// --- database and schema ------------------------------------------------
	pools, err := db.Open(ctx, resolver)
	if err != nil {
		results = append(results, doctor.Result{
			Name: "database", Status: doctor.StatusFail,
			Detail: "could not open a connection pool: " + err.Error(),
		})
	} else {
		defer pools.Close()
		pingErr := db.Ping(ctx, pools.Default())
		results = append(results, doctor.CheckDatabase(pingErr))
		if pingErr == nil {
			var version string
			if err := pools.Default().QueryRow(ctx, "SHOW server_version").Scan(&version); err == nil {
				results = append(results, doctor.Result{Name: "database version", Status: doctor.StatusOK, Detail: version})
			}
			embedded, verr := migrate.EmbeddedVersion()
			if verr != nil {
				results = append(results, doctor.Result{Name: "schema", Status: doctor.StatusFail, Detail: verr.Error()})
			} else {
				results = append(results, doctor.CheckSchema(migrate.Probe(ctx, pools.Default(), embedded)))
			}
		}
	}

	// --- cache --------------------------------------------------------------
	cc, cerr := cache.Open(cfg.CacheURL, cfg.CacheNamespace)
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
	reachable := false
	if cfg.SearchMode != config.SearchOff {
		remote := search.NewRemote(cfg.SearchURL, []byte(cfg.SearchHMACKey), cfg.SearchTimeout)
		svc := search.NewService(search.NewSQL(), remote, nil)
		reachable = svc.Health(ctx) == search.HealthOK
	}
	results = append(results, doctor.CheckSearch(cfg.SearchMode, reachable))

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
