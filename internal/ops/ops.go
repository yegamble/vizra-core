// Package ops is the pure half of the day-2 operator commands (VZ-OPS-003):
// `vizra status`, `vizra logs`, `vizra restart` and `vizra update`.
//
// Every compose invocation is built by Chain.Argv: `docker compose --env-file
// F -f a.yml -f b.yml --profile p ... SUB`, where the -f files and profiles are
// exactly COMPOSE_FILE and COMPOSE_PROFILES from the env file `vizra setup`
// wrote. Running bare `docker compose` on a production host is WRONG: it falls
// back to the default compose file, drops the prod overlay and external-service
// overlays, enables no profiles and reads no env file, so it addresses a
// different project shape than the one that is running.
package ops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Chain is the env-derived compose invocation.
type Chain struct {
	EnvFile  string
	Files    []string
	Profiles []string
}

// LoadChain derives the chain from a parsed env file. An env file without
// COMPOSE_FILE is refused: guessing a chain is the bare-compose mistake.
func LoadChain(envFile string, env map[string]string) (Chain, error) {
	if envFile == "" {
		return Chain{}, errors.New("no env file: pass --env (the file `vizra setup` wrote); bare `docker compose` is not supported")
	}
	sep := env["COMPOSE_PATH_SEPARATOR"]
	if sep == "" {
		sep = ":"
	}
	c := Chain{EnvFile: envFile}
	for _, f := range strings.Split(env["COMPOSE_FILE"], sep) {
		if f = strings.TrimSpace(f); f != "" {
			c.Files = append(c.Files, f)
		}
	}
	if len(c.Files) == 0 {
		return Chain{}, fmt.Errorf("%s has no COMPOSE_FILE; re-run `vizra setup` — refusing to guess a compose chain", envFile)
	}
	for _, p := range strings.Split(env["COMPOSE_PROFILES"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Profiles = append(c.Profiles, p)
		}
	}
	return c, nil
}

// Argv is the full argv for one compose subcommand.
func (c Chain) Argv(sub ...string) []string {
	a := []string{"docker", "compose", "--env-file", c.EnvFile}
	for _, f := range c.Files {
		a = append(a, "-f", f)
	}
	for _, p := range c.Profiles {
		a = append(a, "--profile", p)
	}
	return append(a, sub...)
}

// Service is one service of the rendered model (`compose config --format json`).
type Service struct {
	Name    string
	Image   string
	Restart string
}

// OneShot reports a run-to-completion service (restart "no" or unset, e.g.
// migrate). Restarting one re-runs it, so `vizra restart` refuses.
func (s Service) OneShot() bool { return s.Restart == "" || s.Restart == "no" }

// Model is the rendered service set.
type Model map[string]Service

// ParseModel reads `docker compose ... config --format json`.
func ParseModel(raw []byte) (Model, error) {
	var doc struct {
		Services map[string]struct {
			Image   string `json:"image"`
			Restart string `json:"restart"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse rendered compose model: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, errors.New("rendered compose model has no services")
	}
	m := Model{}
	for n, s := range doc.Services {
		m[n] = Service{Name: n, Image: s.Image, Restart: s.Restart}
	}
	return m, nil
}

// Names lists services, sorted.
func (m Model) Names() []string {
	n := make([]string, 0, len(m))
	for k := range m {
		n = append(n, k)
	}
	slices.Sort(n)
	return n
}

// Lookup validates a service name against the rendered model.
func (m Model) Lookup(name string) (Service, error) {
	s, ok := m[name]
	if !ok {
		return Service{}, fmt.Errorf("unknown service %q: the rendered chain has %s", name, strings.Join(m.Names(), ", "))
	}
	return s, nil
}

// Log bounds.
const (
	DefaultTail = 200
	MaxTail     = 10000
	MaxSince    = 7 * 24 * time.Hour
)

// LogsArgs validates and builds `logs`. since is a Go duration (<= MaxSince)
// or an RFC 3339 time no older than MaxSince; now is injected for tests.
func LogsArgs(c Chain, m Model, svc string, tail int, since string, follow bool, now time.Time) ([]string, error) {
	if _, err := m.Lookup(svc); err != nil {
		return nil, err
	}
	if tail < 1 || tail > MaxTail {
		return nil, fmt.Errorf("--tail must be 1..%d, got %d", MaxTail, tail)
	}
	sub := []string{"logs", "--no-color", "--tail", strconv.Itoa(tail)}
	if since != "" {
		if d, err := time.ParseDuration(since); err == nil {
			if d <= 0 || d > MaxSince {
				return nil, fmt.Errorf("--since must be in (0, %s], got %s", MaxSince, since)
			}
		} else if t, err := time.Parse(time.RFC3339, since); err == nil {
			if now.Sub(t) > MaxSince || t.After(now) {
				return nil, fmt.Errorf("--since %s is outside the last %s", since, MaxSince)
			}
		} else {
			return nil, fmt.Errorf("--since %q is neither a duration (e.g. 30m) nor RFC 3339", since)
		}
		sub = append(sub, "--since", since)
	}
	if follow {
		sub = append(sub, "--follow")
	}
	return c.Argv(append(sub, "--", svc)...), nil
}

// RestartArgs validates and builds `restart`.
func RestartArgs(c Chain, m Model, svc string) ([]string, error) {
	s, err := m.Lookup(svc)
	if err != nil {
		return nil, err
	}
	if s.OneShot() {
		return nil, fmt.Errorf("refusing to restart one-shot service %q (restart policy %q): it runs to completion; re-running it is a deliberate `vizra migrate`/`vizra update` step", svc, s.Restart)
	}
	return c.Argv("restart", "--", svc), nil
}

// PSEntry is one container from `compose ps --format json`.
type PSEntry struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	Image    string `json:"Image"`
	ExitCode int    `json:"ExitCode"`
}

// ParsePS accepts both shapes Compose has emitted: one JSON array (<2.21) and
// newline-delimited objects (>=2.21).
func ParsePS(raw []byte) ([]PSEntry, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		var out []PSEntry
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
		return out, nil
	}
	var out []PSEntry
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e PSEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Status words.
const (
	OK      = "OK"
	Fail    = "FAIL"
	Blocked = "BLOCKED"
)

// ServiceRow is one line of `vizra status`.
type ServiceRow struct {
	Service, Status, Detail, Image string
}

// ServiceHealth judges every service in the model against ps. A long-running
// service that is absent, not running, or unhealthy is FAIL; a one-shot is OK
// only when it exited 0.
func ServiceHealth(m Model, ps []PSEntry) []ServiceRow {
	by := map[string]PSEntry{}
	for _, e := range ps {
		by[e.Service] = e
	}
	var rows []ServiceRow
	for _, n := range m.Names() {
		s := m[n]
		e, ok := by[n]
		r := ServiceRow{Service: n, Image: s.Image}
		switch {
		case !ok:
			r.Status, r.Detail = Fail, "no container"
		case s.OneShot():
			if e.State == "exited" && e.ExitCode == 0 {
				r.Status, r.Detail = OK, "one-shot completed (exit 0)"
			} else {
				r.Status, r.Detail = Fail, fmt.Sprintf("one-shot %s exit %d", e.State, e.ExitCode)
			}
		case e.State != "running":
			r.Status, r.Detail = Fail, e.State
		case e.Health == "" || e.Health == "healthy":
			r.Status, r.Detail = OK, strings.TrimSpace("running "+e.Health)
		default:
			r.Status, r.Detail = Fail, "running, "+e.Health
		}
		if ok && e.Image != "" {
			r.Image = e.Image
		}
		rows = append(rows, r)
	}
	return rows
}

// UpdateReport is `vizra update`'s answer. There is no release feed or
// signed release record yet, so the check is BLOCKED, never "up to date".
func UpdateReport(current string, composeVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "current release: %s\n", current)
	b.WriteString("release check: BLOCKED — no release feed or signed release record exists yet; refusing to claim this host is up to date\n")
	fmt.Fprintf(&b, "compose floor: Docker Compose >= 2.24.4 is required (Q-017: the chain uses `!override`); detected: %s\n", composeVersion)
	b.WriteString(`upgrade path:
  1. vizra doctor --env ENV            (must exit 0)
  2. back up (vizra backup, when it ships) — never skip
  3. pin the new release tag in every component checkout
  4. vizra migrate                     (the schema floor: a binary older than the
                                        database schema must not be rolled out)
  5. docker compose via vizra only: vizra restart <svc> per service, or
     re-run the installer; bare ` + "`docker compose`" + ` on a prod host is wrong.
`)
	return b.String()
}
