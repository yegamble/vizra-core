#!/usr/bin/env python3
"""rows.py — TestEveryOutOfGrammarLineIsRefusedBeforeMake without Go (for the GNU Make 4.3 container).

Reads the rows out of scripts/grammar_test.go (outOfGrammarRows, the directive-keyword loop, and
grammarControls) — the SAME literals the Go test uses, so the two cannot drift — and for each one:
appends it to a copy of the real Makefile, re-pins, runs the anchor in --workflow mode under
scripts/testdata/spawn-recorder.py, and runs ci-required-guard check 11. A row HOLDS when the anchor
exits 1 naming the grammar and the row's reason with 0 make processes recorded, and check 11 exits 1
with the same reason; a control HOLDS when the anchor exits 0 having started make, and check 11 exits 0.

Run from the root of a checkout: python3 docs/evidence/hardening-b5/b5d/rows.py. Exit 0 = every row held.
The Go string literals used there (\\n, \\t, \\x00, \\x0c, \\u00a0, \\u202e, \\\\) mean the same in Python.
"""
import ast
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

src = open("scripts/grammar_test.go", encoding="utf-8").read()


def block(start, end):
    i = src.index(start)
    return src[i:src.index(end, i)]


LIT = r'"(?:[^"\\]|\\.)*"'
ROW = re.compile(r"\{(" + LIT + r"), (" + LIT + r"), (" + LIT + r"|og)\}")
keywords = ast.literal_eval("[" + block("var directiveKeywords = []string{", "}").split("{", 1)[1] + "]")
og = "outside the makefile grammar"
rows = []
for m in ROW.finditer(block("func outOfGrammarRows()", "\n}\n")):
    want = og if m.group(3) == "og" else ast.literal_eval(m.group(3))
    rows.append((ast.literal_eval(m.group(1)), ast.literal_eval(m.group(2)), want))
for kw in keywords:
    rows.append((kw + " as a variable name", "\n" + kw + " := 1\n",
                 "a directive keyword (`" + kw + "`) as a variable name"))
for m in ROW.finditer(block("var grammarControls = []grammarRow{", "\n}\n")):
    rows.append((ast.literal_eval(m.group(1)), ast.literal_eval(m.group(2)), ""))
if len(rows) < 80:
    sys.exit("only %d rows parsed out of scripts/grammar_test.go; the reader no longer sees the table" % len(rows))

env = {k: v for k, v in os.environ.items()
       if k not in ("MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS", "MAKELEVEL", "MAKE_RESTARTS", "MAKEOVERRIDES",
                    "MAKECMDGOALS", "MAKEFILES", "BASH_ENV", "ENV", "GO", "SQLC", "GOFLAGS", "RELEASE",
                    "COMMIT", "BUILT_AT")}
real = open("Makefile", "rb").read()
held = broken = 0
for name, add, want in rows:
    d = tempfile.mkdtemp()
    try:
        os.makedirs(os.path.join(d, ".github"))
        data = real + add.encode("utf-8")
        open(os.path.join(d, "Makefile"), "wb").write(data)
        pin = os.path.join(d, ".github", "pinned-makefiles.yml")
        open(pin, "w").write("makefiles:\n  Makefile: %s\n" % hashlib.sha256(data).hexdigest())
        rec = os.path.join(d, "record.json")
        p = subprocess.run(["python3", "scripts/testdata/spawn-recorder.py", rec, "--root", d, "--targets", "ci",
                            "--workflow"], capture_output=True, text=True, env=env)
        calls = json.load(open(rec))["calls"]
        makes = sum(1 for c in calls if c["argv"] and os.path.basename(c["argv"][0]) in ("make", "gmake"))
        g = subprocess.run(["python3", "scripts/ci-required-guard.py", "--makefile-pins", pin],
                           capture_output=True, text=True, env=env)
        out, gout = (p.stdout + p.stderr).lower(), (g.stdout + g.stderr).lower()
        if want:
            ok = (p.returncode == 1 and og in out and want in out and makes == 0
                  and "make was not invoked (0 make process(es) started)" in out
                  and g.returncode == 1 and og in gout and want in gout)
        else:
            ok = p.returncode == 0 and makes > 0 and g.returncode == 0
        line = next((l.strip() for l in (p.stdout + p.stderr).splitlines() if og in l.lower()), "")
        print("%s %-70s anchor exit=%d make-processes=%d check11 exit=%d | %s"
              % ("HELD  " if ok else "BROKEN", name[:70], p.returncode, makes, g.returncode, line[:110]))
        held += ok
        broken += not ok
    finally:
        shutil.rmtree(d)
print("rows: %d held, %d broken, of %d (%d refusals, %d controls)"
      % (held, broken, len(rows), sum(1 for r in rows if r[2]), sum(1 for r in rows if not r[2])))
sys.exit(1 if broken else 0)
