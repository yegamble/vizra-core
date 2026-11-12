package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Web is the `vizra setup --web` loopback wizard. It drives the same engine
// (Resolve + write) as the terminal interview. A 256-bit one-time token
// gates every request; it doubles as the CSRF token in the form, expires,
// and is invalidated by a successful submit.
type Web struct {
	Opts  Options
	Token string
	// Host is the exact Host header accepted (the bound 127.0.0.1:port);
	// anything else is refused (DNS rebinding).
	Host string

	mu      sync.Mutex
	expires time.Time
	used    bool
	done    chan struct{}
}

// NewWeb mints the token. The token is never logged by this package.
func NewWeb(o Options, ttl time.Duration) (*Web, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	return &Web{Opts: o, Token: hex.EncodeToString(b), expires: time.Now().Add(ttl), done: make(chan struct{})}, nil
}

// Done is closed after a successful write.
func (w *Web) Done() <-chan struct{} { return w.done }

// CheckLoopback accepts only a literal 127.0.0.1 listen address: no
// wildcard, no other interface, no host name that could resolve elsewhere.
func CheckLoopback(addr string) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if h != "127.0.0.1" {
		return fmt.Errorf("vizra setup --web: refusing to listen on %q; only 127.0.0.1 is allowed", addr)
	}
	return nil
}

func (w *Web) valid(tok string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	ok := subtle.ConstantTimeCompare([]byte(tok), []byte(w.Token)) == 1
	return ok && !w.used && time.Now().Before(w.expires)
}

func setHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "text/html; charset=utf-8")
}

var webPage = template.Must(template.New("p").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Vizra setup</title></head><body>
<h1>Vizra setup</h1>
{{if .Msg}}<p role="alert">{{.Msg}}</p>{{end}}
{{if .Form}}<p>Leave a field blank to keep the existing value, take the default, or mint a random secret. Fields that do not apply to your choices are ignored.</p>
<form method="post" action="/">
<input type="hidden" name="token" value="{{.Token}}">
{{range .Qs}}<p><label for="{{.ID}}">{{.Prompt}}{{if .Set}} (already set; kept){{end}}</label><br>
{{if .Choices}}<select id="{{.ID}}" name="{{.ID}}"><option value="">(default{{if .Def}}: {{.Def}}{{end}})</option>{{range .Choices}}<option>{{.}}</option>{{end}}</select>
{{else if .Secret}}<input type="password" id="{{.ID}}" name="{{.ID}}" autocomplete="off">
{{else}}<input type="text" id="{{.ID}}" name="{{.ID}}" placeholder="{{.Def}}">{{end}}</p>
{{end}}<p><button type="submit">Write env file</button></p>
</form>{{end}}
</body></html>
`))

type webQ struct {
	ID, Prompt, Def string
	Choices         []string
	Secret, Set     bool
}

func (w *Web) render(rw http.ResponseWriter, code int, msg string, form bool) {
	existing, _ := ReadEnvFile(w.Opts.Path)
	var qs []webQ
	for _, q := range Questions() {
		d := ""
		if q.Default != nil && !q.Secret {
			d = q.Default(map[string]string{})
		}
		_, set := existing[q.Keys[0]]
		qs = append(qs, webQ{ID: q.ID, Prompt: q.Prompt, Def: d, Choices: q.Choices, Secret: q.Secret, Set: set})
	}
	rw.WriteHeader(code)
	_ = webPage.Execute(rw, map[string]any{"Msg": msg, "Form": form, "Token": w.Token, "Qs": qs})
}

func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	setHeaders(rw.Header())
	if r.Host != w.Host || r.URL.Path != "/" {
		w.render(rw, http.StatusForbidden, "Forbidden.", false)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !w.valid(r.URL.Query().Get("t")) {
			w.render(rw, http.StatusForbidden, "Forbidden: the link is invalid, expired or already used.", false)
			return
		}
		w.render(rw, http.StatusOK, "", true)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			w.render(rw, http.StatusBadRequest, "Bad request.", false)
			return
		}
		if !w.valid(r.PostForm.Get("token")) {
			w.render(rw, http.StatusForbidden, "Forbidden: the link is invalid, expired or already used.", false)
			return
		}
		w.submit(rw, r)
	default:
		w.render(rw, http.StatusMethodNotAllowed, "Method not allowed.", false)
	}
}

func (w *Web) submit(rw http.ResponseWriter, r *http.Request) {
	answers := map[string]string{}
	var secrets []string
	for _, q := range Questions() {
		v := r.PostForm.Get(q.ID)
		if v == "" {
			continue
		}
		// "-" and "@path" are terminal indirections; never let the browser
		// make the CLI read stdin or a local file.
		if v == "-" || strings.HasPrefix(v, "@") {
			w.render(rw, http.StatusBadRequest, q.ID+": values starting with '@' or equal to '-' are not accepted here.", true)
			return
		}
		if q.Secret {
			secrets = append(secrets, v)
		}
		answers[q.ID] = v
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.used {
		w.render(rw, http.StatusForbidden, "Forbidden.", false)
		return
	}
	o := w.Opts
	o.Answers, o.Ask, o.Stdin = answers, nil, nil
	if err := Run(o); err != nil {
		msg := err.Error()
		for _, s := range secrets {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
		w.render(rw, http.StatusBadRequest, msg, true)
		return
	}
	w.used = true
	close(w.done)
	w.render(rw, http.StatusOK, "Written. The wizard has stopped; you can close this tab.", false)
}

// Serve listens on addr (127.0.0.1 only), calls announce with the one-time
// link, and returns after a successful write, expiry, or ctx cancellation.
func (w *Web) Serve(ctx context.Context, addr string, ttl time.Duration, announce func(link string)) error {
	if err := CheckLoopback(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	w.Host = ln.Addr().String()
	srv := &http.Server{Handler: w, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: nil}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	announce("http://" + w.Host + "/?t=" + w.Token)
	var result error
	select {
	case <-w.done:
	case <-time.After(ttl):
		result = errors.New("vizra setup --web: link expired with nothing written")
	case <-ctx.Done():
		result = ctx.Err()
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return result
}
