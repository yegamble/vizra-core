package doctor_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yegamble/vizra-core/internal/doctor"
)

// VZ-OPS-002: every host check has a failing fixture, and every check whose
// subject cannot be examined reports BLOCKED — never OK.

func TestCheckDockerEngine(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		lookErr error
		runErr  error
		want    doctor.Status
	}{
		{"healthy", "29.9.2\n", nil, nil, doctor.StatusOK},
		{"docker absent is BLOCKED", "", errors.New("not found"), nil, doctor.StatusBlocked},
		{"daemon unreachable", "", nil, errors.New("exit 1"), doctor.StatusFail},
		{"unparseable version", "banana", nil, nil, doctor.StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctor.CheckDockerEngine(tc.raw, tc.lookErr, tc.runErr); got.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Detail, tc.want)
			}
		})
	}
}

func TestCheckEnvFile(t *testing.T) {
	if got := doctor.CheckEnvFile("", nil, nil); got.Status != doctor.StatusBlocked {
		t.Errorf("no --env: %s, want BLOCKED", got.Status)
	}
	if got := doctor.CheckEnvFile("x.env", errors.New("no such file"), nil); got.Status != doctor.StatusFail {
		t.Errorf("unreadable: %s, want FAIL", got.Status)
	}
	if got := doctor.CheckEnvFile("x.env", nil, []string{"VIZRA_SESSION_SECRET: too short"}); got.Status != doctor.StatusFail {
		t.Errorf("setup --check problems: %s, want FAIL", got.Status)
	}
	if got := doctor.CheckEnvFile("x.env", nil, nil); got.Status != doctor.StatusOK {
		t.Errorf("valid: %s, want OK", got.Status)
	}
}

func TestCheckTopology(t *testing.T) {
	cases := []struct {
		name string
		in   doctor.TopologyInput
		want doctor.Status
	}{
		{"no env file", doctor.TopologyInput{}, doctor.StatusBlocked},
		{"no chain in env", doctor.TopologyInput{EnvFile: "x.env"}, doctor.StatusBlocked},
		{"docker absent", doctor.TopologyInput{EnvFile: "x.env", ComposeFile: "a.yml", LookErr: errors.New("nf")}, doctor.StatusBlocked},
		{"render fails", doctor.TopologyInput{EnvFile: "x.env", ComposeFile: "a.yml", RunErr: errors.New("exit 15"), Stderr: "service \"api\" refers to undefined volume"}, doctor.StatusFail},
		{"renders", doctor.TopologyInput{EnvFile: "x.env", ComposeFile: "a.yml", Profiles: "edge"}, doctor.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctor.CheckTopology(tc.in); got.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Detail, tc.want)
			}
		})
	}
}

func TestCheckDisk(t *testing.T) {
	if got := doctor.CheckDisk("/data", 0, errors.New("statfs failed")); got.Status != doctor.StatusBlocked {
		t.Errorf("statfs error: %s, want BLOCKED", got.Status)
	}
	if got := doctor.CheckDisk("/data", doctor.DiskFloorBytes-1, nil); got.Status != doctor.StatusFail {
		t.Errorf("below floor: %s, want FAIL", got.Status)
	}
	if got := doctor.CheckDisk("/data", doctor.DiskFloorBytes, nil); got.Status != doctor.StatusOK {
		t.Errorf("at floor: %s, want OK", got.Status)
	}
}

func TestProbeStorageWritable(t *testing.T) {
	dir := t.TempDir()
	if got := doctor.CheckStorage(dir, doctor.ProbeWritable(dir)); got.Status != doctor.StatusOK {
		t.Errorf("writable dir: %s (%s)", got.Status, got.Detail)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe left %d file(s) behind", len(entries))
	}
	missing := filepath.Join(dir, "nope")
	if got := doctor.CheckStorage(missing, doctor.ProbeWritable(missing)); got.Status != doctor.StatusFail {
		t.Errorf("missing root: %s, want FAIL", got.Status)
	}
	if got := doctor.CheckStorage("", nil); got.Status != doctor.StatusBlocked {
		t.Errorf("no root configured: %s, want BLOCKED", got.Status)
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		if got := doctor.CheckStorage(ro, doctor.ProbeWritable(ro)); got.Status != doctor.StatusFail {
			t.Errorf("read-only root: %s, want FAIL", got.Status)
		}
	}
}

func TestCheckAPIReadiness(t *testing.T) {
	if got := doctor.CheckAPIReadiness(0, "ready"); got.Status != doctor.StatusOK {
		t.Errorf("ready: %s", got.Status)
	}
	if got := doctor.CheckAPIReadiness(1, "NOT READY — HTTP 503"); got.Status != doctor.StatusFail {
		t.Errorf("not ready: %s, want FAIL", got.Status)
	}
	if got := doctor.CheckAPIReadiness(64, "bad addr"); got.Status != doctor.StatusBlocked {
		t.Errorf("probe could not run: %s, want BLOCKED", got.Status)
	}
}

// The subjects of these checks do not exist yet. They must say so.
func TestNotYetCheckableReportBlocked(t *testing.T) {
	got := doctor.NotYetCheckable()
	if len(got) != 3 {
		t.Fatalf("got %d results, want DNS/TLS, closed ports, backup freshness", len(got))
	}
	for _, r := range got {
		if r.Status != doctor.StatusBlocked || r.Detail == "" {
			t.Errorf("%s: %s %q, want BLOCKED with a reason", r.Name, r.Status, r.Detail)
		}
	}
}

func TestExitCodeDistinguishesBlockedFromFail(t *testing.T) {
	ok := doctor.Result{Name: "a", Status: doctor.StatusOK}
	bl := doctor.Result{Name: "b", Status: doctor.StatusBlocked, Detail: "x"}
	fl := doctor.Result{Name: "c", Status: doctor.StatusFail}
	if c := doctor.ExitCode([]doctor.Result{ok}); c != doctor.ExitOK {
		t.Errorf("all OK: %d", c)
	}
	if c := doctor.ExitCode([]doctor.Result{ok, bl}); c != doctor.ExitBlocked {
		t.Errorf("OK+BLOCKED: %d, want %d", c, doctor.ExitBlocked)
	}
	if c := doctor.ExitCode([]doctor.Result{bl, fl}); c != doctor.ExitFail {
		t.Errorf("BLOCKED+FAIL: %d, want %d", c, doctor.ExitFail)
	}
	if doctor.ExitBlocked == doctor.ExitOK || doctor.ExitBlocked == doctor.ExitFail {
		t.Error("BLOCKED must have its own non-zero exit code")
	}
}
