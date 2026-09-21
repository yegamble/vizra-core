#!/usr/bin/env python3
"""make-integrity-guard — refuse a Makefile that has been turned into a no-op.

WHY THIS EXISTS
---------------

Every required lane in this repository runs through `make`. A verifier measured,
at a real commit, that ONE line in the Makefile makes every one of them exit 0
without running anything:

    SHELL := /usr/bin/true     make ci = 0   make test = 0   make test-noskip = 0
    MAKEFLAGS += -i            make ci = 0   make test = 0   make test-noskip = 0

(docs/evidence/warroom/2026-09-20-vizra-search-pr2-revendor-VERIFY.md, FINDING 8.)

A merge rule that reads "an independent verifier passed it AND `ci-required` is
green on the verified SHA" is worth exactly as much as the green is. With either
of those lines present, the green means nothing at all.

No check written INSIDE a Makefile can prevent this: a `SHELL` override or
`MAKEFLAGS += -i` disarms the recipe that would run the check along with
everything else. So this program runs OUTSIDE make — as its own workflow step,
before any `make` invocation in every required lane that invokes make.

WHAT IT ACTUALLY CHECKS, AND HOW EACH IS SEEN
---------------------------------------------

Three readings, because no single one sees everything. Each mutation below was
MEASURED on GNU Make 3.81 (macOS system make) and GNU Make 4.3 (ubuntu-24.04,
the runner), and the two disagree in ways that matter:

  reading            sees                                       blind to
  -----------------  -----------------------------------------  ------------------------
  RESOLVER           SHELL / .SHELLFLAGS / MAKEFLAGS as make     a `-` recipe prefix:
  (`make -pn TGT`)   itself resolved them — through variables,   `make --dry-run` prints
                     through an `include`, through a duplicate   the command WITHOUT the
                     definition. MAKEFILE_LIST names every       `-`, so no scan of the
                     file make actually read.                    resolved recipe can see it
  TEXT               a `-` / `@-` prefix; a `|| true` suffix;    a value computed at
  (those same        any SHELL/.SHELLFLAGS/MAKEFLAGS/            runtime, e.g. through
  files, read)       GNUMAKEFLAGS assignment; `.ONESHELL:`;      `$(eval …)`
                     a gate target defined twice
  WARNINGS           a duplicate target — make says so on        nothing else; other
  (`make --dry-run`  stderr. GNU 3.81: "overriding COMMANDS      warnings are printed but
  stderr)            for target"; GNU 4.x: "overriding RECIPE    do NOT fail this guard
                     for target". Both are matched.

Plus the ENVIRONMENT, because `MAKEFLAGS=-i make ci` never appears in any file.

WHAT THIS GUARANTEES — stated at exactly its real strength
-----------------------------------------------------------

While this program runs as an unconditional workflow step before `make` in a
required lane, and its exit status is not discarded:

  * a Makefile (or anything it `include`s) that overrides `SHELL`,
    `.SHELLFLAGS`, `MAKEFLAGS` or `GNUMAKEFLAGS`, or sets `.ONESHELL`, fails the
    lane BY NAME before make is ever invoked;
  * a gate target whose recipe carries a `-` / `@-` prefix or a `|| true`-family
    suffix fails the lane by name;
  * a gate target defined twice — where make silently runs the LAST definition
    while a reader, and any text-based check, sees the first — fails the lane by
    name.

WHAT IT DOES NOT GUARANTEE — stated, not implied
-------------------------------------------------

This list is meant to be EXHAUSTIVE. Something that belongs on it and is not
here is a defect in this docstring, not a detail.

  * **This guard never reads the workflow's own `make` invocation.** It checks
    the Makefile, everything it includes, and its OWN environment. It cannot
    see the argv or the step-level `env:` of a DIFFERENT workflow step, and
    `ci-required-guard.py` checks a make step's presence, position, `if:` and
    `continue-on-error` — never the TEXT of its `run:`. So ONE WORD on a
    workflow line still no-ops every make-driven lane while BOTH guards exit 0.
    Four spellings, each measured green at f56dc03 on an unmodified Makefile
    with a real failing test planted:

        run: make -i ci
        run: make SHELL=/usr/bin/true ci
        run: make MAKEFLAGS=-i ci
        a step-level `env: MAKEFLAGS: -i` on an ordinary `run: make ci` step

    Blast radius: everything make-driven goes silent — fmt-check, vet,
    lint-imports, migrate-lint, config-template-check, openapi-verify,
    sqlc-verify, ci-guard, fixtures-verify, tidy-check, build, and BOTH
    integration lanes including both cache-matrix legs. Only the unit suite
    survives, via the direct `go test ./...` step in build-test.

    Closing it — refusing flag and `VAR=value` overrides on a floor lane's make
    step `run:`, and a MAKEFLAGS/GNUMAKEFLAGS/MFLAGS step-level `env:` — is
    QUEUED FOR CORE HARDENING SWEEP B and is deliberately not implemented here.

  * **The direct `go test` lane covers the UNIT suite only.** It carries no
    `-tags=integration`, and every integration invocation in this repository
    goes through make. Under the evasion above, a failing INTEGRATION test is
    silent rather than red. Queued for sweep B.

  * **The direct `go test` lane does not fail when ZERO tests run.**
    `go test -race -count=1 ./...` with every `*_test.go` moved aside exits 0,
    reporting `[no test files]` for each package (measured at f56dc03). It is a
    control against make being neutered, not against the suite being EMPTIED.
    Queued for sweep B.

  * **`append-only` is a required floor lane with no provenance step.** It
    checks out with `fetch-depth: 0`, computes a merge base and echoes that SHA
    without saying which tree it is standing in. `provenance.sh` runs in every
    required workflow FILE, which is not the same as every required JOB.
    Queued.

  * **This file is editable in the same pull request.** Deleting or weakening it
    is a second diff in a reviewed file; `scripts/ci-required-guard.py` asserts
    the anchor step exists, is unconditional and is not `continue-on-error` in
    every required workflow that invokes make, and `scripts/scripts_test.go`
    runs this program against the real tree from the ordinary suite — but both
    of those are also checked out from the pull request under test. There is no
    mechanical control in this repository that a sufficiently determined PR
    cannot also edit. What there is, is that every such edit is VISIBLE in the
    diff of a file whose only purpose is to be a gate.
  * **CODEOWNERS is not enforced.** These paths are CODEOWNERS-assigned, but the
    ruleset that would make that review mandatory returns 403 on the owner's
    plan, so CODEOWNERS is advisory today. It is not a backstop; it is a label.
  * It says nothing about whether the lanes TEST the right things — only that
    make will really run them. The direct `go test ./...` step in `build-test`
    is the separate control for "a neutered make cannot make the UNIT suite
    silent", with the two limits noted above.
  * It does not sandbox make. A recipe that runs a wrapper script which
    itself lies is out of scope here.

Usage:
    make-integrity-guard.py [--root DIR] [--targets a,b,c]
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

# ---------------------------------------------------------------------------
# THE GATE TARGETS.
#
# Every make target a REQUIRED CI lane invokes. `ci` pulls in its own
# prerequisite closure, which is computed rather than listed, so a lane added to
# `ci` is covered without editing this list.
# ---------------------------------------------------------------------------
GATE_TARGETS = [
    "ci",  # build-test: `make ci`
    "tidy-check",  # build-test: `make tidy-check`
    "build",  # build-test: `make build`
    "test-integration",  # build-test, cache-matrix-leg
    "test-integration-shuffle",  # build-test, cache-matrix-leg
    "fixtures",  # fixtures lane: `make fixtures`
    "fixtures-verify",  # fixtures lane: `make fixtures-verify`
    "govulncheck",  # govulncheck lane: `make govulncheck`
]

# The ONLY accepted values. Anything else — including another shell that happens
# to be a real shell — is refused, because this guard cannot know whether an
# unfamiliar shell honours `-e`.
APPROVED_SHELL = "/usr/bin/env bash"
APPROVED_SHELLFLAGS = "-eu -o pipefail -c"

# Special variables whose assignment can disarm every recipe at once.
FORBIDDEN_ASSIGNMENTS = ("MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS")
CONTROLLED_ASSIGNMENTS = ("SHELL", ".SHELLFLAGS")

# Make's recipe-line prefixes. `-` is the dangerous one and is invisible to
# `make --dry-run`; `+` forces execution even under `-n` and is not something a
# gate recipe has any use for.
RECIPE_PREFIX_CHARS = "@-+"

# Suffixes that throw away a command's exit status.
SWALLOWING_SUFFIXES = (
    "|| true", "|| :", "|| exit 0", "|| /bin/true", "|| /usr/bin/true",
    "; true", "; :", "; exit 0",
)

# GNU make 3.81 says "overriding commands for target"; GNU make 4.x says
# "overriding recipe for target". Matching only one wording is how a check
# silently stops working on the platform that actually gates merges.
DUPLICATE_TARGET_RE = re.compile(r"(overriding|ignoring old)\s+(commands|recipe)\s+for\s+target", re.I)

# Flags that make a failing recipe not fail the build, or make recipes not run
# at all. `n` and `p` are OURS — this guard invokes `make -pn` — so they are
# expected; anything else is refused by name whatever it is.
DANGEROUS_FLAG_WORDS = (
    "--ignore-errors", "--keep-going", "--touch", "--question", "--dry-run",
    "--just-print", "--recon", "--old-file", "--assume-old",
)

MAKE_CONDITIONALS = ("ifeq", "ifneq", "ifdef", "ifndef")


class Guard:
    def __init__(self) -> None:
        self.failed = False

    def ok(self, msg: str) -> None:
        print(f"  ok    {msg}")

    def fail(self, msg: str, *detail: str) -> None:
        print(f"  FAIL  {msg}", file=sys.stderr)
        for d in detail:
            print(f"        {d}", file=sys.stderr)
        self.failed = True


def clean_env() -> dict:
    """The environment with make's own flag variables removed.

    They are checked SEPARATELY (check_environment); stripping them here stops
    an inherited `MAKEFLAGS=-i` from polluting what the resolver reports about
    the FILES, which is a different question with a different answer.
    """
    return {k: v for k, v in os.environ.items() if k not in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "MAKELEVEL")}


def run_make(root: Path, args: list[str]):
    return subprocess.run(
        ["make", *args],
        cwd=str(root),
        env=clean_env(),
        capture_output=True,
        text=True,
    )


# --------------------------------------------------------------- resolver ---


def resolve_database(g: Guard, root: Path, target: str):
    """Ask make for its resolved variable database.

    `make -pn TARGET` expands variables, applies `include`s and resolves
    duplicate-target overrides before printing, so this is what will REALLY be
    used — not what the file looks like. Returns (variables, makefile_list) or
    (None, None) if make could not resolve the target at all.
    """
    proc = run_make(root, ["-pn", target])
    if proc.returncode != 0:
        g.fail(
            f"`make -pn {target}` failed (exit {proc.returncode}); the lane's real shape could not be established.",
            "A gate whose shape cannot be determined is not a gate.",
            *(proc.stderr or proc.stdout).strip().splitlines()[:8],
        )
        return None, None

    variables: dict[str, str] = {}
    oneshell = False
    for line in proc.stdout.splitlines():
        if line.startswith(".ONESHELL:"):
            oneshell = True
            continue
        m = re.match(r"^([A-Za-z_.][A-Za-z0-9_.-]*)\s*[:+?]?=\s?(.*)$", line)
        if m and m.group(1) not in variables:
            variables[m.group(1)] = m.group(2)
    variables["__ONESHELL__"] = "yes" if oneshell else ""
    files = [f for f in variables.get("MAKEFILE_LIST", "").split() if f]
    return variables, files


def check_resolved(g: Guard, target: str, variables: dict) -> None:
    shell = variables.get("SHELL")
    if shell is None:
        g.fail(f"make reports no SHELL at all while resolving `{target}`")
    elif shell.strip() != APPROVED_SHELL:
        g.fail(
            f"make resolves SHELL to {shell.strip()!r} while resolving `{target}`, not {APPROVED_SHELL!r}.",
            "A SHELL pointed at a no-op (`/usr/bin/true`, `:`, `/bin/echo`) makes EVERY recipe in this",
            "repository exit 0 without running — `make ci` included — while every gate stays green.",
            "This is the single line that turns the whole merge rule into a formality.",
        )
    else:
        g.ok(f"`{target}`: make resolves SHELL to the approved {APPROVED_SHELL}")

    flags = variables.get(".SHELLFLAGS")
    if flags is None:
        g.fail(f"make reports no .SHELLFLAGS while resolving `{target}`")
    elif flags.strip() != APPROVED_SHELLFLAGS:
        g.fail(
            f"make resolves .SHELLFLAGS to {flags.strip()!r} while resolving `{target}`, not {APPROVED_SHELLFLAGS!r}.",
            "Dropping `-e` makes a recipe continue after a failing command and report the exit status of",
            "the LAST one; dropping `-o pipefail` hides a failure on the left of a pipe.",
        )
    else:
        g.ok(f"`{target}`: make resolves .SHELLFLAGS to the approved {APPROVED_SHELLFLAGS!r}")

    if variables.get("__ONESHELL__"):
        g.fail(
            f"`.ONESHELL:` is in effect while resolving `{target}`.",
            "Under .ONESHELL make passes the WHOLE recipe to one shell invocation, and the `@`/`-`",
            "prefixes then apply only to the first line — which changes what every other check here",
            "means. A gate recipe has no use for it.",
        )
    else:
        g.ok(f"`{target}`: `.ONESHELL:` is not in effect")

    # MAKEFLAGS. This guard invoked make as `-pn`, so `p` and `n` are ours.
    # Anything else was added by a file or by the environment.
    raw = variables.get("MAKEFLAGS", "")
    words = raw.split()
    cluster = ""
    rest = []
    for w in words:
        if not cluster and not w.startswith("-") and "=" not in w:
            cluster = w
        else:
            rest.append(w)
    extra = sorted(set(cluster) - set("pn"))
    bad_words = [w for w in rest if w in DANGEROUS_FLAG_WORDS or w in ("-i", "-k", "-t", "-q")]
    if extra or bad_words:
        g.fail(
            f"make resolves MAKEFLAGS to {raw!r} while resolving `{target}`.",
            f"This guard invoked `make -pn`, so 'p' and 'n' are its own; everything else was added: "
            f"{''.join(extra) or ''}{' ' if extra and bad_words else ''}{' '.join(bad_words)}",
            "`-i` / `--ignore-errors` makes make ignore EVERY recipe's exit status, so `make ci` exits 0",
            "with the whole gate failing underneath it. `-k`, `-t` and `-q` are the same class.",
        )
    else:
        g.ok(f"`{target}`: MAKEFLAGS carries nothing beyond this guard's own -pn ({raw!r})")


def check_warnings(g: Guard, root: Path, target: str) -> None:
    """A duplicate target means the recipe a reader sees is not the one that runs."""
    proc = run_make(root, ["--dry-run", "--no-print-directory", target])
    stderr = proc.stderr or ""
    if DUPLICATE_TARGET_RE.search(stderr):
        g.fail(
            f"make reports a DUPLICATE definition while resolving `{target}`:",
            *[ln for ln in stderr.splitlines() if DUPLICATE_TARGET_RE.search(ln)],
            "make runs the LAST definition. A reader — and any text-based check — sees the first, so a",
            "duplicate can replace a whole gate recipe with `@true` and leave the file looking correct.",
        )
        return
    # Other warnings are REPORTED but do not fail: GNU make 4.3 emits a benign
    # "modification time in the future" warning on some mounted filesystems, and
    # a guard that goes red for that is a guard people route around. Saying so
    # here rather than silently ignoring them.
    others = [ln for ln in stderr.splitlines() if "warning:" in ln.lower()]
    if others:
        print(f"  note  make emitted {len(others)} non-duplicate warning(s) resolving `{target}` (not a failure):")
        for ln in others[:5]:
            print(f"        {ln}")
    g.ok(f"`{target}`: make reports no duplicate-definition override")


# ------------------------------------------------------------------- text ---


RULE_RE = re.compile(r"^([^\t#=:][^#=:]*):(?!=)([^=]*)$")


def prerequisite_closure(root: Path, files: list[str], seeds: list[str]) -> list[str]:
    """Expand the named gate targets to everything they depend on.

    `ci` is one word in a workflow and ten lanes in the Makefile. Checking only
    the word would leave `test-race`'s recipe — the actual test run — outside
    every recipe check here, which is precisely the gap a `-` prefix would use.
    So the closure is COMPUTED from the rules rather than listed, and a lane
    added to `ci` is covered without editing this file.
    """
    prereqs: dict[str, list[str]] = {}
    for rel in files:
        try:
            text = (root / rel).read_text()
        except OSError:
            continue
        for line in text.split("\n"):
            if line.startswith("\t") or line.lstrip().startswith("#"):
                continue
            m = RULE_RE.match(line)
            if not m:
                continue
            names = m.group(1).split()
            deps = m.group(2).split("#")[0].replace("|", " ").split()
            for n in names:
                if n.startswith(".") or "%" in n:
                    continue  # .PHONY, .SHELLFLAGS, pattern rules
                prereqs.setdefault(n, [])
                prereqs[n].extend(d for d in deps if not d.startswith("$"))

    out: list[str] = []
    queue = list(seeds)
    while queue:
        t = queue.pop(0)
        if t in out:
            continue
        out.append(t)
        for d in prereqs.get(t, []):
            if d not in out:
                queue.append(d)
    return out


def logical_recipe_lines(lines: list[str], start: int):
    """Collect one target's recipe from Makefile text, joining continuations."""
    recipe = []
    i = start
    while i < len(lines):
        line = lines[i]
        if line.strip() == "" or line.lstrip().startswith("#"):
            i += 1
            continue
        if not line.startswith("\t"):
            break
        first, body = i, line[1:]
        while body.rstrip().endswith("\\") and i + 1 < len(lines):
            i += 1
            body = body.rstrip()[:-1] + " " + lines[i].lstrip("\t")
        recipe.append((first + 1, body.strip()))
        i += 1
    return recipe


def check_text(g: Guard, root: Path, files: list[str], targets: list[str], seeds: list[str]) -> None:
    """Read the files make said it read.

    MAKEFILE_LIST comes from make itself, so an `include` cannot hide a file
    from this scan — which matters, because an included makefile carrying
    `MAKEFLAGS += -i` no-ops the whole repository just as well as the root one.
    """
    if not files:
        g.fail("make reported an empty MAKEFILE_LIST; there is nothing to read")
        return
    g.ok(f"make read {len(files)} makefile(s): {', '.join(files)}")

    assignment_re = re.compile(
        r"^\s*(?:export\s+|override\s+)*(" + "|".join(
            re.escape(n) for n in (*CONTROLLED_ASSIGNMENTS, *FORBIDDEN_ASSIGNMENTS)
        ) + r")\s*([:+?!]?=)\s*(.*?)\s*$"
    )

    definitions: dict[str, list[str]] = {t: [] for t in targets}
    seen_shell = seen_shellflags = False

    for rel in files:
        path = root / rel
        try:
            text = path.read_text()
        except OSError as err:
            g.fail(f"cannot read {rel}, which make says it read: {err}")
            continue
        lines = text.split("\n")
        is_root = Path(rel).name == "Makefile" and Path(rel).parent in (Path("."), Path(""))

        for n, line in enumerate(lines, 1):
            if line.startswith("\t"):
                continue  # a recipe line, handled below
            m = assignment_re.match(line)
            if m:
                name, op, value = m.group(1), m.group(2), m.group(3)
                if name in FORBIDDEN_ASSIGNMENTS:
                    g.fail(
                        f"{rel}:{n} assigns {name}: `{line.strip()}`",
                        "No assignment to MAKEFLAGS/GNUMAKEFLAGS is legitimate here, whatever its value.",
                        "`MAKEFLAGS += -i` is one line and makes EVERY recipe in this repository exit 0",
                        "without its failures counting — measured on GNU Make 3.81 and 4.3.",
                    )
                    continue
                want = APPROVED_SHELL if name == "SHELL" else APPROVED_SHELLFLAGS
                if not is_root:
                    g.fail(
                        f"{rel}:{n} assigns {name}, but only the root Makefile may: `{line.strip()}`",
                        "An included makefile is a second file a reviewer may not open.",
                    )
                elif op != ":=" or value != want:
                    g.fail(
                        f"{rel}:{n} sets {name} to something other than the approved value: `{line.strip()}`",
                        f"Approved: `{name} := {want}`",
                    )
                else:
                    if name == "SHELL":
                        seen_shell = True
                    else:
                        seen_shellflags = True
                continue

            if line.strip().startswith(".ONESHELL"):
                g.fail(f"{rel}:{n} declares `.ONESHELL`: `{line.strip()}`",
                       "It changes what a `-` prefix means, and a gate recipe has no use for it.")

            for t in targets:
                if re.match(r"^%s\s*:(?!=)" % re.escape(t), line):
                    definitions[t].append(f"{rel}:{n}")
                    # Guard against a gate target hidden inside a conditional:
                    # which recipe runs would then depend on a variable.
                    depth = 0
                    for prev in lines[: n - 1]:
                        head = prev.strip().split(" ")[0]
                        if head in MAKE_CONDITIONALS:
                            depth += 1
                        elif head == "endif":
                            depth = max(0, depth - 1)
                    if depth > 0:
                        g.fail(
                            f"{rel}:{n} defines the gate target `{t}` inside a make conditional.",
                            "Which recipe runs would depend on a variable, so the recipe a reader sees is",
                            "not necessarily the one that executes. Gate targets must be unconditional.",
                        )
                    check_recipe(g, rel, t, logical_recipe_lines(lines, n))

    if not seen_shell or not seen_shellflags:
        g.fail(
            "the root Makefile does not carry BOTH approved assignments.",
            f"Required, exactly: `SHELL := {APPROVED_SHELL}` and `.SHELLFLAGS := {APPROVED_SHELLFLAGS}`.",
            "Without them make uses /bin/sh with no -e, and a failing command mid-recipe does not fail the lane.",
        )
    else:
        g.ok("the root Makefile pins SHELL and .SHELLFLAGS to the approved values, and nothing else assigns them")

    undefined = []
    for t in targets:
        where = definitions[t]
        if not where:
            if t in seeds:
                g.fail(
                    f"gate target `{t}` is not defined in any makefile make read.",
                    "A required CI lane invokes it by that name.",
                )
            else:
                # A prerequisite with no rule of its own is an ordinary FILE
                # dependency, not a missing lane. Named rather than silently
                # dropped, so the transcript says what was and was not scanned.
                undefined.append(t)
        elif len(where) > 1:
            g.fail(
                f"gate target `{t}` is defined {len(where)} times: {', '.join(where)}",
                "make runs the LAST definition and REPLACES the earlier recipe. The lane must have one recipe.",
            )
        else:
            g.ok(f"gate target `{t}` is defined exactly once ({where[0]})")
    if undefined:
        print(f"  note  {len(undefined)} prerequisite(s) have no rule and are treated as file dependencies, "
              f"not lanes: {', '.join(sorted(undefined))}")


def check_recipe(g: Guard, rel: str, target: str, recipe) -> None:
    """Refuse recipe lines whose failure would not fail the lane.

    This reading is the only one that can see a `-` prefix: `make --dry-run`
    prints the command WITHOUT it, so the resolved recipe looks identical to a
    correct one.
    """
    if not recipe:
        # Legitimate for an aggregate like `ci:` whose work is its prerequisites.
        return
    for lineno, body in recipe:
        prefix = ""
        while body and body[0] in RECIPE_PREFIX_CHARS:
            prefix += body[0]
            body = body[1:].lstrip()
        if "-" in prefix:
            g.fail(
                f"{rel}:{lineno} — gate target `{target}` has a recipe line prefixed `-`: `{prefix}{body}`",
                "make IGNORES that line's exit status, and `make --dry-run` prints the command WITHOUT the",
                "`-`, so no scan of the resolved recipe can see it: the command can fail, print its failure,",
                "and the lane still exits 0.",
            )
        if "+" in prefix:
            g.fail(
                f"{rel}:{lineno} — gate target `{target}` has a recipe line prefixed `+`: `{prefix}{body}`",
                "`+` forces the line to run even under `-n`, which is how a guard's own dry-run probe can be",
                "made to execute something. A gate recipe has no use for it.",
            )
        stripped = body.rstrip()
        for suffix in SWALLOWING_SUFFIXES:
            if stripped.endswith(suffix):
                g.fail(
                    f"{rel}:{lineno} — gate target `{target}` has a recipe line ending `{suffix}`: `{stripped}`",
                    "The command's exit status is discarded. A check whose failure is swallowed is not a check.",
                )
                break


# ------------------------------------------------------------ environment ---


def check_environment(g: Guard) -> None:
    """`MAKEFLAGS=-i make ci` appears in no file at all."""
    bad = False
    for name in ("MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS"):
        value = os.environ.get(name, "")
        if not value:
            continue
        words = value.split()
        cluster = words[0] if words and not words[0].startswith("-") and "=" not in words[0] else ""
        hits = sorted(set(cluster) & set("ikt q".replace(" ", "")))
        hits += [w for w in words if w in DANGEROUS_FLAG_WORDS or w in ("-i", "-k", "-t", "-q")]
        if hits:
            g.fail(
                f"the environment sets {name}={value!r}, which carries {', '.join(hits)}.",
                "make reads it as if it were on the command line, so it can disarm every recipe without",
                "appearing in any file. Unset it for this lane.",
            )
            bad = True
    if not bad:
        g.ok("the environment carries no MAKEFLAGS/GNUMAKEFLAGS that would disarm a recipe")


# ------------------------------------------------------------------- main ---


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent,
                    help="directory holding the Makefile (default: the repository root)")
    ap.add_argument("--targets", default=",".join(GATE_TARGETS),
                    help="comma-separated gate targets (default: every target a required CI lane invokes)")
    args = ap.parse_args()

    root = args.root.resolve()
    targets = [t.strip() for t in args.targets.split(",") if t.strip()]

    g = Guard()
    print(f"make-integrity-guard: {root}")
    print(f"  gate targets: {', '.join(targets)}")

    if not (root / "Makefile").exists():
        g.fail(f"{root}/Makefile does not exist")
        print("make-integrity-guard: FAILED", file=sys.stderr)
        return 1

    check_environment(g)

    # One resolver pass names every file make reads, including everything an
    # `include` pulls in. The text reading then covers all of them.
    variables, files = resolve_database(g, root, targets[0])
    if variables is None:
        print("make-integrity-guard: FAILED", file=sys.stderr)
        return 1

    # The named targets are what CI invokes; the CLOSURE is what actually runs.
    # `make ci` is one word in a workflow and ten lanes here, and it is
    # `test-race`'s recipe — not `ci`'s, which has none — that runs the tests.
    closure = prerequisite_closure(root, files, targets)
    derived = [t for t in closure if t not in targets]
    print(f"  closure: {len(closure)} target(s); {len(derived)} reached through prerequisites"
          + (f" ({', '.join(derived)})" if derived else ""))

    check_text(g, root, files, closure, targets)

    # The resolver checks read make's own variable database, which is global to
    # the invocation, and make reports a duplicate definition at PARSE time — so
    # one pass per NAMED target covers the closure as well, and also proves each
    # name CI invokes is a target make can resolve at all.
    for t in targets:
        v, _ = resolve_database(g, root, t)
        if v is None:
            continue
        check_resolved(g, t, v)
        check_warnings(g, root, t)

    if g.failed:
        print("make-integrity-guard: FAILED", file=sys.stderr)
        print("A make-driven gate that can be turned into a no-op is not a gate. Fix the Makefile.", file=sys.stderr)
        return 1
    print(f"make-integrity-guard: passed ({len(targets)} gate target(s))")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
