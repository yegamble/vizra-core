package setup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestWeb(t *testing.T) (*Web, *httptest.Server, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "env", "production.env")
	w, err := NewWeb(Options{Path: p}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	w.Host = strings.TrimPrefix(srv.URL, "http://")
	return w, srv, p
}

func get(t *testing.T, u, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if host != "" {
		req.Host = host
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func post(t *testing.T, u string, v url.Values) (int, string, http.Header) {
	t.Helper()
	r, err := http.PostForm(u, v)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b), r.Header
}

const webSecret = "s3cret-Value-Never-Echoed-123456"

func validForm(tok string) url.Values {
	v := url.Values{"token": {tok}, "domain": {"photos.example.org"}, "release": {"v1.0.0"}, "postgres_password": {webSecret}}
	return v
}

func TestWebTokenRequired(t *testing.T) {
	w, srv, _ := newTestWeb(t)
	for _, u := range []string{srv.URL + "/", srv.URL + "/?t=", srv.URL + "/?t=" + strings.Repeat("0", 64)} {
		if c, _ := get(t, u, ""); c != 403 {
			t.Errorf("GET %s = %d, want 403", u, c)
		}
	}
	if c, _, _ := post(t, srv.URL+"/", url.Values{"token": {"bad"}}); c != 403 {
		t.Errorf("POST bad token = %d", c)
	}
	if len(w.Token) != 64 {
		t.Errorf("token len %d, want 64 hex (256 bits)", len(w.Token))
	}
}

func TestWebHostRebinding(t *testing.T) {
	w, srv, _ := newTestWeb(t)
	if c, _ := get(t, srv.URL+"/?t="+w.Token, "evil.example:80"); c != 403 {
		t.Errorf("rebinding Host = %d, want 403", c)
	}
}

func TestWebFormListsQuestionsAndHeaders(t *testing.T) {
	w, srv, _ := newTestWeb(t)
	r, err := http.Get(srv.URL + "/?t=" + w.Token)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("GET = %d", r.StatusCode)
	}
	body := string(b)
	for _, q := range Questions() {
		if !strings.Contains(body, `name="`+q.ID+`"`) || !strings.Contains(body, `for="`+q.ID+`"`) {
			t.Errorf("form lacks question %s", q.ID)
		}
		if q.Secret && !strings.Contains(body, `type="password" id="`+q.ID+`"`) {
			t.Errorf("secret %s not a password input", q.ID)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Error("form has script or external reference")
	}
	h := r.Header
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q", csp)
	}
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers %v", h)
	}
}

func TestWebSubmitWritesAndStops(t *testing.T) {
	w, srv, p := newTestWeb(t)
	c, body, _ := post(t, srv.URL+"/", validForm(w.Token))
	if c != 200 {
		t.Fatalf("POST = %d %s", c, body)
	}
	if strings.Contains(body, webSecret) {
		t.Error("secret echoed")
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", st, err)
	}
	env, _ := ReadEnvFile(p)
	if env["VIZRA_DOMAIN"] != "photos.example.org" {
		t.Errorf("domain %q", env["VIZRA_DOMAIN"])
	}
	select {
	case <-w.Done():
	case <-time.After(time.Second):
		t.Fatal("server not stopped after submit")
	}
	if c, _, _ := post(t, srv.URL+"/", validForm(w.Token)); c != 403 {
		t.Errorf("reused token = %d, want 403", c)
	}
	if c, _ := get(t, srv.URL+"/?t="+w.Token, ""); c != 403 {
		t.Errorf("reused token GET = %d, want 403", c)
	}
}

func TestWebSecretsNeverEchoedOnError(t *testing.T) {
	w, srv, p := newTestWeb(t)
	v := validForm(w.Token)
	v.Set("domain", "not a host!")
	c, body, _ := post(t, srv.URL+"/", v)
	if c != 400 || strings.Contains(body, webSecret) {
		t.Errorf("invalid POST = %d, secret echoed=%v", c, strings.Contains(body, webSecret))
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("file written on invalid input")
	}
	// file indirection must not be reachable from the browser
	v = validForm(w.Token)
	v.Set("postgres_password", "@/etc/passwd")
	if c, _, _ := post(t, srv.URL+"/", v); c != 400 {
		t.Errorf("@file from web = %d, want 400", c)
	}
	_, g := get(t, srv.URL+"/?t="+w.Token, "")
	if strings.Contains(g, webSecret) {
		t.Error("secret in GET")
	}
}

func TestWebRerunKeepsValuesAndNeedsYes(t *testing.T) {
	w, srv, p := newTestWeb(t)
	if c, b, _ := post(t, srv.URL+"/", validForm(w.Token)); c != 200 {
		t.Fatal(b)
	}
	before, _ := os.ReadFile(p)
	w2, _ := NewWeb(Options{Path: p}, time.Minute)
	s2 := httptest.NewServer(w2)
	defer s2.Close()
	w2.Host = strings.TrimPrefix(s2.URL, "http://")
	_, g := get(t, s2.URL+"/?t="+w2.Token, "")
	if strings.Contains(g, webSecret) {
		t.Error("existing secret pre-filled")
	}
	v := url.Values{"token": {w2.Token}}
	if c, b, _ := post(t, s2.URL+"/", v); c != 200 {
		t.Fatalf("unchanged rerun = %d %s", c, b)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("rerun changed file")
	}
}

func TestWebExpiry(t *testing.T) {
	w, srv, _ := newTestWeb(t)
	w.expires = time.Now().Add(-time.Second)
	if c, _ := get(t, srv.URL+"/?t="+w.Token, ""); c != 403 {
		t.Errorf("expired = %d", c)
	}
}

func TestLoopbackAddr(t *testing.T) {
	for _, a := range []string{"0.0.0.0:0", ":0", "192.168.1.2:0", "localhost:0", "[::]:0", "example.com:80"} {
		if err := CheckLoopback(a); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
	for _, a := range []string{"127.0.0.1:0", "127.0.0.1:8099"} {
		if err := CheckLoopback(a); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
}

func TestWebRefusesRewriteWithoutYes(t *testing.T) {
	w, srv, p := newTestWeb(t)
	if c, b, _ := post(t, srv.URL+"/", validForm(w.Token)); c != 200 {
		t.Fatal(b)
	}
	old, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append([]byte("# operator comment\n"), old...), 0o600); err != nil {
		t.Fatal(err)
	}
	w2, _ := NewWeb(Options{Path: p}, time.Minute)
	s2 := httptest.NewServer(w2)
	defer s2.Close()
	w2.Host = strings.TrimPrefix(s2.URL, "http://")
	c, b, _ := post(t, s2.URL+"/", url.Values{"token": {w2.Token}})
	if c != 400 || !strings.Contains(b, "--yes") {
		t.Errorf("rewrite without --yes = %d", c)
	}
}
