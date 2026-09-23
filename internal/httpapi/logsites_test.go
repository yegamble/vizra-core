package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

// obsImportPath is the ONE redactor a log value may pass through.
const obsImportPath = "github.com/yegamble/vizra-core/internal/obs"

// slogArgLayout maps each slog method or function that writes a record, or
// binds attributes to a logger, to the index of its message argument (-1: it
// has none) and the index of its first key/value argument.
var slogArgLayout = map[string]struct{ msg, kv int }{
	"Debug": {0, 1}, "Info": {0, 1}, "Warn": {0, 1}, "Error": {0, 1},
	"DebugContext": {1, 2}, "InfoContext": {1, 2}, "WarnContext": {1, 2}, "ErrorContext": {1, 2},
	"Log": {2, 3}, "LogAttrs": {2, 3},
	"With": {-1, 0},
}

// logSiteReport is what checkLogSites found.
type logSiteReport struct {
	sites    int      // slog calls examined
	problems []string // one line per violation, with its position
}

// checkLogSites enforces the httpapi logging rule over parsed source:
//
//   - every slog call's message is a string literal, so no error text or
//     request value can ride in on fmt.Sprintf;
//   - every key is a string literal, which also rules out slog.Attr
//     constructors (slog.Any("error", err) hides a raw value behind a call);
//   - every VALUE is a literal or an obs.Redact(...) call, where `obs` is the
//     local name of github.com/yegamble/vizra-core/internal/obs in that file;
//   - these other ways out are refused by name: the standard "log" import,
//     fmt.Print*/Fprint*, the print/println builtins, os.Stdout/os.Stderr,
//     os.NewFile, slog.NewLogLogger, slog.NewRecord, Record.AddAttrs,
//     Handler.WithAttrs, Handler().Handle(...), a logger method taken as a
//     VALUE (g := l.Error), and panic with a non-literal argument (net/http
//     writes a handler's panic value to its own unredacted stderr logger);
//   - nothing in the file declares a name equal to the internal/obs import's
//     local name, so `obs.Redact` cannot be a shadowing look-alike.
//
// Logger calls are recognised by METHOD NAME, not by type: type-checking this
// package from source takes about fifteen seconds (measured on go1.27.1), and a
// name match fails CLOSED — a non-logger method that happens to share a name is
// reported, never silently skipped, and a renamed logger variable is still
// caught.
//
// What it CANNOT see, and does not claim to (AGENTS.md states the same): a
// logger handed to a helper in ANOTHER package that logs raw values there; a
// write through any channel not named above; and anything reached through
// reflection or unsafe. The site count below guards against the walker going
// blind; it cannot bound shapes the walker does not recognise.
func checkLogSites(fset *token.FileSet, files []*ast.File) logSiteReport {
	var r logSiteReport
	bad := func(n ast.Node, format string, args ...any) {
		r.problems = append(r.problems, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, args...))
	}
	for _, f := range files {
		obsName := ""
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			switch path {
			case "log":
				bad(imp, `imports the standard "log" package, whose output bypasses the redacting slog handler; log through the injected *slog.Logger`)
			case obsImportPath:
				obsName = "obs"
				if imp.Name != nil {
					obsName = imp.Name.Name
				}
			}
		}
		isRedact := func(e ast.Expr) bool {
			call, ok := e.(*ast.CallExpr)
			if !ok || obsName == "" {
				return false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Redact" {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == obsName
		}

		// A declaration that shadows the obs import makes `obs.Redact` a
		// look-alike the value rule would accept.
		if obsName != "" {
			for _, id := range declaredNames(f) {
				if id.Name == obsName {
					bad(id, "declares %q, shadowing the internal/obs import; obs.Redact at a log site must be the real redactor", obsName)
				}
			}
		}

		// Selectors in call position, so a logger method taken as a VALUE can be
		// told apart from one that is called (and checked) where it stands.
		called := map[*ast.SelectorExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					called[sel] = true
				}
			}
			return true
		})

		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				x, _ := n.X.(*ast.Ident)
				switch {
				case x != nil && x.Name == "os" && (n.Sel.Name == "Stdout" || n.Sel.Name == "Stderr" || n.Sel.Name == "NewFile"):
					bad(n, "references os.%s; a write there bypasses the redacting logger", n.Sel.Name)
				case x != nil && x.Name == "slog" && (n.Sel.Name == "NewLogLogger" || n.Sel.Name == "NewRecord"):
					bad(n, "references slog.%s; a record or log.Logger built by hand bypasses this check", n.Sel.Name)
				case n.Sel.Name == "AddAttrs" || n.Sel.Name == "WithAttrs":
					bad(n, "calls %s; attributes attached to a record or handler by hand bypass this check", n.Sel.Name)
				case !called[n]:
					if _, isLog := slogArgLayout[n.Sel.Name]; isLog {
						bad(n, "takes the method %s as a value; a logger method called through a variable escapes this check", n.Sel.Name)
					}
				}
				return true
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
					bad(n, "calls the %s builtin, which writes to stderr unredacted", id.Name)
					return true
				}
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "panic" {
					for _, a := range n.Args {
						if _, lit := a.(*ast.BasicLit); !lit {
							bad(n, "panics with a non-literal value; net/http writes a handler's panic value to its own stderr logger, unredacted")
						}
					}
					return true
				}
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "Handle" {
					if inner, ok := sel.X.(*ast.CallExpr); ok {
						if isel, ok := inner.Fun.(*ast.SelectorExpr); ok && isel.Sel.Name == "Handler" {
							bad(n, "calls Handler().Handle directly; a record handed to the handler bypasses this check")
							return true
						}
					}
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fmt" &&
					(strings.HasPrefix(sel.Sel.Name, "Print") || strings.HasPrefix(sel.Sel.Name, "Fprint")) {
					bad(n, "calls fmt.%s; a formatted write bypasses the redacting logger", sel.Sel.Name)
					return true
				}
				layout, ok := slogArgLayout[sel.Sel.Name]
				if !ok || len(n.Args) == 0 {
					// err.Error() and friends take no arguments; every slog
					// method that writes a record takes at least a message.
					return true
				}
				r.sites++
				if layout.msg >= 0 {
					if layout.msg >= len(n.Args) {
						bad(n, "%s(...) has no message argument", sel.Sel.Name)
						return true
					}
					if !isStringLit(n.Args[layout.msg]) {
						bad(n.Args[layout.msg], "the message of %s(...) is not a string literal; put variable text in a key/value pair wrapped in obs.Redact", sel.Sel.Name)
					}
				}
				kv := n.Args[min(layout.kv, len(n.Args)):]
				for i := 0; i < len(kv); i += 2 {
					if !isStringLit(kv[i]) {
						bad(kv[i], "argument %d of %s(...) is not a string-literal key; slog.Attr constructors and computed keys hide a raw value from this check — use \"key\", obs.Redact(value)", layout.kv+i, sel.Sel.Name)
						continue
					}
					if i+1 >= len(kv) {
						bad(kv[i], "the key %s of %s(...) has no value", kv[i].(*ast.BasicLit).Value, sel.Sel.Name)
						continue
					}
					v := kv[i+1]
					if _, lit := v.(*ast.BasicLit); lit || isRedact(v) {
						continue
					}
					bad(v, "the value of %s in %s(...) is neither a literal nor obs.Redact(...); every error and request-derived value must pass through the redactor at the call site", kv[i].(*ast.BasicLit).Value, sel.Sel.Name)
				}
			}
			return true
		})
	}
	return r
}

// declaredNames returns every identifier the file DECLARES: functions, types,
// package and local var/const, := assignments (a type switch's included),
// range variables, and function parameters, results and receivers. Struct
// fields are not declarations in this sense: they are reached through a
// selector and cannot shadow an import.
func declaredNames(f *ast.File) []*ast.Ident {
	var out []*ast.Ident
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fd := range fl.List {
			out = append(out, fd.Names...)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			out = append(out, n.Name)
			fields(n.Recv)
		case *ast.FuncType:
			fields(n.Params)
			fields(n.Results)
		case *ast.TypeSpec:
			out = append(out, n.Name)
		case *ast.ValueSpec:
			out = append(out, n.Names...)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						out = append(out, id)
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						out = append(out, id)
					}
				}
			}
		}
		return true
	})
	return out
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

// parsePackageSources parses every non-test Go file in this directory. A test
// file is excluded because the rule governs what the API logs in production.
func parsePackageSources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		files = append(files, f)
		names = append(names, n)
	}
	sort.Strings(names)
	if len(files) == 0 {
		t.Fatal("found no production Go files in internal/httpapi; this test would check nothing")
	}
	t.Logf("checked files: %s", strings.Join(names, ", "))
	return fset, files
}

// The twin of TestEveryErrorLogSiteInTheWorkerIsRedacted, for the API process
// (security review of core #8: N-7, NEW-2, F-1).
//
// The API's logger is whatever Deps.Logger is, and New falls back to
// slog.Default() — which is NOT the redacting handler unless the process
// happened to install one. So "the handler redacts" is not an argument this
// package may rely on: every value is redacted where it is logged, and this
// test counts that on every run, the way the worker's test does. It is stricter
// than the worker's in one respect: it covers EVERY value, not only the "error"
// key. A request path, a method, a constraint name and a request id are all
// request- or error-derived, and deciding case by case which of them "cannot"
// carry a secret is the argument the worker's history shows goes stale.
func TestEveryLogSiteInTheAPIIsRedacted(t *testing.T) {
	fset, files := parsePackageSources(t)
	r := checkLogSites(fset, files)
	for _, p := range r.problems {
		t.Error(p)
	}

	// A guard against checking nothing: if the calls are restructured so the
	// walker no longer recognises them, the loop above finds no sites and
	// passes vacuously. If a log site was deliberately added or removed, change
	// this number in the same commit.
	const want = 7
	if r.sites != want {
		t.Fatalf("examined %d slog call sites in internal/httpapi, expected %d. If a site was "+
			"deliberately added or removed, update this number in the same commit — a count "+
			"that drifts silently is how the worker's overclaim happened.", r.sites, want)
	}
	t.Logf("%d slog call sites in internal/httpapi, every value a literal or obs.Redact(...)", r.sites)
}

// The checker must actually catch what it claims to. Each planted source below
// is a way a raw value has reached, or could reach, a log line; each must be
// reported. The clean source must not be.
func TestTheLogSiteCheckerCatchesPlantedUnredactedCalls(t *testing.T) {
	const header = `package httpapi
import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"github.com/yegamble/vizra-core/internal/obs"
)
var _ = obs.Redact
var _ = fmt.Sprint
var _ = os.Getenv
var _ context.Context
`
	planted := map[string]string{
		"raw err.Error() value (N-7 as found)":  `func f(log *slog.Logger, err error) { log.Error("m", "error", err.Error()) }`,
		"raw error value":                       `func f(log *slog.Logger, err error) { log.Error("m", "error", err) }`,
		"raw request path":                      `func f(log *slog.Logger, p string) { log.Warn("m", "path", p) }`,
		"raw value on a renamed logger":         `func f(l *slog.Logger, err error) { l.Info("m", "e", err) }`,
		"raw value on a struct-field logger":    `type s struct{ Logger *slog.Logger }; func (x s) f(id string) { x.Logger.Debug("m", "request_id", id) }`,
		"package-level slog function":           `func f(err error) { slog.Error("m", "error", err) }`,
		"context variant":                       `func f(ctx context.Context, l *slog.Logger, err error) { l.ErrorContext(ctx, "m", "error", err) }`,
		"Log with a level":                      `func f(ctx context.Context, l *slog.Logger, err error) { l.Log(ctx, slog.LevelError, "m", "error", err) }`,
		"attrs bound with With":                 `func f(l *slog.Logger, p string) { l.With("path", p).Info("m") }`,
		"slog.Attr constructor hides the value": `func f(l *slog.Logger, err error) { l.Error("m", slog.Any("error", err)) }`,
		"LogAttrs":                              `func f(ctx context.Context, l *slog.Logger, err error) { l.LogAttrs(ctx, slog.LevelError, "m", slog.String("error", err.Error())) }`,
		"error text in the message":             `func f(l *slog.Logger, err error) { l.Error(fmt.Sprintf("failed: %v", err)) }`,
		"redacted by some other function":       `func redact(s string) string { return s }; func f(l *slog.Logger, err error) { l.Error("m", "error", redact(err.Error())) }`,
		"fmt.Fprintln to stderr":                `func f(err error) { fmt.Fprintln(os.Stderr, err) }`,
		"println builtin":                       `func f(err error) { println(err.Error()) }`,
		"a key with no value":                   `func f(l *slog.Logger) { l.Error("m", "error") }`,

		// B3 verifier, core #12 V-2: shapes that escaped the by-name walk.
		"a logger method value":                      `func f(l *slog.Logger, p string) { g := l.Error; g("m", "path", p) }`,
		"Handler().Handle with a hand-built record":  `func f(ctx context.Context, l *slog.Logger, p string) { var r slog.Record; r.Add("path", p); _ = l.Handler().Handle(ctx, r) }`,
		"Record.AddAttrs":                            `func f(p string) { var r slog.Record; r.AddAttrs(slog.String("path", p)) }`,
		"Handler().WithAttrs with a raw attribute":   `func f(l *slog.Logger, p string) { slog.New(l.Handler().WithAttrs([]slog.Attr{slog.String("path", p)})).Info("m") }`,
		"slog.NewRecord":                             `func f() { _ = slog.NewRecord }`,
		"slog.NewLogLogger":                          `func f(l *slog.Logger, p string) { slog.NewLogLogger(l.Handler(), slog.LevelError).Printf("m %s", p) }`,
		"os.NewFile(2, ...)":                         `func f(p string) { _, _ = os.NewFile(2, "e").WriteString(p) }`,
		"panic with a non-literal argument":          `func f(p string) { panic("request failed: " + p) }`,
		"a local obs shadowing the import":           `type fake struct{}; func (fake) Redact(s string) string { return s }; func f(l *slog.Logger, p string) { obs := fake{}; l.Error("m", "path", obs.Redact(p)) }`,
		"a parameter named obs shadowing the import": `type fake struct{}; func (fake) Redact(s string) string { return s }; func f(l *slog.Logger, obs fake, p string) { l.Error("m", "path", obs.Redact(p)) }`,
		"a var named obs shadowing the import":       `type fake struct{}; func (fake) Redact(s string) string { return s }; func f(l *slog.Logger, p string) { var obs fake; l.Error("m", "path", obs.Redact(p)) }`,
	}
	for name, body := range planted {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "planted.go", header+body, 0)
			if err != nil {
				t.Fatalf("the planted source does not parse: %v", err)
			}
			if r := checkLogSites(fset, []*ast.File{f}); len(r.problems) == 0 {
				t.Fatalf("the checker accepted a planted unredacted call:\n%s", body)
			}
		})
	}

	t.Run("the standard log package", func(t *testing.T) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "planted.go", "package httpapi\nimport \"log\"\nfunc f(err error) { log.Print(err) }\n", 0)
		if err != nil {
			t.Fatal(err)
		}
		if r := checkLogSites(fset, []*ast.File{f}); len(r.problems) == 0 {
			t.Fatal(`the checker accepted an import of the standard "log" package`)
		}
	})

	t.Run("an obs name that is not the redactor", func(t *testing.T) {
		// A local package or variable called `obs` is not the redactor.
		src := "package httpapi\nimport (\n\"log/slog\"\nobs \"example.com/notobs\"\n)\n" +
			`func f(l *slog.Logger, err error) { l.Error("m", "error", obs.Redact(err.Error())) }`
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "planted.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r := checkLogSites(fset, []*ast.File{f}); len(r.problems) == 0 {
			t.Fatal("the checker accepted obs.Redact from a package that is not internal/obs")
		}
	})

	t.Run("clean source is accepted", func(t *testing.T) {
		src := header + `func f(ctx context.Context, l *slog.Logger, err error, p string) {
	l.Error("m", "error", obs.Redact(err.Error()), "path", obs.Redact(p), "n", 3)
	l.WarnContext(ctx, "m", "k", "literal")
	l.Info("no attributes")
	_ = err.Error()
	if false {
		panic("unreachable: a literal panic carries no request value")
	}
}`
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "clean.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		r := checkLogSites(fset, []*ast.File{f})
		if len(r.problems) != 0 {
			t.Fatalf("the checker rejected clean source: %v", r.problems)
		}
		if r.sites != 3 {
			t.Fatalf("the checker examined %d sites in the clean source, want 3", r.sites)
		}
	})
}

// The behaviour N-7 / F-1 asked for: a 500's cause is logged redacted by the
// handler itself, whatever slog handler the process installed. The logger here
// is a PLAIN JSON handler, deliberately not obs.NewLogger — that is what
// New(Deps{}) gets from slog.Default() when no logger is injected.
func TestTheErrorHandlerRedactsTheCauseOfA500(t *testing.T) {
	const (
		dsnPassword  = "hunter2-dsn-password"
		amzSignature = "deadbeefcafef00d0123456789"
		amzCred      = "AKIAEXAMPLEKEY0000"
		apiKey       = "vzk_live0123456789abcdefghij"
		requestID    = "01996a3c-6b4e-7c1a-9d2e-3f4a5b6c7d8e"
	)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/things/"+apiKey, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(headerRequestID, requestID)

	cause := errors.New("store: failed to connect to postgres://vizra:" + dsnPassword +
		"@db:5432/vizra; then fetched https://bucket.example/o.jpg?X-Amz-Credential=" + amzCred +
		"&X-Amz-Signature=" + amzSignature)
	errorHandler(logger)(c, cause)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "http: request failed") {
		t.Fatalf("the 500 was not logged at all; this test would pass vacuously. Log:\n%s", logged)
	}
	for name, secret := range map[string]string{
		"the DSN password":         dsnPassword,
		"the presigned signature":  amzSignature,
		"the presigned credential": amzCred,
		"the API key in the path":  apiKey,
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("%s reached the log line:\n%s", name, logged)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s reached the client body:\n%s", name, rec.Body.String())
		}
	}
	// Redacted, not dropped: the operator still gets something to act on.
	for _, want := range []string{"[redacted]", "failed to connect", "db:5432", requestID, "/api/v1/things/", `"method":"GET"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log line lost %q, which is diagnostic and not secret:\n%s", want, logged)
		}
	}
}

// The same handler, one 500 per credential form the B3 verifier measured
// leaking through it (core #12, V-1), under a PLAIN JSON handler. Each cause is
// the shape a real error would take: a driver or client error quoting its
// connection string or request URL.
func TestTheErrorHandlerRedactsEveryCredentialFormInA500(t *testing.T) {
	s := func(tag string) string { return "Zq" + tag + strings.Repeat("k3", 6) }
	for _, tc := range []struct{ name, cause, secret string }{
		{"redis URL with an empty username", "cache: dial redis://:" + s("a") + "@cache:6379/0: refused", s("a")},
		{"keyword/value DSN password", "db: connect host=db user=vizra password=" + s("b") + " dbname=vizra: refused", s("b")},
		{"?password= in a URL", "db: open postgres://db/vizra?password=" + s("c") + ": refused", s("c")},
		{"GCS X-Goog-Signature", "storage: GET https://storage.example/b/o?X-Goog-Signature=" + s("d") + ": 403", s("d")},
		{"GCS X-Goog-Credential", "storage: GET https://storage.example/b/o?X-Goog-Credential=" + s("e") + ": 403", s("e")},
		{"?api_key=", "upstream: GET https://api.example/v1?api_key=" + s("f") + ": 500", s("f")},
		{"&key=", "upstream: GET https://api.example/v1?q=1&key=" + s("g") + ": 500", s("g")},
		{"?access_token=", "upstream: GET https://api.example/me?access_token=" + s("h") + ": 401", s("h")},
		{"?claim_token=", "setup: replay of /api/v1/setup/claim-owner?claim_token=" + s("i") + " failed", s("i")},
		{"session cookie value", "session: Cookie: vizra_session=" + s("j") + "; theme=dark could not be decoded", s("j")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/v1/x", nil), rec)
			errorHandler(slog.New(slog.NewJSONHandler(&buf, nil)))(c, errors.New(tc.cause))
			logged := buf.String()
			if !strings.Contains(logged, "http: request failed") {
				t.Fatalf("the 500 was not logged; this case would pass vacuously:\n%s", logged)
			}
			if strings.Contains(logged, tc.secret) {
				t.Errorf("the credential reached the log line:\n%s", logged)
			}
			if !strings.Contains(logged, "[redacted]") {
				t.Errorf("nothing was marked redacted:\n%s", logged)
			}
		})
	}
}
