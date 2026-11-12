package ops

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var env = map[string]string{
	"COMPOSE_FILE":     "docker-compose.yml:docker-compose.prod.yml:docker-compose.external-postgres.yml",
	"COMPOSE_PROFILES": "core,frontend,edge",
}

const model = `{"services":{
 "api":{"image":"vizra-core:1","restart":"unless-stopped"},
 "migrate":{"image":"vizra-core:1","restart":"no"},
 "web":{"image":"vizra-user:1","restart":"unless-stopped"}}}`

func fixture(t *testing.T) (Chain, Model) {
	t.Helper()
	c, err := LoadChain("env/production.env", env)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseModel([]byte(model))
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}

var wantPrefix = []string{"docker", "compose", "--env-file", "env/production.env",
	"-f", "docker-compose.yml", "-f", "docker-compose.prod.yml", "-f", "docker-compose.external-postgres.yml",
	"--profile", "core", "--profile", "frontend", "--profile", "edge"}

func TestArgvIsExactEnvDerivedChain(t *testing.T) {
	c, _ := fixture(t)
	got := c.Argv("ps", "--format", "json")
	want := append(slices.Clone(wantPrefix), "ps", "--format", "json")
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

func TestLoadChainRefusesBareCompose(t *testing.T) {
	if _, err := LoadChain("", env); err == nil {
		t.Fatal("no env file accepted")
	}
	if _, err := LoadChain("e.env", map[string]string{"COMPOSE_PROFILES": "core"}); err == nil || !strings.Contains(err.Error(), "COMPOSE_FILE") {
		t.Fatalf("missing COMPOSE_FILE accepted: %v", err)
	}
}

func TestLogsArgsBoundedAndValidated(t *testing.T) {
	c, m := fixture(t)
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	got, err := LogsArgs(c, m, "api", 50, "30m", false, now)
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(wantPrefix), "logs", "--no-color", "--tail", "50", "--since", "30m", "--", "api")
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	bad := []struct {
		svc, since string
		tail       int
	}{
		{"nope", "", 10}, {"api; rm -rf /", "", 10}, {"api", "", 0}, {"api", "", MaxTail + 1},
		{"api", "200h", 10}, {"api", "-5m", 10}, {"api", "yesterday", 10},
		{"api", "2020-01-01T00:00:00Z", 10}, {"api", "2027-01-01T00:00:00Z", 10},
	}
	for _, b := range bad {
		if a, err := LogsArgs(c, m, b.svc, b.tail, b.since, false, now); err == nil {
			t.Errorf("accepted %+v -> %q", b, a)
		}
	}
	if _, err := LogsArgs(c, m, "api", MaxTail, "2026-11-11T00:00:00Z", true, now); err != nil {
		t.Errorf("in-bounds RFC3339 refused: %v", err)
	}
}

func TestRestartRefusesUnknownAndOneShot(t *testing.T) {
	c, m := fixture(t)
	got, err := RestartArgs(c, m, "web")
	if err != nil {
		t.Fatal(err)
	}
	if want := append(slices.Clone(wantPrefix), "restart", "--", "web"); !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	if _, err := RestartArgs(c, m, "postgres"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unknown service accepted: %v", err)
	}
	if _, err := RestartArgs(c, m, "migrate"); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("one-shot restart accepted: %v", err)
	}
}

func TestParsePSBothShapesAndHealth(t *testing.T) {
	_, m := fixture(t)
	nd := `{"Service":"api","State":"running","Health":"healthy","Image":"vizra-core:1.2"}
{"Service":"migrate","State":"exited","ExitCode":0}
{"Service":"web","State":"running","Health":"unhealthy"}`
	arr := "[" + strings.ReplaceAll(nd, "\n", ",") + "]"
	for _, raw := range []string{nd, arr} {
		ps, err := ParsePS([]byte(raw))
		if err != nil || len(ps) != 3 {
			t.Fatalf("parse: %v %d", err, len(ps))
		}
		rows := ServiceHealth(m, ps)
		got := map[string]string{}
		for _, r := range rows {
			got[r.Service] = r.Status
		}
		if got["api"] != OK || got["migrate"] != OK || got["web"] != Fail {
			t.Fatalf("health %v", got)
		}
		if rows[0].Image != "vizra-core:1.2" {
			t.Fatalf("version from ps image not reported: %+v", rows[0])
		}
	}
	rows := ServiceHealth(m, nil)
	for _, r := range rows {
		if r.Status != Fail {
			t.Fatalf("absent container not FAIL: %+v", r)
		}
	}
	ps, _ := ParsePS([]byte(`{"Service":"migrate","State":"exited","ExitCode":1}`))
	if ServiceHealth(m, ps)[1].Status != Fail {
		t.Fatal("failed one-shot reported OK")
	}
}

func TestUpdateReportIsBlockedNotUpToDate(t *testing.T) {
	r := UpdateReport("dev", "2.29.0")
	for _, want := range []string{"BLOCKED", "2.24.4", "vizra migrate", "bare `docker compose`"} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in\n%s", want, r)
		}
	}
	if !strings.Contains(r, "release check: BLOCKED") {
		t.Error("release check not BLOCKED")
	}
}
