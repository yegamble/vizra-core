package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeRun struct {
	calls [][]string
	ps    string
}

func (f *fakeRun) env(depthErr error) (opsEnv, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return opsEnv{
		run: func(_ context.Context, _ string, argv []string, stdout, _ io.Writer) error {
			f.calls = append(f.calls, slices.Clone(argv))
			switch {
			case slices.Contains(argv, "config"):
				_, _ = io.WriteString(stdout, `{"services":{"api":{"restart":"unless-stopped","image":"core"},"migrate":{"restart":"no"}}}`)
			case slices.Contains(argv, "ps"):
				_, _ = io.WriteString(stdout, f.ps)
			}
			return nil
		},
		queueDepth: func(context.Context, string) ([]depthRow, error) {
			if depthErr != nil {
				return nil, depthErr
			}
			return []depthRow{{"thumbnail", "queued", 7}}, nil
		},
		stdout: &out, stderr: &errb,
	}, &out, &errb
}

func writeEnv(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "production.env")
	if err := os.WriteFile(p, []byte("COMPOSE_FILE=docker-compose.yml:docker-compose.prod.yml\nCOMPOSE_PROFILES=core,edge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertChain: every compose call carries the env-derived chain; none is bare.
func assertChain(t *testing.T, calls [][]string, envFile string) {
	t.Helper()
	prefix := []string{"docker", "compose", "--env-file", envFile, "-f", "docker-compose.yml", "-f", "docker-compose.prod.yml", "--profile", "core", "--profile", "edge"}
	if len(calls) == 0 {
		t.Fatal("no compose call")
	}
	for _, c := range calls {
		if len(c) < len(prefix) || !slices.Equal(c[:len(prefix)], prefix) {
			t.Errorf("call without the env-derived chain: %q", c)
		}
	}
}

func TestRestartExecsChainAndRefusesUnknownAndOneShot(t *testing.T) {
	envFile := writeEnv(t)
	f := &fakeRun{}
	e, _, _ := f.env(nil)
	if code := runRestart([]string{"--env", envFile, "api"}, e); code != 0 {
		t.Fatalf("exit %d", code)
	}
	assertChain(t, f.calls, envFile)
	if last := f.calls[len(f.calls)-1]; !slices.Equal(last[len(last)-3:], []string{"restart", "--", "api"}) {
		t.Fatalf("last call %q", last)
	}
	for _, svc := range []string{"postgres", "migrate"} {
		f := &fakeRun{}
		e, _, errb := f.env(nil)
		if code := runRestart([]string{"--env", envFile, svc}, e); code != 2 {
			t.Errorf("%s: exit %d, want 2", svc, code)
		}
		for _, c := range f.calls {
			if slices.Contains(c, "restart") {
				t.Errorf("%s: restart exec'd anyway: %q (%s)", svc, c, errb)
			}
		}
	}
}

func TestLogsExecsBoundedArgs(t *testing.T) {
	envFile := writeEnv(t)
	f := &fakeRun{}
	e, _, _ := f.env(nil)
	if code := runLogs([]string{"api", "--env", envFile, "--tail", "5", "--since", "10m"}, e); code != 0 {
		t.Fatalf("exit %d", code)
	}
	assertChain(t, f.calls, envFile)
	last := f.calls[len(f.calls)-1]
	if want := []string{"logs", "--no-color", "--tail", "5", "--since", "10m", "--", "api"}; !slices.Equal(last[len(last)-len(want):], want) {
		t.Fatalf("logs argv %q", last)
	}
	f = &fakeRun{}
	e, _, _ = f.env(nil)
	if code := runLogs([]string{"--env", envFile, "--tail", "999999", "api"}, e); code != 2 {
		t.Fatalf("unbounded tail exit %d", code)
	}
}

func TestStatusQueueDepthBlockedNeverZero(t *testing.T) {
	envFile := writeEnv(t)
	f := &fakeRun{ps: `{"Service":"api","State":"running","Health":"healthy","Image":"core:1"}
{"Service":"migrate","State":"exited","ExitCode":0}`}
	e, out, _ := f.env(errors.New("dial tcp: connection refused"))
	if code := runStatus([]string{"--env", envFile}, e); code != 3 {
		t.Fatalf("exit %d, want 3 (BLOCKED)\n%s", code, out)
	}
	assertChain(t, f.calls, envFile)
	if !strings.Contains(out.String(), "BLOCKED") || strings.Contains(out.String(), "queue empty") {
		t.Fatalf("unmeasured queue not BLOCKED:\n%s", out)
	}
	f2 := &fakeRun{ps: f.ps}
	e, out, _ = f2.env(nil)
	if code := runStatus([]string{"--env", envFile}, e); code != 0 || !strings.Contains(out.String(), "thumbnail/queued 7") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestOpsRefuseEnvWithoutChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.env")
	_ = os.WriteFile(p, []byte("VIZRA_X=1\n"), 0o600)
	f := &fakeRun{}
	e, _, _ := f.env(nil)
	if code := runRestart([]string{"--env", p, "api"}, e); code != 1 || len(f.calls) != 0 {
		t.Fatalf("exit %d calls %q", code, f.calls)
	}
}

func TestUpdateIsBlocked(t *testing.T) {
	f := &fakeRun{}
	e, out, _ := f.env(nil)
	if code := runUpdate(nil, e); code != 3 || !strings.Contains(out.String(), "BLOCKED") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
