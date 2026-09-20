package jobs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The `vizra-core` contract claims that the worker "applies safeError at EVERY
// error log site". A sentence in AGENTS.md is not enforcement — the first round
// of this slice made exactly that claim while 8 of 11 sites still passed
// unredacted text, and it took an independent verifier to count them.
//
// So this test counts them, mechanically, on every run. It walks worker.go's
// AST and asserts that the value of every `"error"` attribute passed to a slog
// call is a `safeError(...)` call.
//
// Why every site and not only the handler-originated ones: the other eight
// carry pgx driver errors, and the argument that those are safe rests on how a
// THIRD-PARTY library formats its errors today. `postgres://user:pw@host` in a
// dial failure is one upstream change away from being a credential in a log
// line, and safeError on a driver error is a no-op with no diagnostic cost. A
// mechanical rule that needs no argument is worth more than a correct argument
// that a future reader has to reconstruct.
func TestEveryErrorLogSiteInTheWorkerIsRedacted(t *testing.T) {
	const file = "worker.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	// The slog methods that take ...any key/value pairs.
	logMethods := map[string]bool{"Error": true, "Warn": true, "Info": true, "Debug": true}

	checked := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !logMethods[sel.Sel.Name] {
			return true
		}
		// Walk the variadic pairs looking for the literal key "error".
		for i, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit.Value != `"error"` {
				continue
			}
			if i+1 >= len(call.Args) {
				t.Errorf("%s: an \"error\" key with no value", fset.Position(lit.Pos()))
				continue
			}
			checked++
			value := call.Args[i+1]
			vc, ok := value.(*ast.CallExpr)
			if !ok {
				t.Errorf("%s: the \"error\" value is not a call; it must be safeError(...)",
					fset.Position(value.Pos()))
				continue
			}
			ident, ok := vc.Fun.(*ast.Ident)
			if !ok || ident.Name != "safeError" {
				t.Errorf("%s: the \"error\" value is %s(...), not safeError(...). "+
					"Every error text the worker logs must go through the same "+
					"truncate(obs.Redact(...)) path last_error takes, so the log is safe "+
					"whichever slog handler the process happened to install.",
					fset.Position(value.Pos()), exprName(vc.Fun))
			}
		}
		return true
	})

	// A guard against this test silently checking nothing — if someone renames
	// the attribute key or restructures the calls, the loop above would find no
	// sites and pass vacuously.
	const want = 11
	if checked != want {
		t.Fatalf("checked %d \"error\" log sites in %s, expected %d. If a site was "+
			"deliberately added or removed, update this number in the same commit — "+
			"a count that drifts silently is how the previous overclaim happened.",
			checked, file, want)
	}
	t.Logf("%d error log sites in %s, all going through safeError", checked, file)
}

func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	}
	return strings.TrimSpace("<expression>")
}
