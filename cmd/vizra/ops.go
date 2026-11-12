package main

// Day-2 operator commands (VZ-OPS-003). Every compose call goes through
// ops.Chain.Argv — the env-derived -f/--profile chain plus --env-file — and
// through the opsEnv.run seam, so ops_test.go asserts the exact argv the real
// command functions would exec. Bare `docker compose` on a production host is
// wrong: it drops the prod and external overlays, every profile and the env file.

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yegamble/vizra-core/internal/buildinfo"
	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/ops"
	"github.com/yegamble/vizra-core/internal/setup"
	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

const defaultEnvFile = "env/production.env"

type depthRow struct {
	Kind, State string
	Depth       int64
}

// opsEnv holds the I/O seams.
type opsEnv struct {
	run        func(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error
	queueDepth func(ctx context.Context, envFile string) ([]depthRow, error)
	stdout     io.Writer
	stderr     io.Writer
}

func realOpsEnv() opsEnv {
	return opsEnv{run: execRun, queueDepth: realQueueDepth, stdout: os.Stdout, stderr: os.Stderr}
}

func execRun(ctx context.Context, dir string, argv []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, stdout, stderr
	// The chain is passed explicitly; an inherited COMPOSE_FILE/PROFILES must not
	// silently widen or replace it.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "COMPOSE_FILE=") && !strings.HasPrefix(kv, "COMPOSE_PROFILES=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd.Run()
}

func realQueueDepth(ctx context.Context, envFile string) ([]depthRow, error) {
	lookup, err := envSource(envFile)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadFrom(lookup)
	if err != nil {
		return nil, err
	}
	pools, err := db.Open(ctx, site.NewResolver(cfg))
	if err != nil {
		return nil, err
	}
	defer pools.Close()
	rows, err := sqlcgen.New(pools.Default()).JobDepthByKindState(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]depthRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, depthRow{r.Kind, r.State, r.Depth})
	}
	return out, nil
}

type opsFlags struct {
	fs         *flag.FlagSet
	envFile    *string
	projectDir *string
}

func newOpsFlags(name string) opsFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return opsFlags{fs,
		fs.String("env", defaultEnvFile, "env file written by `vizra setup` (source of COMPOSE_FILE/COMPOSE_PROFILES)"),
		fs.String("project-dir", ".", "directory holding the compose files (the meta checkout)")}
}

// parseInterspersed lets flags follow the service name (`vizra logs api --tail 5`).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// chainAndModel derives the chain and renders the model it addresses.
func (e opsEnv) chainAndModel(ctx context.Context, envFile, dir string) (ops.Chain, ops.Model, error) {
	env, err := setup.ReadEnvFile(envFile)
	if err != nil {
		return ops.Chain{}, nil, fmt.Errorf("read env file: %w", err)
	}
	c, err := ops.LoadChain(envFile, env)
	if err != nil {
		return ops.Chain{}, nil, err
	}
	var out, errb bytes.Buffer
	if err := e.run(ctx, dir, c.Argv("config", "--format", "json"), &out, &errb); err != nil {
		return c, nil, fmt.Errorf("render compose model: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	m, err := ops.ParseModel(out.Bytes())
	return c, m, err
}

func runStatus(args []string, e opsEnv) int {
	f := newOpsFlags("status")
	if pos, err := parseInterspersed(f.fs, args); err != nil || len(pos) != 0 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Fprintf(e.stdout, "vizra %s (%s)\n", buildinfo.Release, buildinfo.Commit)
	c, m, err := e.chainAndModel(ctx, *f.envFile, *f.projectDir)
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 1
	}
	var out, errb bytes.Buffer
	if err := e.run(ctx, *f.projectDir, c.Argv("ps", "--all", "--format", "json"), &out, &errb); err != nil {
		fmt.Fprintf(e.stderr, "compose ps: %v: %s\n", err, strings.TrimSpace(errb.String()))
		return 1
	}
	ps, err := ops.ParsePS(out.Bytes())
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 1
	}
	code := 0
	for _, r := range ops.ServiceHealth(m, ps) {
		fmt.Fprintf(e.stdout, "%-8s %-10s %-30s %s\n", r.Status, r.Service, r.Image, r.Detail)
		if r.Status == ops.Fail {
			code = 1
		}
	}
	qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
	defer qcancel()
	rows, err := e.queueDepth(qctx, *f.envFile)
	if err != nil {
		// Never 0: an unmeasured queue is not an empty one.
		fmt.Fprintf(e.stdout, "%-8s %-10s queue depth not measured: %v\n", ops.Blocked, "jobs", err)
		if code == 0 {
			code = 3
		}
		return code
	}
	if len(rows) == 0 {
		fmt.Fprintf(e.stdout, "%-8s %-10s queue empty (measured)\n", ops.OK, "jobs")
	}
	for _, r := range rows {
		fmt.Fprintf(e.stdout, "%-8s %-10s %s/%s %d\n", ops.OK, "jobs", r.Kind, r.State, r.Depth)
	}
	return code
}

func runLogs(args []string, e opsEnv) int {
	f := newOpsFlags("logs")
	tail := f.fs.Int("tail", ops.DefaultTail, fmt.Sprintf("lines per container, 1..%d", ops.MaxTail))
	since := f.fs.String("since", "", "duration (30m) or RFC 3339 time, at most 168h back")
	follow := f.fs.Bool("follow", false, "stream new lines")
	dry := f.fs.Bool("dry-run", false, "print the compose argv instead of running it")
	pos, err := parseInterspersed(f.fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(e.stderr, "usage: vizra logs [--env F] [--tail N] [--since S] [--follow] SERVICE")
		return 2
	}
	ctx := context.Background()
	c, m, err := e.chainAndModel(ctx, *f.envFile, *f.projectDir)
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 1
	}
	argv, err := ops.LogsArgs(c, m, pos[0], *tail, *since, *follow, time.Now())
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 2
	}
	return e.exec(ctx, *f.projectDir, argv, *dry)
}

func runRestart(args []string, e opsEnv) int {
	f := newOpsFlags("restart")
	dry := f.fs.Bool("dry-run", false, "print the compose argv instead of running it")
	pos, err := parseInterspersed(f.fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(e.stderr, "usage: vizra restart [--env F] [--dry-run] SERVICE")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, m, err := e.chainAndModel(ctx, *f.envFile, *f.projectDir)
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 1
	}
	argv, err := ops.RestartArgs(c, m, pos[0])
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return 2
	}
	return e.exec(ctx, *f.projectDir, argv, *dry)
}

func (e opsEnv) exec(ctx context.Context, dir string, argv []string, dry bool) int {
	if dry {
		fmt.Fprintf(e.stdout, "would run (in %s): %s\n", dir, strings.Join(argv, " "))
		return 0
	}
	if err := e.run(ctx, dir, argv, e.stdout, e.stderr); err != nil {
		fmt.Fprintln(e.stderr, err)
		return 1
	}
	return 0
}

// runUpdate always exits 3 today: the release check cannot run (no feed).
func runUpdate(args []string, e opsEnv) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	var out bytes.Buffer
	v := "unknown (`docker compose version` failed)"
	if err := e.run(context.Background(), ".", []string{"docker", "compose", "version", "--short"}, &out, io.Discard); err == nil {
		v = strings.TrimSpace(out.String())
	}
	fmt.Fprint(e.stdout, ops.UpdateReport(buildinfo.Release, v))
	return 3
}

