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


# ------------------------------------------------------------------------------------------------------
# Queue 2p (core B5d): the allowlist grammar refuses BEFORE make every committed fixture that used to reach
# these post-make branches (a computed variable name, a function in a recipe, a pattern-specific
# assignment, an unparseable line). The branches stay as defence in depth, so each keeps a red case HERE,
# driven in-process: the database strings through parse_database, and make itself replaced by a stub of
# the guard's own run_make (no process is started — subprocess raises).


class _Proc:
    def __init__(self, returncode: int, stdout: str = "", stderr: str = ""):
        self.returncode, self.stdout, self.stderr = returncode, stdout, stderr


def _guarded(call):
    g = mig.Guard()
    out = io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
        call(g)
    return g.failed, out.getvalue()


def resolved(db_text: str):
    return lambda: _guarded(lambda g: mig.check_resolved(g, "ci", mig.parse_database(db_text + END)))


def expanded(line: str, target_specific=()):
    def run():
        mig.GATE_RECIPE_LINES[:] = [("Makefile", 1, "ci", line)]
        mig.TARGET_SPECIFIC_NAMES.clear()
        mig.TARGET_SPECIFIC_NAMES.update(target_specific)
        variables = mig.parse_database(GOOD_DB + END)
        return _guarded(lambda g: mig.check_expanded_prefixes(g, variables))
    return run


def with_make(returncode: int, call, calls=None):
    def run():
        real = mig.run_make

        def stub(root, args):
            if calls is not None:
                calls.append(list(args))
            return _Proc(returncode, "", "make: *** stub")
        mig.run_make = stub
        try:
            return _guarded(call)
        finally:
            mig.run_make = real
    return run


PROBE_CALLS: list = []


def remake_probe_is_one_invocation():
    PROBE_CALLS.clear()
    failed, out = with_make(0, lambda g: mig.check_no_pinned_makefile_would_be_remade(
        g, Path("."), ["Makefile", "a.mk", "b.mk"]), PROBE_CALLS)()
    one = PROBE_CALLS == [["-q", "Makefile", "a.mk", "b.mk"]]
    return (failed or not one), out + f"\nmake calls: {PROBE_CALLS}"


POST_MAKE_ROWS = [
    # (name, fn, expect_fail, must_contain, must_not_contain)
    ("resolver: SHELL resolved to a no-op", resolved(GOOD_DB.replace("SHELL := /usr/bin/env bash",
                                                                     "SHELL := /usr/bin/true")), True,
     "make resolves SHELL to '/usr/bin/true'", None),
    ("resolver: MAKEFLAGS carries i", resolved(GOOD_DB.replace(SHELL_VARS, SHELL_VARS + "MAKEFLAGS = pni\n")), True,
     "make resolves MAKEFLAGS to 'pni'", None),
    ("resolver: .RECIPEPREFIX set", resolved(GOOD_DB.replace(SHELL_VARS, SHELL_VARS + ".RECIPEPREFIX := >\n")), True,
     "make resolves .RECIPEPREFIX to '>'", None),
    ("resolver: .EXTRA_PREREQS set", resolved(GOOD_DB.replace(SHELL_VARS, SHELL_VARS + ".EXTRA_PREREQS := Makefile\n")),
     True, "make resolves .EXTRA_PREREQS to 'Makefile'", None),
    ("expanded prefix: a leading make function", expanded("$(if yes,-)./run-the-real-tests.sh"), True,
     "starts with a make function", None),
    ("expanded prefix: a target-specific leading variable", expanded("$(IGN)./run-the-real-tests.sh", ("IGN",)), True,
     "which a target- or pattern-specific assignment sets", None),
    ("control: expanded prefix of a plain command", expanded("./run-the-real-tests.sh"), False,
     "no gate recipe line expands", None),
    ("resolver: make -pn fails", with_make(2, lambda g: mig.resolve_database(g, Path("."), "ci")), True,
     "could not be established", None),
    ("remake probe: make -q exit 2", with_make(2, lambda g: mig.check_no_pinned_makefile_would_be_remade(
        g, Path("."), ["Makefile"])), True, "failed (exit 2): make reported an ERROR", "would REMAKE"),
    ("remake probe: make -q exit 1", with_make(1, lambda g: mig.check_no_pinned_makefile_would_be_remade(
        g, Path("."), ["Makefile"])), True, "make would REMAKE one of Makefile", None),
    ("remake probe: ONE make -q naming every pinned makefile", remake_probe_is_one_invocation, False,
     "make calls: [['-q', 'Makefile', 'a.mk', 'b.mk']]", None),
]


def main() -> int:
    bad = 0
    rows = [(name, (lambda w=which, d=db, c=closure, t=text: run_check(w, d, c, t)), expect_fail, want, not_want)
            for name, which, db, closure, text, expect_fail, want, not_want in ROWS] + POST_MAKE_ROWS
    for name, fn, expect_fail, want, not_want in rows:
        failed, out = fn()
        ok = failed == expect_fail and want in out and (not not_want or not_want not in out)
        print(f"ROW {'ok  ' if ok else 'BAD '} {name}: refused={failed} (expected {expect_fail}); "
              f"says {want!r}: {want in out}")
        if not ok:
            bad += 1
            for line in out.strip().splitlines()[:6]:
                print(f"        | {line}")
    if bad:
        print(f"PROBE: FAILED — {bad} of {len(rows)} rows did not behave as expected")
        return 1
    print(f"PROBE: all {len(rows)} rows as expected")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
