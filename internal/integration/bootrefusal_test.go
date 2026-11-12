//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProductionBootRefusesDevSecrets is the VZ-FOUND-006 N1 demonstration at
// process level: the REAL api and worker binaries, started with
// VIZRA_MODE=production, must exit non-zero with an error that names the
// refused key (and never echoes the refused value) for a published/dev
// session secret, a blank secret, and an insecure seam (plain-http origin).
func TestProductionBootRefusesDevSecrets(t *testing.T) {
	dir := binaries(t)
	secret := strings.Repeat("Vz9Kp4Mw2Ng7", 3)[:32]
	base := map[string]string{
		"VIZRA_MODE":           "production",
		"VIZRA_PUBLIC_ORIGIN":  "https://photos.example.org",
		"DATABASE_URL":         "postgres://vizra@127.0.0.1:1/vizra?sslmode=disable&connect_timeout=1",
		"VIZRA_CACHE_URL":      "redis://127.0.0.1:1/0",
		"VIZRA_SESSION_SECRET": secret,
		"VIZRA_MFA_KEY_KEK":    secret,
	}
	devSecret := "changeme-changeme-changeme-changeme"
	cases := []struct {
		name, key, value, wantKey, wantMsg string
	}{
		{"dev session secret", "VIZRA_SESSION_SECRET", devSecret, "VIZRA_SESSION_SECRET", "known development value"},
		{"blank session secret", "VIZRA_SESSION_SECRET", "", "VIZRA_SESSION_SECRET", "must be set in production"},
		{"blank MFA KEK", "VIZRA_MFA_KEY_KEK", "", "VIZRA_MFA_KEY_KEK", "must be set in production"},
		{"plain-http origin", "VIZRA_PUBLIC_ORIGIN", "http://photos.example.org", "VIZRA_PUBLIC_ORIGIN", "plain-http origin"},
	}
	for _, bin := range []string{"vizra-api", "vizra-worker"} {
		for _, c := range cases {
			t.Run(bin+"/"+c.name, func(t *testing.T) {
				env := map[string]string{}
				for k, v := range base {
					env[k] = v
				}
				env[c.key] = c.value
				code, stderr := runBoot(t, filepath.Join(dir, bin), env)
				t.Logf("%s exit=%d stderr=%q", bin, code, stderr)
				if code == 0 {
					t.Fatalf("%s booted in production with %s; it must refuse", bin, c.name)
				}
				if !strings.Contains(stderr, c.wantKey+": ") || !strings.Contains(stderr, c.wantMsg) {
					t.Fatalf("%s refusal did not name %s (%q): %s", bin, c.wantKey, c.wantMsg, stderr)
				}
				if c.value != "" && c.key != "VIZRA_PUBLIC_ORIGIN" && strings.Contains(stderr, c.value) {
					t.Fatalf("%s refusal echoed the refused secret", bin)
				}
			})
		}
		// Control: the valid baseline gets past configuration, so the refusals
		// above are caused by the one mutated key, not by the harness.
		t.Run(bin+"/control", func(t *testing.T) {
			_, stderr := runBoot(t, filepath.Join(dir, bin), base)
			t.Logf("%s control stderr=%q", bin, stderr)
			for _, k := range []string{"VIZRA_SESSION_SECRET:", "VIZRA_MFA_KEY_KEK:", "VIZRA_PUBLIC_ORIGIN:"} {
				if strings.Contains(stderr, k) {
					t.Fatalf("valid production config was refused on %s: %s", k, stderr)
				}
			}
		})
	}
}

func runBoot(t *testing.T, bin string, env map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), stderr.String()
	default:
		t.Fatalf("run %s: %v", bin, err)
		return -1, ""
	}
}
