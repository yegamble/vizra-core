package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Queue 2p (core B5d) — the Makefile LINE GRAMMAR is an allowlist, and there is
// ONE line reader.
//
// Before this slice the anchor refused a NAMED list of constructs before make
// and relied on make's own database after it. A list of names is a blacklist:
// search's closing re-verification (meta
// docs/evidence/warroom/2026-09-23-vizra-search-pr5-closing-VERIFY.md) found
// lines two readers split differently and spellings no name covered. So every
// logical line of every pinned makefile must now be one of five shapes —
// empty or a column-0 comment, a column-0 literal assignment, `.PHONY:`, a
// single-literal-target rule with literal prerequisites, or a TAB recipe line
// using only `$$` and `$(NAME)` — and anything else is refused by line number
// BEFORE make. The code is vizra-search's (scripts/makegate.py at 4810048),
// ported into scripts/makefile_pin.py; so are these tests' ideas.
//
// Every row is inert: an appended line of a copy of the real Makefile,
// re-pinned the way a reviewer's paired pin update would be. None is run —
// the recorder proves make is not started.
// ---------------------------------------------------------------------------

// directiveKeywords are GNU Make's directive words. None may be an assigned
// variable NAME or a rule target: some make versions read `ifdef := 1` as the
// directive, so the grammar would see an assignment where make sees a
// conditional (search FINDING 15).
var directiveKeywords = []string{
	"ifdef", "ifndef", "ifeq", "ifneq", "else", "endif", "include", "-include", "sinclude",
	"define", "endef", "export", "unexport", "override", "private", "undefine", "vpath", "load",
}

type grammarRow struct{ name, add, want string }

// outOfGrammarRows are every out-of-grammar form vizra-search's table covers,
// with core's inert names. want is the reason the refusal must name; every
// row must ALSO say "outside the Makefile grammar".
func outOfGrammarRows() []grammarRow {
	const og = "outside the makefile grammar"
	rows := []grammarRow{
		// search FINDING 9: an inline `;` recipe, whatever it holds.
		{"inline recipe holding = (-run=Foo)", "\ninert-target: ; -go test -run=Foo ./...\n", og},
		{"inline recipe with a prerequisite, holding X=1", "\ninert-target: dep ; -false X=1\n", og},
		{"inline recipe echo a=b", "\ninert-target: ; @echo a=b\n", og},
		// FINDING 10: a conditional inside a recipe.
		{"a non-TAB line inside a conditional within a recipe", "\ninert-target:\n\ttrue\nifeq (a,b)\nINERT := 1\nendif\n\t-false\n", "conditional directive"},
		{"a conditional within a recipe", "\ninert-target:\n\ttrue\nifndef VIZRA_NEVER_SET\n\t-true\nendif\n", "conditional directive"},
		// FINDING 11: a modifier word before a rule line.
		{"private rule line with an inline recipe", "\nprivate inert-target: ; -true\n", og},
		{"override rule line with an inline recipe", "\noverride inert-target: ; -true\n", og},
		{"private rule line with a TAB recipe", "\nprivate inert-target:\n\t-true\n", og},
		{"undefine rule line", "\nundefine inert-target: ; -true\n", og},
		{"load rule line", "\nload inert-target: ; -true\n", og},
		// FINDING 12: a partly computed function name.
		{"call with a partly computed function name", "\nINERT := $(call ev$(A)al,x)\n", og},
		// The directives and forms outside the five shapes.
		{"include", "\ninclude inert.mk\n", "the `include` directive"},
		{"-include", "\n-include inert.mk\n", "the `-include` directive"},
		{"sinclude", "\nsinclude inert.mk\n", "the `sinclude` directive"},
		{"define", "\ndefine INERT\nx\nendef\n", "the `define` directive"},
		{"export", "\nexport INERT\n", "the `export` directive"},
		{"export with an assignment", "\nexport INERT := 1\n", "the `export` directive"},
		{"unexport", "\nunexport INERT\n", "the `unexport` directive"},
		{"override assignment", "\noverride INERT := 1\n", "the `override` directive"},
		{"vpath", "\nvpath %.c src\n", "the `vpath` directive"},
		{"a conditional", "\nifeq (a,b)\nendif\n", "conditional directive"},
		{"ifdef", "\nifdef VZ_B5D_IFDEF\nendif\n", "conditional directive"},
		{"a second colon", "\ninert-target: a: b\n", og},
		{"a double-colon rule", "\ninert-target:: a\n", og},
		{"a pattern rule", "\n%.o: %.c\n", og},
		{"a pattern-specific assignment", "\n%: INERT := bar\n", og},
		{"a target-specific assignment", "\ninert-target: INERT := bar\n", og},
		{"a special target (.SILENT)", "\n.SILENT:\n", og},
		{"a += assignment", "\nINERT += 1\n", og},
		{"a != assignment", "\nINERT != true\n", og},
		{"a TAB line outside a rule", "\nINERT := 1\n\t-true\n", og},
		{"a # inside $(shell …)", "\nINERT := $(shell echo # x)\n", og},
		{"a substitution reference in a value", "\nINERT := $(PKG:a=b)\n", og},
		{"a function in a value", "\nINERT := $(foreach f,a,b)\n", og},
		{"$(eval …) in a value", "\nINERT := $(eval INERT2 := 1)\n", og},
		{"$(file …) in a value", "\nINERT := $(file <inert)\n", og},
		{"$X in a recipe", "\ninert-target:\n\techo $X\n", og},
		{"$@ at the start of a recipe", "\ninert-target:\n\t$@-is-not-run\n", og},
		{"$(shell …) in a recipe", "\ninert-target:\n\techo $(shell true)\n", og},
		{"a function at the start of a recipe", "\ninert-target:\n\t$(if yes,-)true\n", og},
		{"$V in a value", "\nINERT := $Q\n", og},
		{"$(origin V) in a value", "\nINERT := $(origin VZ_B5D_ORIGIN)\n", og},
		{"$(value V) in a value", "\nINERT := $(value VZ_B5D_VALUE)\n", og},
		{"a computed variable name", "\n$(M)AKEFLAGS := x\n", og},
		{"a computed rule target", "\n$(I)ORE: inert\n", og},
		{"a computed prerequisite", "\ninert-target: $(LANE)\n", og},
		{"a rule line that starts with whitespace", "\n  inert-target:\n\t-true\n", og},
		{"a rule line continued with a backslash", "\ninert-target \\\n  :\n\t-true\n", "a rule line continued with a backslash"},
		{"a .PHONY line continued with a backslash", "\n.PHONY: inert-a \\\n  inert-b\n", "a rule line continued with a backslash"},
		{"two targets on one rule line", "\ninert-target ci:\n\t-true\n", og},
		{"grouped targets &:", "\ninert-a inert-b &:\n\t@true\n", og},
		{"a line of only a word", "\ninert\n", og},
		// FINDING 14: where a line ENDS must be read the same way by every reader.
		{"a comment continued into a rule line", "\n# note \\\ninert:\n\t-false\n", "a comment continued onto the next line"},
		{"a rule line whose trailing comment is continued", "\ninert-target: # c \\\ninert:\n\t-false\n", "a comment continued onto the next line"},
		{"a lone CR inside a comment", "\n# note\rX := 1\n", "a carriage return"},
		{"CRLF line endings", "\ninert-target:\r\n\ttrue\r\n", "a carriage return"},
		{"a NUL inside a comment", "\n# inert\x00 comment\n", "a nul byte"},
		{"an NBSP-only line", "\n\u00a0\n", "a non-ascii whitespace character (u+00a0)"},
		{"a form feed", "\nINERT := 1\x0c\n", "a control character (u+000c)"},
		{"a right-to-left override in a comment", "\n# inert \u202e\n", "an invisible format character (u+202e)"},
		{"a line of only spaces", "\n   \n", "only spaces or tabs"},
		{"a directive keyword as a rule target", "\nifdef: inert\n", "a directive keyword (`ifdef`) as a rule target"},
	}
	for _, kw := range directiveKeywords {
		rows = append(rows, grammarRow{kw + " as a variable name", "\n" + kw + " := 1\n",
			"a directive keyword (`" + kw + "`) as a variable name"})
	}
	return rows
}

// grammarControls fit the grammar: the anchor runs make on them and is green,
// so the refusals above are not a grammar that refuses every appended line.
var grammarControls = []grammarRow{
	{"control: an assignment using $(NAME) and ${NAME}", "\nINERT := $(PKGS) ${GO}\n", ""},
	{"control: a continued assignment", "\nINERT := a \\\n  b # c\n", ""},
	{"control: a rule with literal prerequisites and a $$ / $(NAME) recipe", "\n.PHONY: inert-target\ninert-target: vet\n\t@echo $$HOME $(PKGS) >/dev/null\n", ""},
	{"control: a comment and an empty line", "\n# an inert comment\n\n", ""},
}

// Every out-of-grammar row, appended to a copy of the REAL Makefile and
// re-pinned: refused by line number with make not started (recorded), in the
// anchor's strict mode; and refused by check 11, which calls the same
// verify_pin. The controls reach make and are green.
func TestEveryOutOfGrammarLineIsRefusedBeforeMake(t *testing.T) {
	for _, tc := range append(outOfGrammarRows(), grammarControls...) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := copyRealTree(t)
			if err := appendBytes(filepath.Join(dir, "Makefile"), tc.add); err != nil {
				t.Fatal(err)
			}
			if err := repin(dir, "Makefile"); err != nil {
				t.Fatal(err)
			}
			out, rec := runRecorded(t, dir, true)
			gout, gcode := run(t, "ci-required-guard.sh", "--makefile-pins", filepath.Join(dir, ".github", "pinned-makefiles.yml"))
			if tc.want == "" {
				assertGreen(t, tc.name, out, rec)
				if gcode != 0 {
					t.Fatalf("%s: check 11 refused a line that fits the grammar: exit %d\n%s", tc.name, gcode, gout)
				}
				return
			}
			low := strings.ToLower(out)
			if rec.Exit != 1 || !strings.Contains(low, "outside the makefile grammar") || !strings.Contains(low, tc.want) {
				t.Fatalf("%s: exit %d; want a grammar refusal naming %q:\n%s", tc.name, rec.Exit, tc.want, out)
			}
			if n := rec.makeCalls(); n != 0 || !strings.Contains(out, "make was NOT invoked (0 make process(es) started)") {
				t.Fatalf("%s: make was started %d time(s) after a grammar refusal:\n%s", tc.name, n, out)
			}
			glow := strings.ToLower(gout)
			if gcode != 1 || !strings.Contains(glow, "outside the makefile grammar") || !strings.Contains(glow, tc.want) {
				t.Fatalf("%s: check 11 exit %d; want the same grammar refusal:\n%s", tc.name, gcode, gout)
			}
			line := ""
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(strings.ToLower(l), "outside the makefile grammar") {
					line = strings.TrimSpace(l)
					break
				}
			}
			t.Logf("%s | anchor exit 1, 0 make processes (recorded) | check 11 exit 1 | %s", tc.name, line)
		})
	}
}

// The real Makefile fits unchanged, and has a line of every shape, so this
// test shows the grammar reads each shape rather than merely never meeting it.
func TestTheRealMakefileFitsTheGrammar(t *testing.T) {
	prog := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("makefile_pin", sys.argv[1] + "/scripts/makefile_pin.py")
mp = importlib.util.module_from_spec(spec)
sys.modules["makefile_pin"] = mp  # its dataclasses resolve their module through sys.modules
spec.loader.exec_module(mp)
problems = mp.grammar_problems("Makefile", mp.read_makefile_text(sys.argv[1] + "/Makefile"))
print(json.dumps({"problems": problems, "shapes": mp.grammar_problems.last_shapes}))
`
	root := repoRoot(t)
	cmd := exec.Command("python3", "-c", prog, root)
	cmd.Env = guardEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the grammar check did not run: %v\n%s", err, out)
	}
	var got struct {
		Problems []string       `json:"problems"`
		Shapes   map[string]int `json:"shapes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unreadable result: %v\n%s", err, out)
	}
	if len(got.Problems) != 0 {
		t.Fatalf("the real Makefile does not fit the grammar:\n%s", strings.Join(got.Problems, "\n"))
	}
	for _, shape := range []string{"blank/comment", "assignment", "phony", "rule", "recipe"} {
		if got.Shapes[shape] == 0 {
			t.Errorf("the real Makefile has no %q line, so this test does not show the grammar reads that shape: %v", shape, got.Shapes)
		}
	}
	if !t.Failed() {
		t.Logf("the real Makefile fits the grammar: %v", got.Shapes)
	}
}

type grammarCase struct{ name, text, want string }

// misreadLineCases are inert strings the grammar must refuse BY NAME: every
// place two readers could disagree on where a line ends, and the directive
// keywords as names. Nothing here is written to a Makefile or run.
func misreadLineCases() []grammarCase {
	cases := []grammarCase{
		{"a comment continued into a rule line", "ci:\n\tgo test\n# note \\\ninert:\n\t-false\n", "a comment continued onto the next line"},
		{"a rule line whose trailing comment is continued", "ci: # c \\\ninert:\n\t-false\n", "a comment continued onto the next line"},
		{"an assignment whose trailing comment is continued", "INERT := 1 # c \\\ninert:\n\t-false\n", "a comment continued onto the next line"},
		{"a lone CR inside a comment", "ci:\n\tgo test\n# note\rX := 1\n\t-false\n", "a carriage return"},
		{"CRLF line endings", "ci:\r\n\tgo test\r\n", "a carriage return"},
		{"CRLF after a comment's trailing backslash", "ci:\n\tgo test\n# note \\\r\ninert:\n\t-false\n", "a carriage return"},
		{"a NUL inside a comment", "ci:\n\tgo test\n# note\x00\ninert:\n\t-false\n", "a nul byte"},
		{"an NBSP-only line", "ci:\n\tgo test\n\u00a0\n\t-false\n", "a non-ascii whitespace character (u+00a0)"},
		{"blank means empty: a line of only spaces", "ci:\n\tgo test\n   \n\t-false\n", "only spaces or tabs"},
		{"blank means empty: a TAB-only line", "ci:\n\tgo test\n\t\n", "only spaces or tabs"},
		{"a form feed", "INERT := 1\x0c\n", "a control character (u+000c)"},
		{"a right-to-left override in a comment", "# inert \u202e\n", "an invisible format character (u+202e)"},
		{"a directive keyword as a rule target", "ifdef: inert\n", "a directive keyword (`ifdef`) as a rule target"},
	}
	for _, kw := range directiveKeywords {
		cases = append(cases, grammarCase{kw + " as a variable name", kw + " := 1\n",
			"a directive keyword (`" + kw + "`) as a variable name"})
	}
	return cases
}

// acceptedLineCases are lines every reader reads the same way, which the
// grammar must NOT refuse.
var acceptedLineCases = []grammarCase{
	{"a comment line that is not continued, between recipe lines", "ci:\n\tgo test\n# note\n\t-false\n", ""},
	{"an EVEN run of backslashes ending a comment (not a continuation)", "# note \\\\\ninert:\n", ""},
	{"a continued assignment whose comment is on its last line", "INERT := a \\\n  b # c\n", ""},
	{"a continued recipe line", "ci:\n\tgo test \\\n\t  -v\n", ""},
	{"a recipe line holding # and a continuation (make hands it to the shell)", "ci:\n\techo a # b \\\n\techo c\n", ""},
	{"an empty line inside a recipe", "ci:\n\tgo test\n\n\tgo vet\n", ""},
}

const grammarOnStrings = `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("makefile_pin", sys.argv[1] + "/scripts/makefile_pin.py")
mp = importlib.util.module_from_spec(spec)
sys.modules["makefile_pin"] = mp  # its dataclasses resolve their module through sys.modules
spec.loader.exec_module(mp)
cases = json.load(open(sys.argv[2]))
print(json.dumps({c[0]: mp.grammar_problems("Makefile", c[1]) for c in cases}))
`

// Through the committed grammar function on inert strings: each misread line
// is refused by name, and each control is accepted. No make process.
func TestTheGrammarRefusesEveryLineReadersCouldSplitDifferently(t *testing.T) {
	refused := misreadLineCases()
	all := append(append([]grammarCase{}, refused...), acceptedLineCases...)
	var pairs [][2]string
	for _, c := range all {
		pairs = append(pairs, [2]string{c.name, c.text})
	}
	data, err := json.Marshal(pairs)
	if err != nil {
		t.Fatal(err)
	}
	casesFile := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(casesFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", grammarOnStrings, repoRoot(t), casesFile)
	cmd.Env = guardEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the grammar check did not run: %v\n%s", err, out)
	}
	var got map[string][]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unreadable result: %v\n%s", err, out)
	}
	for _, c := range refused {
		c := c
		t.Run(c.name, func(t *testing.T) {
			problems, ok := got[c.name]
			if !ok {
				t.Fatalf("no result for %q", c.name)
			}
			joined := strings.ToLower(strings.Join(problems, "\n"))
			if !strings.Contains(joined, c.want) || !strings.Contains(joined, "outside the makefile grammar") {
				t.Fatalf("%q: grammar_problems gave %q; want a refusal naming %q", c.text, problems, c.want)
			}
			t.Logf("%q | refused: %s", c.text, problems[0])
		})
	}
	for _, c := range acceptedLineCases {
		c := c
		t.Run("control: "+c.name, func(t *testing.T) {
			if problems := got[c.name]; len(problems) != 0 {
				t.Fatalf("%q: grammar_problems refused a line every reader reads the same way: %q", c.text, problems)
			}
		})
	}
}

// The anchor reads the recipe make reads: a TAB line after a continued comment
// belongs to `ci` for make, and for the one reader, even with the grammar's
// refusal of that comment set aside.
func TestTheAnchorReadsTheRecipeMakeReads(t *testing.T) {
	prog := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("mig", sys.argv[1] + "/scripts/make-integrity-guard.py")
mig = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mig)
mp = mig.mp
out = {}
for name, text in (("comment", "ci:\n\tgo test\n# note \\\ninert:\n\t-false\n"),
                   ("rule-comment", "ci: # c \\\ninert:\n\t-false\n"),
                   ("even-backslashes", "ci:\n\tgo test \\\\\n\t-false\n")):
    out[name] = mp.recipe_lines(mp.makefile_lines(text), 1)
print(json.dumps(out))
`
	cmd := exec.Command("python3", "-c", prog, repoRoot(t))
	cmd.Env = guardEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var got map[string][][]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unreadable: %v\n%s", err, out)
	}
	want := map[string]string{
		"comment":          `[[2,"go test"],[5,"-false"]]`,
		"rule-comment":     `[[3,"-false"]]`,
		"even-backslashes": `[[2,"go test \\\\"],[3,"-false"]]`,
	}
	for name, w := range want {
		b, _ := json.Marshal(got[name])
		if string(b) != w {
			t.Errorf("%s: the anchor's recipe for ci is %s; want %s (make's reading)", name, b, w)
		}
	}
}

type readerProbeResult struct {
	Probes []struct {
		Name       string   `json:"name"`
		CleanOK    bool     `json:"clean_ok"`
		PoisonedOK bool     `json:"poisoned_ok"`
		Detail     []string `json:"detail"`
	} `json:"probes"`
	Source   []string `json:"source"`
	Identity []struct {
		Text     string `json:"text"`
		Distinct int    `json:"distinct_sequences"`
	} `json:"identity"`
	ReadersSeen []string `json:"readers_seen"`
	NamedReads  int      `json:"named_reads"`
}

func runReaderProbe(t *testing.T, scriptsDir string) readerProbeResult {
	t.Helper()
	cmd := exec.Command("python3", filepath.Join(repoRoot(t), "scripts", "testdata", "one-reader-probe.py"),
		scriptsDir, t.TempDir())
	cmd.Env = guardEnv()
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("the reader probe did not run: %v\n%s\n%s", err, out, stderr)
	}
	var got readerProbeResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unreadable result: %v\n%s", err, out)
	}
	return got
}

// ONE line reader (chair, queue 2p): every text reading of the Makefile in the
// anchor, check 11 and ci-required-guard consumes makefile_pin.makefile_lines.
// POISON: rewriting the text inside makefile_lines changes every reader's
// verdict. SOURCE: no reader splits, decodes or reads text itself, and every
// such spelling in the three files is a named read of a non-makefile input.
// IDENTITY: every reader of one text receives the same sequence object.
func TestEveryMakefileReaderConsumesTheOneLineReader(t *testing.T) {
	got := runReaderProbe(t, filepath.Join(repoRoot(t), "scripts"))
	if len(got.Probes) < 15 {
		t.Fatalf("%d probe(s) ran; want every reader probed", len(got.Probes))
	}
	for _, p := range got.Probes {
		p := p
		t.Run("poison: "+p.Name, func(t *testing.T) {
			if !p.CleanOK {
				t.Fatalf("%s: the clean fixture did not give the clean verdict: %v", p.Name, p.Detail)
			}
			if !p.PoisonedOK {
				t.Fatalf("%s: rewriting the text inside makefile_lines did NOT change this reader's verdict, so it "+
					"reads the bytes some other way: %v", p.Name, p.Detail)
			}
		})
	}
	t.Run("source", func(t *testing.T) {
		for _, s := range got.Source {
			t.Error(s)
		}
		if got.NamedReads == 0 {
			t.Error("no named read was checked; the file-level SOURCE check read nothing")
		}
	})
	t.Run("identity", func(t *testing.T) {
		if len(got.Identity) == 0 {
			t.Fatal("no call of makefile_lines was recorded")
		}
		for _, id := range got.Identity {
			if id.Distinct != 1 {
				t.Errorf("text %s: its readers received %d different line sequences; want the same one", id.Text, id.Distinct)
			}
		}
		seen := map[string]bool{}
		for _, r := range got.ReadersSeen {
			seen[r] = true
		}
		for _, r := range []string{"grammar_problems", "static_read_set", "verify_pin", "prerequisite_closure",
			"check_text", "check_environment_overrides", "load_makefile_env_names", "check_makefile_selection",
			"check_makefile_pin"} {
			if !seen[r] {
				t.Errorf("%s never reached makefile_lines", r)
			}
		}
	})
	if !t.Failed() {
		t.Logf("%d reader probes changed verdict under a poisoned makefile_lines; no second splitter (%d named "+
			"non-makefile reads); one sequence per text (%d)", len(got.Probes), got.NamedReads, len(got.Identity))
	}
}

// plantedReaders are second readers of the Makefile, each planted into a copy
// of one of the three files as a helper nobody calls. The file-level SOURCE
// check must name each one. Inert: nothing calls them.
var plantedReaders = []struct{ name, file, code, want string }{
	{"read_text + split in the anchor", "make-integrity-guard.py",
		"\n\ndef _planted_reader(root):\n    return (root / \"Makefile\").read_text().split(\"\\n\")\n",
		"make-integrity-guard.py: _planted_reader uses read_text"},
	{"open + iterate in ci-required-guard", "ci-required-guard.py",
		"\n\ndef _planted_reader(path):\n    with open(path) as fh:\n        return [line for line in fh]\n",
		"ci-required-guard.py: _planted_reader uses open"},
	{"a multi-line regex over the Makefile in ci-required-guard", "ci-required-guard.py",
		"\n\ndef _planted_reader(text):\n    return re.findall(r\"^ci:\", text, re.M)\n",
		"ci-required-guard.py: _planted_reader uses re.m"},
	{"an inline (?m) flag in the anchor", "make-integrity-guard.py",
		"\n\ndef _planted_reader(text):\n    return re.findall(r\"(?m)^ci:\", text)\n",
		"make-integrity-guard.py: _planted_reader uses re.m"},
	{"read_bytes + decode in makefile_pin", "makefile_pin.py",
		"\n\ndef _planted_reader(path):\n    return Path(path).read_bytes().decode()\n",
		"makefile_pin.py: _planted_reader uses decode"},
	{"splitlines in makefile_pin", "makefile_pin.py",
		"\n\ndef _planted_reader(text):\n    return text.splitlines()\n",
		"makefile_pin.py: _planted_reader uses splitlines"},
	{"an aliased open in the anchor", "make-integrity-guard.py",
		"\n\n_planted_open = open\n",
		"make-integrity-guard.py: <module> uses open"},
	{"a second splitter inside a named reader", "ci-required-guard.py",
		"\n\ndef load_workflows_helper(text):\n    return text.split(\"\\n\")\n",
		"ci-required-guard.py: load_workflows_helper uses split-newline"},
}

// Each planted second reader turns the SOURCE check red by name; the
// unplanted tree is green (TestEveryMakefileReaderConsumesTheOneLineReader).
func TestTheOneReaderSourceCheckRefusesAPlantedReader(t *testing.T) {
	src := filepath.Join(repoRoot(t), "scripts")
	for _, p := range plantedReaders {
		p := p
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, f := range []string{"makefile_pin.py", "make-integrity-guard.py", "ci-required-guard.py"} {
				b, err := os.ReadFile(filepath.Join(src, f))
				if err != nil {
					t.Fatal(err)
				}
				if f == p.file {
					b = append(b, []byte(p.code)...)
				}
				if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := runReaderProbe(t, dir)
			joined := strings.ToLower(strings.Join(got.Source, "\n"))
			if !strings.Contains(joined, strings.ToLower(p.want)) {
				t.Fatalf("planted %q in %s; the SOURCE check reported %q, want %q", p.code, p.file, got.Source, p.want)
			}
			t.Logf("%s | refused: %s", p.name, got.Source[0])
		})
	}
}
