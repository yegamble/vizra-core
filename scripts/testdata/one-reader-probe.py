#!/usr/bin/env python3
"""one-reader-probe — every reading of pinned makefile text consumes makefile_pin.makefile_lines.

Test harness only (driven by scripts/grammar_test.go; queue 2p, core B5d). The idea is ported from
vizra-search's oneReaderProbe (scripts/scripts_test.go at 4810048): two readers that split a makefile into
lines differently disagree on which recipe a TAB line belongs to, so there is ONE reader, and this proves it
three ways:

  POISON    makefile_lines is wrapped to rewrite the text it is given. Every named reader's verdict on an
            inert fixture must change exactly as the poison says; a reader with another path to the bytes
            would not see the poison.
  SOURCE    (function level) no named reader splits text, reads a file as text, decodes bytes or uses a
            multi-line regex itself; (file level, by AST) in makefile_pin.py, make-integrity-guard.py and
            ci-required-guard.py every such spelling is one of NAMED_READS, each of a NON-makefile input
            (the pin file, workflows, the manifest, make's or the shell's output) or the reader itself.
  IDENTITY  within the program every reader of one text receives the SAME sequence object.

Usage: one-reader-probe.py SCRIPTS_DIR TMP_DIR. Prints one JSON object. The fixture Makefile is written to
TMP_DIR and never run: subprocess is replaced by a stub that raises.
"""

from __future__ import annotations

import ast
import contextlib
import hashlib
import importlib.util
import inspect
import io
import json
import re
import subprocess
import sys
from pathlib import Path

scripts = Path(sys.argv[1])
root = Path(sys.argv[2])
sys.dont_write_bytecode = True


def _no_process(*_a, **_kw):
    raise RuntimeError("one-reader-probe: a reader tried to start a process")


def load(name, f):
    spec = importlib.util.spec_from_file_location(name, scripts / f)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


mig = load("make_integrity_guard", "make-integrity-guard.py")
crg = load("ci_required_guard", "ci-required-guard.py")
mp = mig.mp
assert crg.mp is mp, "the anchor and ci-required-guard must share one makefile_pin module"
subprocess.Popen = _no_process  # type: ignore[assignment]
subprocess.run = _no_process  # type: ignore[assignment]

MAKEFILE = (
    "# inert fixture: nothing here is run\n"
    "SHELL := /usr/bin/env bash\n"
    ".SHELLFLAGS := -eu -o pipefail -c\n"
    "PKGS := ./...\n"
    ".PHONY: ci test-race\n"
    "ci: test-race\n"
    "test-race:\n"
    "\tgo test -race -run 'TestInert' $(PKGS)\n"
)
mk = root / "Makefile"
mk.write_bytes(MAKEFILE.encode())
(root / ".github").mkdir(exist_ok=True)
pin = root / ".github" / "pinned-makefiles.yml"
pin.write_text("makefiles:\n  Makefile: " + hashlib.sha256(MAKEFILE.encode()).hexdigest() + "\n")

state = {"poison": None, "calls": []}
real_lines = mp.makefile_lines


def spy(text):
    if state["poison"] is not None:
        old, new = state["poison"]
        if text.count(old) != 1:
            raise AssertionError("the poison did not apply: %r" % old)
        text = text.replace(old, new)
    res = real_lines(text)
    f, stack = sys._getframe(1), []
    while f is not None:
        stack.append(f.f_code.co_name)
        f = f.f_back
    # The sequence object itself is kept (not only its id): a freed object's id can be reused.
    state["calls"].append((hashlib.sha256(text.encode()).hexdigest(), res, stack))
    return res


mp.makefile_lines = spy


def guarded(module, call):
    g = module.Guard()
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
        call(g)
    return g.failed, buf.getvalue()


def env_names():
    crg.MAKEFILE_ENV_NAMES.clear()
    crg.MAKEFILE_ENV_NAMES.add("GOFLAGS")
    crg.load_makefile_env_names(mk)
    return sorted(crg.MAKEFILE_ENV_NAMES)


ADD = "PKGS := ./...\n"
RECIPE = "\tgo test -race -run 'TestInert' $(PKGS)\n"
TARGETS = ["ci", "test-race"]
probes = [
    ("makefile_pin.grammar_problems", lambda: mp.grammar_problems("Makefile", MAKEFILE),
     (ADD, ADD + "ifdef INERT\nendif\n"), lambda r: r == [], lambda r: any("conditional directive" in p for p in r)),
    ("makefile_pin.static_read_set (its include reading)", lambda: mp.static_read_set(root, {"Makefile": MAKEFILE})[0],
     (ADD, ADD + "include inert.mk\n"), lambda r: r == ["Makefile"], lambda r: "inert.mk" in r),
    ("makefile_pin.verify_pin (its parse-time sites)", lambda: mp.verify_pin(root).sites,
     (ADD, ADD + "INERT := $(shell true)\n"), lambda r: r == [], lambda r: any("INERT" in s for s in r)),
    ("makefile_pin.verify_pin (its grammar)", lambda: [p.message for p in mp.verify_pin(root).grammar],
     (ADD, ADD + "vpath %.inert .\n"), lambda r: r == [], lambda r: any("`vpath` directive" in p for p in r)),
    ("anchor prerequisite_closure", lambda: mig.prerequisite_closure(root, ["Makefile"], ["ci"]),
     ("ci: test-race\n", "ci: test-race inert-lane\n"), lambda r: "inert-lane" not in r, lambda r: "inert-lane" in r),
    ("anchor check_text (recipes, through makefile_pin.recipe_lines)",
     lambda: guarded(mig, lambda g: mig.check_text(g, root, ["Makefile"], TARGETS, ["ci"])),
     (RECIPE, "\t-" + RECIPE[1:]), lambda r: not r[0], lambda r: r[0] and "prefixed `-`" in r[1]),
    ("anchor check_text (assignments)",
     lambda: guarded(mig, lambda g: mig.check_text(g, root, ["Makefile"], TARGETS, ["ci"])),
     ("SHELL := /usr/bin/env bash\n", "SHELL := /bin/sh\n"), lambda r: not r[0],
     lambda r: r[0] and "other than the approved value" in r[1]),
    ("anchor check_text (definitions)",
     lambda: guarded(mig, lambda g: mig.check_text(g, root, ["Makefile"], TARGETS, TARGETS)),
     ("test-race:\n", "inert-renamed:\n"), lambda r: not r[0], lambda r: r[0] and "not defined in any makefile" in r[1]),
    ("anchor check_text (a named token)",
     lambda: guarded(mig, lambda g: mig.check_text(g, root, ["Makefile"], TARGETS, ["ci"])),
     (ADD, ADD + ".POSIX:\n"), lambda r: not r[0], lambda r: r[0] and "names `.POSIX`" in r[1]),
    ("anchor check_environment_overrides",
     lambda: guarded(mig, lambda g: mig.check_environment_overrides(g, root, ["Makefile"], False)),
     (ADD, ADD + "INERT_TAKEN ?= 1\n"), lambda r: "INERT_TAKEN" not in r[1], lambda r: "INERT_TAKEN" in r[1]),
    ("ci-required-guard load_makefile_env_names", env_names,
     (ADD, ADD + "INERT_TAKEN ?= 1\n"), lambda r: "INERT_TAKEN" not in r, lambda r: "INERT_TAKEN" in r),
    ("ci-required-guard check_makefile_selection (PKGS)",
     lambda: guarded(crg, lambda g: crg.check_makefile_selection(g, mk)),
     (ADD, "PKGS := ./internal/...\n"), lambda r: not r[0], lambda r: r[0] and "not './...'" in r[1]),
    ("ci-required-guard check_makefile_selection (the test-race recipe)",
     lambda: guarded(crg, lambda g: crg.check_makefile_selection(g, mk)),
     (RECIPE, "\tgo test -race -run 'TestInert' ./cmd/...\n"), lambda r: not r[0],
     lambda r: r[0] and "does not use $(PKGS)" in r[1]),
    ("ci-required-guard check_makefile_selection (-run)",
     lambda: guarded(crg, lambda g: crg.check_makefile_selection(g, mk)),
     ("-run 'TestInert'", "-run ''"), lambda r: not r[0], lambda r: r[0] and "are empty" in r[1]),
    ("ci-required-guard check_makefile_pin (check 11's grammar)",
     lambda: guarded(crg, lambda g: crg.check_makefile_pin(g, pin)),
     (ADD, ADD + "ifdef INERT\nendif\n"), lambda r: not r[0], lambda r: r[0] and "conditional directive" in r[1]),
]

result = {"probes": [], "source": [], "identity": [], "readers_seen": [], "named_reads": 0}
for name, fn, poison, clean_ok, poisoned_ok in probes:
    state["poison"] = None
    clean = fn()
    state["poison"] = poison
    try:
        poisoned, err = fn(), ""
    except Exception as e:  # noqa: BLE001 — reported, not swallowed
        poisoned, err = None, repr(e)
    state["poison"] = None
    result["probes"].append({"name": name, "clean_ok": bool(clean_ok(clean)),
                             "poisoned_ok": poisoned is not None and bool(poisoned_ok(poisoned)),
                             "detail": [repr(clean)[:300], repr(poisoned)[:300], err]})

# IDENTITY: every reader, clean, gets the SAME sequence object for the same text.
state["calls"] = []
for name, fn, poison, clean_ok, poisoned_ok in probes:
    fn()
by = {}
for digest, seq, stack in state["calls"]:
    by.setdefault(digest, set()).add(id(seq))
    result["readers_seen"].extend(stack)
for digest, ids in sorted(by.items()):
    result["identity"].append({"text": digest[:12], "distinct_sequences": len(ids)})
result["readers_seen"] = sorted(set(result["readers_seen"]))

# SOURCE, function level.
forbidden = [r'split\(\s*["\']\\n["\']\s*\)', r"\.splitlines\(", r"\.read_text\(", r"(?<![\w.])open\(", r"\.decode\(",
             r"\bre\.M\b", r"re\.MULTILINE", r"\.readlines\("]
readers = [(mp, n) for n in ("grammar_problems", "static_read_set", "verify_pin", "recipe_lines",
                             "read_makefile_lines", "keeps_rule_open")]
readers += [(mig, "prerequisite_closure"), (mig, "check_text"), (mig, "check_environment_overrides"),
            (mig, "check_makefile_pin"), (crg, "load_makefile_env_names"), (crg, "check_makefile_selection"),
            (crg, "check_makefile_pin")]
for mod, fname in readers:
    fn = getattr(mod, fname, None)
    if fn is None:
        result["source"].append("%s.%s does not exist" % (mod.__name__, fname))
        continue
    src = inspect.getsource(fn)
    for pat in forbidden:
        if re.search(pat, src):
            result["source"].append("%s.%s reads makefile text itself (%s)" % (mod.__name__, fname, pat))

# SOURCE, file level (by AST, so comments and docstrings do not count, and a reference without a call, an
# alias import or an inline (?m) does): every such spelling must be a NAMED read of a non-makefile input.
TEXT_ATTRS = {"splitlines", "readlines", "read_text", "read_bytes", "decode", "open"}


def text_reads(src):
    out = set()

    def visit(node, owner):
        for child in ast.iter_child_nodes(node):
            o = owner
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                o = child.name if owner == "<module>" else owner + "." + child.name
            k = None
            if isinstance(child, ast.Attribute) and child.attr in TEXT_ATTRS:
                k = child.attr
            elif isinstance(child, ast.Name) and child.id == "open":
                k = "open"
            elif isinstance(child, ast.alias) and (child.name in TEXT_ATTRS or child.name in ("M", "MULTILINE")):
                k = "import " + child.name
            elif isinstance(child, ast.Attribute) and (child.attr == "MULTILINE" or (
                    child.attr == "M" and isinstance(child.value, ast.Name) and child.value.id == "re")):
                k = "re.M"
            elif isinstance(child, ast.Constant) and isinstance(child.value, str) and re.search(
                    r"\(\?[aiLmsux]*m[aiLmsux]*[):]", child.value):
                k = "re.M"
            elif isinstance(child, ast.Call) and isinstance(child.func, ast.Attribute) and child.func.attr in (
                    "split", "rsplit") and child.args and isinstance(child.args[0], ast.Constant) and \
                    child.args[0].value in ("\n", b"\n", "\r\n"):
                k = "split-newline"
            if k:
                out.add((o, k))
            visit(child, o)

    visit(ast.parse(src), "<module>")
    return out


NAMED_READS = {
    "makefile_pin.py": {
        ("makefile_lines", "split-newline"): "THE line reader",
        ("decode_makefile", "decode"): "THE decoding",
        ("read_makefile_text", "read_bytes"): "the bytes THE decoding decodes",
        ("verify_pin", "read_bytes"): "the bytes it digests, then decodes through decode_makefile",
        ("load_makefile_pin", "read_bytes"): "the pin file .github/pinned-makefiles.yml",
        ("load_makefile_pin", "decode"): "the pin file",
        ("load_makefile_pin", "split-newline"): "the pin file",
    },
    "make-integrity-guard.py": {
        ("parse_database", "splitlines"): "make -pn's stdout",
        ("resolve_database", "splitlines"): "make -pn's stderr/stdout, quoted in the refusal",
        ("check_warnings", "splitlines"): "make --dry-run's stderr and stdout",
        ("check_no_pinned_makefile_would_be_remade", "splitlines"): "make -q's output, quoted in the refusal",
        ("check_make_resolves_to_a_real_program", "splitlines"): "the shell's stdout (type -t make; command -v make)",
        ("recheck_pinned_bytes", "read_bytes"): "the bytes it re-digests; never decoded",
        ("main", "splitlines"): "this module's own docstring",
    },
    "ci-required-guard.py": {
        ("<module>", "re.M"): "MAKE_INVOCATION, applied to workflow step text",
        ("_commands", "splitlines"): "a workflow step's `run:` text",
        ("load_workflows", "read_text"): "the workflow YAML files",
        ("load_pins", "read_text"): ".github/pinned-steps.yml",
        ("main", "read_text"): ".github/required-checks.txt",
        ("main", "splitlines"): ".github/required-checks.txt",
    },
}
for f, named in NAMED_READS.items():
    got = text_reads((scripts / f).read_text())
    for owner, kind in sorted(got - set(named)):
        result["source"].append("%s: %s uses %s, which is not a named read of a non-makefile input; makefile text "
                                "is read only through makefile_pin.makefile_lines" % (f, owner, kind))
    for owner, kind in sorted(set(named) - got):
        result["source"].append("%s: the named read (%s, %s) no longer exists; remove it from NAMED_READS so the "
                                "allowance cannot be reused" % (f, owner, kind))
    result["named_reads"] += len(named)
print(json.dumps(result))
