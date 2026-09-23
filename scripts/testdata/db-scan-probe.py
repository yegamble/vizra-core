#!/usr/bin/env python3
"""db-scan-probe — the post-make database checks of make-integrity-guard.py, fed STRINGS in-process.

Test harness only (driven by scripts/makefiledigest_test.go and by
docs/evidence/hardening-b5/b5b/demo.sh on hosts with no Go toolchain). No
Makefile is written, no make is run: `subprocess` is replaced by a stub that
raises, so a row that tried to start a process would fail loudly.

Each row hands parse_database() a `-pn`-shaped database text, sets the recipe
lines the TEXT reading would have scanned, runs one check with a fresh Guard,
and compares: did it refuse, and does its output carry the expected words.
Rows exist because these checks must fail CLOSED (#11 fix round 2, R1-1): an
empty database, an empty entry, a duplicated entry, an unreadable name, a
recipe the text does not show, and a closure make widens were each read as
"nothing to refuse" before. Controls must PASS, or the refusals prove nothing.

Exit 0 when every row behaves as expected; 1 otherwise. The last line is
`PROBE: all N rows as expected` or `PROBE: FAILED …`.
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import subprocess
import sys
from pathlib import Path

GUARD = Path(__file__).resolve().parent.parent / "make-integrity-guard.py"


def _no_process(*_a, **_kw):
    raise RuntimeError("db-scan-probe: a check tried to start a process")


subprocess.Popen = _no_process  # type: ignore[assignment]
subprocess.run = _no_process  # type: ignore[assignment]

spec = importlib.util.spec_from_file_location("make_integrity_guard", GUARD)
mig = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mig)

SHELL_VARS = "SHELL := /usr/bin/env bash\n.SHELLFLAGS := -eu -o pipefail -c\n\n"

GOOD_DB = "# Make data base, printed on (probe)\n\n" + SHELL_VARS + (
    "ci: lane\n"
    "#  Phony target (prerequisite of .PHONY).\n"
    "\t@echo gate\n"
    "\n"
    "lane:\n"
    "#  Phony target (prerequisite of .PHONY).\n"
    "\t./run-the-real-tests.sh\n"
    "\n"
    ".PHONY: ci lane\n"
    "\n"
)
# run_check appends make's end marker to every non-empty database text.
END = "\n# Finished Make data base on (probe)\n"
TEXT = {"ci": ["@echo gate"], "lane": ["./run-the-real-tests.sh"]}


def run_check(which: str, db_text: str, closure, text, seeds=("ci",), target="ci"):
    mig.GATE_RECIPE_LINES[:] = [("Makefile", 1, t, b) for t, lines in text.items() for b in lines]
    g = mig.Guard()
    out = io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
        variables = mig.parse_database(db_text + END if db_text else db_text)
        if which == "recipes":
            mig.check_db_recipes(g, list(closure))
        elif which == "closure":
            mig.check_db_closure(g, list(seeds), list(closure))
        elif which == "resolved":
            mig.check_resolved(g, target, variables)
    return g.failed, out.getvalue()


ROWS = [
    # (name, check, db, closure, text, expect_fail, must_contain, must_not_contain)
    ("control: consistent database", "recipes", GOOD_DB, ["ci", "lane"], TEXT, False,
     "has ONE readable entry", None),
    ("empty database", "recipes", "", ["ci", "lane"], TEXT, True,
     "has NO entry for gate closure target `ci`", None),
    ("empty entry, recipe expected", "recipes",
     GOOD_DB.replace("\t@echo gate\n", ""), ["ci", "lane"], TEXT, True,
     "DIFFERENT recipe for gate closure target `ci`", None),
    ("duplicate entry", "recipes",
     GOOD_DB + "\nci:\n\t-./run-the-real-tests.sh\n", ["ci", "lane"], TEXT, True,
     "MORE THAN ONE entry for gate closure target `ci`", None),
    ("space-containing name", "recipes",
     GOOD_DB + "\nci other: x\n\t@true\n", ["ci", "lane"], TEXT, True,
     "cannot read as ONE name", None),
    ("a recipe line the text does not show", "recipes",
     GOOD_DB.replace("\t@echo gate\n", "\t@echo gate\n\t./another-command\n"), ["ci", "lane"], TEXT, True,
     "DIFFERENT recipe for gate closure target `ci`", None),
    # Under -n make prints the commands it would run OUTSIDE the database
    # (3.81: before it); a command line such as this is not a target.
    ("control: command output outside the database", "recipes",
     'echo "make ci: all lanes passed"\n' + GOOD_DB, ["ci", "lane"], TEXT, False,
     "has ONE readable entry", "cannot read"),
    ("no database section at all", "recipes",
     GOOD_DB.replace("# Make data base, printed on (probe)\n", ""), ["ci", "lane"], TEXT, True,
     "no `# Make data base` section", None),
    ("control: closures equal", "closure", GOOD_DB, ["ci", "lane"], TEXT, False,
     "equals the text closure (2 target(s))", None),
    ("make widens the closure", "closure",
     GOOD_DB.replace("ci: lane\n", "ci: lane extra\n") + "\nextra:\n\t@true\n", ["ci", "lane"], TEXT, True,
     "make reaches, the text does not: extra", None),
    ("make narrows the closure", "closure",
     GOOD_DB.replace("ci: lane\n", "ci:\n"), ["ci", "lane"], TEXT, True,
     "the text reaches, make does not: lane", None),
    # The resolver backstops whose committed fixtures became PRE-make refusals
    # in #11 fix round 2 (a rule whose TARGET make computes is refused before
    # make): each branch keeps a red case here.
    ("control: resolver sees nothing special", "resolved", GOOD_DB, [], {}, False,
     "no `.IGNORE`, no `.DEFAULT` recipe, no `.EXTRA_PREREQS`", "is in effect"),
    ("resolver: .IGNORE in effect", "resolved", GOOD_DB + "\n.IGNORE:\n", [], {}, True,
     "`.IGNORE:` is in effect", None),
    ("resolver: .DEFAULT has a recipe", "resolved",
     GOOD_DB + "\n.DEFAULT:\n#  recipe to execute (from 'Makefile', line 9):\n\t@true\n", [], {}, True,
     "`.DEFAULT` has a recipe", None),
    ("resolver: .SECONDEXPANSION in effect", "resolved", GOOD_DB + "\n.SECONDEXPANSION:\n", [], {}, True,
     "`.SECONDEXPANSION:` is in effect", None),
]


def main() -> int:
    bad = 0
    for name, which, db, closure, text, expect_fail, want, not_want in ROWS:
        failed, out = run_check(which, db, closure, text)
        ok = failed == expect_fail and want in out and (not not_want or not_want not in out)
        print(f"ROW {'ok  ' if ok else 'BAD '} {name}: refused={failed} (expected {expect_fail}); "
              f"says {want!r}: {want in out}")
        if not ok:
            bad += 1
            for line in out.strip().splitlines()[:6]:
                print(f"        | {line}")
    if bad:
        print(f"PROBE: FAILED — {bad} of {len(ROWS)} rows did not behave as expected")
        return 1
    print(f"PROBE: all {len(ROWS)} rows as expected")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
