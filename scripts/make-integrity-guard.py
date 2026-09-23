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

Plus the ENVIRONMENT, because `MAKEFLAGS=-i make ci` never appears in any file —
and neither does `GO=true`, which a `?=` assignment lets win. With `--workflow`
(the only invocation a floor lane may use: ci-required-guard pins the anchor
step byte-equal to it) the environment check is STRICT. The mode is chosen by
that argument, never by the environment; round 2 chose it from MAKELEVEL's mere
presence, which any earlier step could set (PR#9 re-verification, R-2). See
check_environment and check_environment_overrides.

WHAT THIS GUARANTEES — stated at exactly its real strength
-----------------------------------------------------------

While this program runs as `--workflow`, as an unconditional workflow step
IMMEDIATELY before each `make` step in a required lane, and its exit status is
not discarded:

  * **make runs only on REVIEWED Makefile bytes** (sweep B5). Before make is
    invoked AT ALL — in both modes — every file make will read must match its
    sha256 in `.github/pinned-makefiles.yml`. The set of files make will read
    is determined WITHOUT running any makefile: the root `Makefile` (a
    `GNUmakefile` or `makefile` beside it, which make would read instead, is
    refused), plus every literal `include` / `-include` / `sinclude` / `load`
    path in pinned bytes, transitively. Refusing an unpinned or computed
    directive is sound ONLY because the bytes that contain the directives are
    themselves digest-pinned: this is a reading of REVIEWED text, not a model
    of make's parser defending against hostile text — a changed byte never
    reaches it. `$(eval …)` / `$(guile …)`, which could manufacture a
    directive, are refused. MAKEFILES, which would add a file, is refused and
    scrubbed. make's first invocation is then ONE `make -q` naming every
    pinned makefile as a goal — -q applies in make's remake phase only to
    makefiles that are goals — to ask whether it would REMAKE one of them from
    something nobody pinned (make does that even under -n). -q runs no ORDINARY
    recipe; a `+` or `$(MAKE)` recipe line still runs under it, and such a line
    can come only from the pinned, reviewed bytes (make's builtin RCS/SCCS
    checkout rule is a `+` line that expands to nothing for a file that exists,
    and every pinned file must exist). After make has run, its own
    MAKEFILE_LIST must equal the pinned set and the pinned bytes must be
    unchanged. Any failure: make is not invoked (or, for the probe, invoked
    only as that one `make -q`; for the last two, the lane fails).
    Those reviewed bytes are NOT inert. Reading them runs the reviewed
    parse-time calls — today `$(shell git rev-parse …)` and `$(shell date …)`
    at Makefile:22-23 — and the ok line lists every such site it finds.
  * every process this program starts — make, and the bash it asks what `make`
    is — gets an environment WITHOUT GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT,
    GITHUB_STATE, GITHUB_STEP_SUMMARY or any other variable whose value is a
    file in the runner's command-file directory (clean_env), nor MAKEFLAGS,
    MAKEFILES, BASH_ENV or ENV;
  * a Makefile (or anything it `include`s) whose TEXT assigns `SHELL`,
    `.SHELLFLAGS`, `MAKEFLAGS` or `GNUMAKEFLAGS`, or sets `.ONESHELL`, fails the
    lane BY NAME before make is ever invoked — the text checks run on the
    pinned read set, before the gate (fix round 1; they used to run after four
    make processes). A value only make can resolve (a computed variable name,
    say) is refused by the RESOLVER, which necessarily runs make first — on
    the pinned bytes only;
  * a gate target whose recipe carries a `-` / `@-` prefix or a `|| true`-family
    suffix fails the lane by name, before make is invoked;
  * a gate target defined twice — where make silently runs the LAST definition
    while a reader, and any text-based check, sees the first — fails the lane by
    name.
  * in its OWN process: a set MAKEFLAGS / GNUMAKEFLAGS / MFLAGS; any present
    MAKELEVEL / MAKE_RESTARTS / MAKEOVERRIDES / MAKECMDGOALS; any present
    variable the makefiles take from the environment (`?=`, or referenced and
    never assigned — today GO, SQLC, GOFLAGS, RELEASE, COMMIT, BUILT_AT); a set
    MAKEFILES / BASH_ENV / ENV; a SHELL that is not a shell; and a `make` that
    is not a FILE whose real name is make in a system directory — each fails
    the lane by name. Because the step is adjacent to make, a $GITHUB_ENV or
    $GITHUB_PATH write by any EARLIER step is in this process when it runs.

WHAT IT DOES NOT GUARANTEE — stated, not implied
-------------------------------------------------

This list is deliberately NOT called exhaustive. The previous one was, and a
verifier found thirteen things missing from it (PR#9 VERIFY, § 3b).

  * **This guard never reads the workflow's own `make` invocation.** It checks
    the Makefile, everything it includes, and its OWN environment — which is
    now the point rather than a gap. It runs IMMEDIATELY before every make
    step, so a `$GITHUB_ENV` or `$GITHUB_PATH` write by an earlier step lands
    in this process exactly as it would in make's — and is refused here IF it
    touches a variable this guard checks (listed above). A write of any other
    variable (`GOTOOLCHAIN`, `GODEBUG`, …) is not refused. The
    step's own argv and keys are `ci-required-guard.py`'s checks 8b and 8c,
    which pin them to byte-equal literals in `.github/pinned-steps.yml` rather
    than parsing them.

  * **It cannot see what an earlier step did to the machine.** Its assertions
    cover the Makefile and its includes, its own environment, and what `make`
    resolves to. They do not cover a Go toolchain that was replaced, a test
    file that was rewritten, or a `python3` that was swapped, by an arbitrary
    `run:` step or a `uses:` action earlier in the job. REVIEW is the control
    for that, and CODEOWNERS is ADVISORY.

  * **Review is the control for WHAT the Makefile says.** The digest pin makes
    make run only on bytes that were reviewed together with their pin. A
    reviewer who approves a malicious Makefile together with its pin update
    defeats it, and CODEOWNERS is advisory until the owner's ruleset exists.
    The pin moves the question from "can this guard parse make?" to "did a
    human approve these bytes?" — it does not answer the second one.

  * **Reviewed parse-time calls run whatever PATH resolves.** Makefile:22-23's
    `git` and `date` are looked up on PATH while make reads the file, so a
    program planted earlier on PATH by another step runs during the anchor.
    That is the "what an earlier step did to the machine" boundary below.

  * **The command-file scrub hides NAMES, not the directory.** A process that
    lists `$RUNNER_TEMP/_runner_file_commands/` can still find the files. The
    scrub is defence in depth; the digest pin is what keeps unreviewed code
    from running while make reads the Makefile.

  * **The digest is taken at one moment.** A process left running by an
    earlier step could swap a pinned file between this guard's read and
    make's; the re-check after make narrows that window and does not close
    it. Same boundary.

  * **It checks what `make` IS, not what it DOES.** A real FILE named make in
    /usr/local/bin that forwards this guard's own `make -pn` and exits 0
    otherwise passes; planting one needs an earlier step to write to the
    machine (the bullet above). The ok line says exactly this rather than
    claiming to have ruled it out (PR#9 re-verification, R-5).

  * **Without `--workflow` it is not a control.** That is how `make ci-guard`
    runs it for local parity: make itself exports MAKEFLAGS to the recipe, so
    the words are checked against an ALLOWLIST measured on GNU Make 3.81 and
    4.3 instead of being refused outright, and variables the makefiles take
    from the environment are reported, not refused.

  * **A wrapper script or composite action that calls make is not read.** It
    carries no `make` token on the workflow line. `ci-required-guard.py` check
    8c bounds the damage — the invocations a lane MUST run are asserted present
    — but a lane may run one in addition.

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
    make will really run them. The direct `go test` steps in `build-test` are
    the separate control for "a neutered make cannot make a suite silent"; from
    sweep B1 they cover the INTEGRATION suite as well as the unit suite, and
    `go-test-report.py` is what makes an EMPTY suite red.
  * It does not sandbox make. A recipe that runs a wrapper script which
    itself lies is out of scope here.

Usage:
    make-integrity-guard.py [--root DIR] [--targets a,b,c] [--workflow]

The pin is read from DIR/.github/pinned-makefiles.yml. scripts/scripts_test.go
and scripts/makefiledigest_test.go drive this program against
scripts/testdata/makeguard/ (each fixture carries its own pin) and against byte
mutations of a temporary copy of the real tree, under
scripts/testdata/spawn-recorder.py, which records every process it starts.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

# The pin reader shared with ci-required-guard.py check 11. No bytecode is
# written next to it: a __pycache__ in scripts/ would dirty the tree the anchor
# is judging.
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import makefile_pin as mp  # noqa: E402

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


# The runner's step-command files. GitHub applies what a step writes to them to
# the NEXT step: $GITHUB_ENV sets variables, $GITHUB_PATH prepends PATH. The
# anchor passes, and the very next step is the pinned `make` step — so nothing
# the anchor starts may be handed these names. Read (not ported as a control)
# from vizra-search's anchor, chore/m0-ci-hardening@4476ad5, lines 183-196.
RUNNER_COMMAND_FILES = ("GITHUB_ENV", "GITHUB_PATH", "GITHUB_OUTPUT", "GITHUB_STATE", "GITHUB_STEP_SUMMARY")

# The directory the runner creates those files in (measured name on the hosted
# runner: $RUNNER_TEMP/_runner_file_commands/). A command file GitHub adds later
# lands in the same directory, so ANY variable whose value is a file in it is
# dropped too — whatever it is called.
RUNNER_FILE_COMMANDS_DIR = "_runner_file_commands"

# Dropped from every subprocess this guard starts. make's flag variables are
# checked SEPARATELY (check_environment) and must not pollute what the resolver
# reports about the FILES; MAKEFILES would add a makefile nobody pinned; a
# non-interactive bash sources BASH_ENV / ENV, and the Makefile's own reviewed
# `$(shell …)` calls run through bash.
_ALWAYS_DROPPED = ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "MAKELEVEL", "MAKEFILES", "BASH_ENV", "ENV")


def _is_runner_command_file(value: str, command_dirs: set[str]) -> bool:
    if not value or os.pathsep in value or "\n" in value:
        return False
    parent = os.path.dirname(os.path.normpath(value))
    return bool(parent) and (os.path.basename(parent) == RUNNER_FILE_COMMANDS_DIR or parent in command_dirs)


def clean_env(environ=None) -> dict:
    """The environment for EVERY process this guard starts — make and bash alike.

    Removes make's own flag variables, MAKEFILES, BASH_ENV and ENV, the five
    named runner command-file variables, and any other variable whose value is a
    file in the runner's command-file directory.

    This is defence in depth, NOT the control. It stops a process this guard
    starts from addressing a command file BY NAME. It does not make the file
    unfindable: the directory is on disk, and a program that lists it can still
    write to it. What stops a Makefile from running anything unreviewed while
    it is read is the digest pin (check_makefile_pin), which runs first.
    """
    env = dict(os.environ if environ is None else environ)
    command_dirs = {
        os.path.dirname(os.path.normpath(env[n])) for n in RUNNER_COMMAND_FILES if env.get(n)
    } - {""}
    drop = set(_ALWAYS_DROPPED) | set(RUNNER_COMMAND_FILES)
    drop |= {k for k, v in env.items() if _is_runner_command_file(v, command_dirs)}
    return {k: v for k, v in env.items() if k not in drop}


# Every make invocation this guard makes goes through here, and none happens
# before check_makefile_pin has passed: main() returns first. The counter is
# what the "make was NOT invoked" line reports, so it cannot drift from reality.
MAKE_INVOCATIONS = 0


def run_make(root: Path, args: list[str]):
    global MAKE_INVOCATIONS
    MAKE_INVOCATIONS += 1
    return subprocess.run(
        ["make", *args],
        cwd=str(root),
        env=clean_env(),
        capture_output=True,
        text=True,
    )


# ------------------------------------------------------- the Makefile digest pin ---
#
# THE CONTROL for "make runs only on reviewed bytes" (chair ruling, tick 132, on
# docs/evidence/warroom/2026-09-23-anchor-preflight-DESK-REVIEW-security.md
# FINDING 4). This guard reads the Makefile database with `make -pn`, and GNU
# Make EVALUATES the file while reading it — `$(shell …)`, `$(file …)`, `!=`,
# `+` and `$(MAKE)` recipe lines, makefile-remake rules, `.SECONDEXPANSION`
# prerequisites. So one Makefile line could run code DURING the anchor step and
# write the next step's environment after the anchor had passed.
#
# A scanner over the TEXT would have to re-implement make's parser to be sound,
# and vizra-search's attempt missed two constructs in its first review. So the
# check is on the BYTES instead: every file make will read must match a sha256
# in .github/pinned-makefiles.yml, and make is not invoked at all until it does.
# A Makefile change then merges only together with a reviewed pin update — the
# same discipline as .github/pinned-steps.yml.
#
# The pin is read, and what make will read is decided, by scripts/makefile_pin.py
# — the SAME function ci-required-guard.py check 11 calls, so the static check
# refuses exactly what this anchor refuses.

PIN_FILE = mp.PIN_FILE

_PIN_FAIL_DETAIL = {
    "mismatch": lambda p: (
        "These are not the bytes that were reviewed. make would EVALUATE them while this guard",
        "reads its database — `$(shell …)` and friends run at parse time — so it is not invoked.",
        f"If the edit is intended, update {PIN_FILE} in the same reviewed diff:",
        f"  shasum -a 256 {p.file}",
    ),
    "pin": lambda p: ("make is only ever run on bytes a reviewer approved together with this pin.",),
    "unpinned": lambda p: (
        "Every file make reads is pinned by digest, or make is not run: an unpinned include is",
        "bytes nobody reviewed, evaluated while this guard reads the Makefile database.",
    ),
    "stale": lambda p: (
        "The pin must be exactly what make reads; a stale entry means the pin and the Makefile drifted.",
    ),
}


def check_makefile_pin(g: Guard, root: Path):
    """Digest every file make will read, BEFORE make is invoked. Returns (files, digests) or None.

    None means: do NOT invoke make. The caller returns without running it.
    """
    r = mp.verify_pin(root)
    for p in r.problems:
        g.fail(p.message, *_PIN_FAIL_DETAIL.get(p.kind, lambda _p: ())(p))
    if not r.ok:
        return None
    g.ok(
        f"make runs only on REVIEWED bytes: the {len(r.order)} file(s) it will read ({', '.join(r.order)}) "
        f"match the sha256 pins in {PIN_FILE}, checked before make was invoked. Those bytes are not "
        f"inert — reading them runs the reviewed parse-time call(s) "
        + ("; ".join(r.sites) if r.sites else "(none)")
        + ". Review of a Makefile together with its pin is the control; CODEOWNERS is advisory."
    )
    return r.order, {rel: r.digests[rel] for rel in r.order}


# The pinned makefiles, named as GOALS on every make command line after the
# probe. GNU make remakes an out-of-date makefile BEFORE anything else — and
# does it even under -n, -q and -t, because an out-of-date makefile would give
# wrong answers (manual §3.5). -n / -q apply in that phase ONLY to makefiles
# that are also command-line goals; for every other makefile make clears them
# and runs the remake recipe for real. MEASURED on GNU Make 3.81 and 4.3 with a
# newer sibling `Makefile.sh` (the builtin rule `%: %.sh`): `make -pn ci`
# EXECUTED `cat Makefile.sh >Makefile` and re-read the result; `make -q Makefile`
# exited 1 and changed nothing; `make -pn Makefile ci` printed that recipe
# without running it.
PINNED_MAKEFILE_GOALS: list[str] = []


def check_no_pinned_makefile_would_be_remade(g: Guard, root: Path, files: list[str]) -> None:
    """Ask make, with ONE `make -q` naming EVERY pinned makefile, whether it would remake any.

    A makefile can be remade from files nobody pinned: make's own builtin
    implicit rules turn a newer `Makefile.sh` into the Makefile (`cat`) and a
    `Makefile.c` into one (the C compiler), with no line in any makefile saying
    so. The static reading of the pinned bytes cannot see make's builtin rules,
    so make itself is asked, on the pinned bytes only.

    ONE invocation, every pinned file a goal. Fix round 1 (PR#10 VERIFY,
    FINDING 1): the probe used to run `make -q Makefile`, then `make -q a.mk`, …
    — and in `make -q Makefile` the pinned include a.mk is NOT a goal, so make
    really ran `cat a.mk.sh >a.mk`, re-read the result and reported "up to
    date". Measured on 3.81 and 4.3; `make -q Makefile a.mk b.mk` exits 1 and
    changes nothing.

    What -q does NOT stop: GNU make runs a recipe line that starts with `+` or
    names `$(MAKE)` even under -q (and -n). Such a line can come only from the
    pinned, reviewed bytes, or from make's builtin RCS/SCCS checkout rule
    (`+$(if $(wildcard $@),,$(CO) …)`), which expands to nothing for a file that
    exists — and every pinned file must exist. (Measured by the PR#10 verifier:
    `Makefile,v` and `RCS/Makefile,v` siblings → `make -q` exit 0, nothing run.)
    """
    probes = [files]  # ONE invocation; every pinned makefile is a goal, so -q applies to each
    bad = []
    for goals in probes:
        proc = run_make(root, ["-q", *goals])
        if proc.returncode != 0:
            bad.append((goals, proc.returncode, (proc.stdout + proc.stderr).strip().splitlines()[:4]))
    for goals, code, lines in bad:
        g.fail(
            f"make would REMAKE one of {', '.join(goals)} before reading it (`make -q {' '.join(goals)}` "
            f"exit {code}).",
            "make remakes an out-of-date makefile even under -n, from a rule or a builtin implicit rule",
            "(a newer `Makefile.sh` or `Makefile.c` is enough), and then reads the NEW bytes — which",
            "nobody pinned. -q runs no ordinary recipe; only a `+` or `$(MAKE)` line in the pinned bytes",
            "would have run.",
            *lines,
        )
    if not bad:
        g.ok(f"`make -q {' '.join(files)}` (every pinned makefile a goal, so -q applies to each) reports none "
             f"would be remade; every later make command names them as goals so -n applies to them. -q runs "
             f"no ordinary recipe — only `+`/`$(MAKE)` lines, which can come only from the pinned bytes")


def recheck_pinned_bytes(g: Guard, root: Path, digests: dict[str, str]) -> None:
    """After make ran: the pinned bytes are still the pinned bytes."""
    for rel, want in digests.items():
        try:
            got = mp.sha256((root / rel).read_bytes())
        except OSError as err:
            g.fail(f"{rel} could not be re-read after make ran: {err}")
            continue
        if got != want:
            g.fail(f"{rel} CHANGED while this guard ran make (sha256 {want} -> {got}).",
                   "Something rewrote a pinned makefile during the anchor step — for example a rule that",
                   "remakes it. The next step's make would read bytes nobody reviewed.")


# --------------------------------------------------------------- resolver ---


def resolve_database(g: Guard, root: Path, target: str):
    """Ask make for its resolved variable database.

    `make -pn TARGET` expands variables, applies `include`s and resolves
    duplicate-target overrides before printing, so this is what will REALLY be
    used — not what the file looks like. Returns (variables, makefile_list) or
    (None, None) if make could not resolve the target at all.
    """
    proc = run_make(root, ["-pn", *PINNED_MAKEFILE_GOALS, target])
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
    proc = run_make(root, ["--dry-run", "--no-print-directory", *PINNED_MAKEFILE_GOALS, target])
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


# Make's own variables, which a makefile may reference without assigning.
_MAKE_BUILTIN_VARS = {
    "MAKE", "MAKEFILE_LIST", "CURDIR", "MAKEFLAGS", "MAKECMDGOALS", "SHELL", "MAKELEVEL",
    ".SHELLFLAGS", "MAKE_VERSION", "MAKE_HOST", ".DEFAULT_GOAL", "MFLAGS", "MAKEFILES",
    "VPATH", ".RECIPEPREFIX", ".VARIABLES", ".FEATURES", ".INCLUDE_DIRS", "SUFFIXES",
}
_FUNCTIONS = {
    "shell", "wildcard", "eval", "info", "error", "warning", "foreach", "call", "patsubst",
    "subst", "filter", "filter-out", "sort", "dir", "notdir", "strip", "word", "words",
    "firstword", "lastword", "abspath", "realpath", "if", "or", "and", "origin", "value",
    "addprefix", "addsuffix", "basename", "suffix", "join", "findstring", "flavor", "file",
}
_ASSIGN_RE = re.compile(r"^\s*(?:export\s+|override\s+)*([A-Za-z_][A-Za-z0-9_.]*)\s*(\?=|:{1,3}=|\+=|!=|=)", re.M)
# `$(NAME)` / `${NAME}` — but not the shell's `$${NAME}` inside a recipe.
_REF_RE = re.compile(r"(?<!\$)\$[({]([A-Za-z_][A-Za-z0-9_]*)[)}]")


def check_environment_overrides(g: Guard, root: Path, files: list[str], workflow: bool) -> None:
    """In the WORKFLOW invocation, nothing in the environment may change what a recipe runs.

    Found while fixing round 2 (not by the verifier): the Makefile has
    `GO ?= go`, and `?=` means the ENVIRONMENT wins. `GO=true make test-race`
    exits 0 over a planted failing test where `make test-race` exits 2, and
    round 2's anchor passed it — MAKEFLAGS and friends were checked, the
    variables the Makefile deliberately takes from the environment were not.
    An earlier step's `$GITHUB_ENV` write reaches make exactly as MAKEFLAGS does.

    So, from every file make will read (the pinned read set, includes too): a variable
    assigned with `?=`, or referenced as `$(NAME)` without ever being assigned,
    is one the environment can set — and in `--workflow` mode it must be ABSENT.
    `GOFLAGS` is refused as well whether or not a makefile names it, because
    the go command reads it directly. Local (`make ci-guard`) mode only reports
    them: a developer may legitimately run `GO=go1.27.1 make ci`.
    """
    assigned: dict[str, str] = {}
    refs: set[str] = set()
    for rel in files:
        path = root / rel if not Path(rel).is_absolute() else Path(rel)
        try:
            text = path.read_text()
        except OSError:
            continue
        for m in _ASSIGN_RE.finditer(text):
            if m.group(1) not in assigned or m.group(2) == "?=":
                assigned[m.group(1)] = m.group(2)
        refs |= set(_REF_RE.findall(text))
    from_env = {n for n, op in assigned.items() if op == "?="}
    from_env |= {n for n in refs if n not in assigned} - _MAKE_BUILTIN_VARS - _FUNCTIONS
    from_env.add("GOFLAGS")
    present = sorted(n for n in from_env if n in os.environ)
    if not workflow:
        g.ok(f"[local parity] the environment may set these makefile variables: {sorted(from_env)}"
             + (f"; set now: {present}" if present else ""))
        return
    if present:
        g.fail(
            f"the environment sets {', '.join(f'{n}={os.environ[n]!r}' for n in present)}, and this is "
            f"the WORKFLOW anchor.",
            "The makefiles take these FROM the environment (`?=`, or referenced but never assigned),",
            "so the environment decides what the recipe runs: `GO=true make test-race` exits 0 over a",
            "failing test. In a floor lane nothing may set them; the Makefile's own values must win.",
        )
        return
    g.ok(f"[--workflow] none of the {len(from_env)} variable(s) the makefiles take from the "
         f"environment is set: {sorted(from_env)}")


def check_text(g: Guard, root: Path, files: list[str], targets: list[str], seeds: list[str]) -> None:
    """Read the files make will read — BEFORE make is invoked.

    The list is the pinned read set (makefile_pin.static_read_set over the
    pinned bytes), so an `include` cannot hide a file from this scan — which
    matters, because an included makefile carrying `MAKEFLAGS += -i` no-ops the
    whole repository just as well as the root one. After make has run, its own
    MAKEFILE_LIST is required to equal the same set.
    """
    if not files:
        g.fail("the pinned read set is empty; there is nothing to read")
        return
    g.ok(f"make will read {len(files)} makefile(s), from the pinned bytes: {', '.join(files)}")

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
            g.fail(f"cannot read {rel}, which make will read: {err}")
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
                    f"gate target `{t}` is not defined in any makefile make will read.",
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


# Directories a system `make` legitimately lives in. A `make` resolved anywhere
# else is a stub someone put on PATH — the $GITHUB_PATH evasion, which GitHub
# applies to LATER steps, so it would be invisible to a non-adjacent anchor.
APPROVED_MAKE_DIRS = ("/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin", "/usr/sbin", "/sbin")

# Shells that are not shells: a SHELL pointed at one of these makes every recipe
# a no-op on the platforms that honour the environment's SHELL.
NEUTERED_SHELLS = ("/usr/bin/true", "/bin/true", ":", "/bin/echo", "/usr/bin/echo", "/bin/false", "/usr/bin/false")


# Variables make itself exports to a recipe. In the WORKFLOW invocation the
# anchor is not a recipe, so any of these being present means something planted
# it — typically an earlier step writing to $GITHUB_ENV.
MAKE_INTERNAL_VARS = ("MAKELEVEL", "MAKE_RESTARTS", "MAKEOVERRIDES", "MAKECMDGOALS")
FLAG_VARS = ("MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS")

# LENIENT mode is an ALLOWLIST, not a list of bad letters. These are the words
# GNU make itself puts in MAKEFLAGS/MFLAGS for a recipe such as `make ci-guard`,
# MEASURED on GNU Make 4.3 (ubuntu:24.04, CI's version) and 3.81 (the host):
#   make            MAKEFLAGS=''                            (both)
#   make -j2        ' -j2 --jobserver-auth=3,4'             (4.3)
#                   ' --jobserver-fds=3,4 -j', MFLAGS '- --jobserver-fds=3,4 -j'  (3.81)
#   make -s         's'          make -w  'w'          --no-print-directory
# Anything else — `-ki`, `n`, `--ign`, `e` — is refused, whatever it means.
_ALLOWED_FLAG_WORD = re.compile(
    r"^(?:-|[ws]+|-[ws]|-j\d*|--jobserver-(?:fds|auth)=\S+"
    r"|--print-directory|--no-print-directory|--silent|--quiet)$"
)


def check_environment(g: Guard, workflow: bool) -> None:
    """The anchor's OWN process environment.

    WHICH MODE is chosen by how the anchor is INVOKED, never by the environment.
    Round 2 chose it from `MAKELEVEL is not None`, and an earlier step can put
    `MAKELEVEL=1` (or even an empty `MAKELEVEL=`) in $GITHUB_ENV beside
    `MAKEFLAGS=-ki`. The lenient path then passed `-ki`, `n` and `--ign`, and GNU
    Make 4.3 made a failing recipe exit 0 under each (PR#9 re-verification,
    FINDING R-2). Now:

    --workflow  the invocation pinned byte-for-byte in .github/pinned-steps.yml,
                the ONLY form ci-required-guard accepts as the anchor. STRICT:
                MAKEFLAGS/GNUMAKEFLAGS/MFLAGS must be UNSET (not merely empty or
                free of known flags), and MAKELEVEL/MAKE_RESTARTS/MAKEOVERRIDES/
                MAKECMDGOALS must be ABSENT — the anchor is not a make recipe, so
                if any is present, something planted it.

    (no flag)   `make ci-guard` for local parity, and the scripts_test.go runs.
                Make legitimately exports MAKEFLAGS to a recipe, so every word of
                it must be in a measured ALLOWLIST (_ALLOWED_FLAG_WORD). This mode
                is not a control, and no floor lane can select it: a workflow
                anchor without the pinned `--workflow` is not byte-equal to the
                pin and is refused by ci-required-guard.

    In BOTH modes MAKEFILES, BASH_ENV and ENV must be unset and SHELL must be a
    real shell.
    """
    bad = False
    mode = "--workflow (strict)" if workflow else "local parity (allowlist)"

    if workflow:
        for name in MAKE_INTERNAL_VARS:
            if name in os.environ:
                g.fail(
                    f"the environment has {name}={os.environ[name]!r}, and this is the WORKFLOW anchor.",
                    "The anchor is not a make recipe, so make did not put it there: something else did,",
                    "most likely an earlier step writing to $GITHUB_ENV. MAKELEVEL in particular was how",
                    "the round-2 anchor was talked into a lenient mode (PR#9 re-verification, R-2).",
                )
                bad = True
        for name in FLAG_VARS:
            if name in os.environ:
                g.fail(
                    f"the environment sets {name}={os.environ[name]!r}, and this is the WORKFLOW anchor.",
                    "It must be UNSET: make reads it as if it were typed on the command line, so ANY",
                    "value is a command line nobody reviewed — including ones a blacklist would pass.",
                )
                bad = True
    else:
        for name in FLAG_VARS:
            value = os.environ.get(name, "")
            refused = [w for w in value.split() if not _ALLOWED_FLAG_WORD.match(w)]
            if refused:
                g.fail(
                    f"the environment sets {name}={value!r}; {refused} is not a flag make itself",
                    "exports to a `make ci-guard` recipe. This mode is an ALLOWLIST of the words GNU make",
                    "3.81 and 4.3 were measured to export (-jN, --jobserver-*, s, w, --no-print-directory).",
                )
                bad = True
        if os.environ.get("MAKEOVERRIDES", "").strip():
            g.fail(f"the environment sets MAKEOVERRIDES={os.environ['MAKEOVERRIDES']!r}: a command-line "
                   f"variable override reached the recipe.")
            bad = True

    # MAKEFILES makes make read extra makefiles BEFORE the root one, and make
    # never sets it itself, so it is refused in both modes.
    if os.environ.get("MAKEFILES", "").strip():
        g.fail(
            f"the environment sets MAKEFILES={os.environ['MAKEFILES']!r}.",
            "make reads those files before the root Makefile, so they can assign SHELL or MAKEFLAGS",
            "without appearing in anything this guard scans.",
        )
        bad = True

    # A non-interactive bash SOURCES $BASH_ENV, so it can define a `make` shell
    # function before any recipe or any command runs.
    for name in ("BASH_ENV", "ENV"):
        if os.environ.get(name, "").strip():
            g.fail(
                f"the environment sets {name}={os.environ[name]!r}.",
                "A non-interactive shell sources it, so it can define a `make` function or alias that",
                "shadows the real program before a single recipe runs.",
            )
            bad = True

    shell = os.environ.get("SHELL", "").strip()
    if shell and (shell in NEUTERED_SHELLS or not os.path.isfile(shell) or not os.access(shell, os.X_OK)):
        g.fail(
            f"the environment sets SHELL={shell!r}, which is not a usable shell program.",
            "Some platforms let the environment's SHELL reach make. A SHELL pointed at `true` or `:`",
            "makes every recipe a silent no-op.",
        )
        bad = True

    if not bad:
        if workflow:
            g.ok(f"[{mode}] MAKEFLAGS/GNUMAKEFLAGS/MFLAGS unset; MAKELEVEL/MAKE_RESTARTS/MAKEOVERRIDES/"
                 f"MAKECMDGOALS absent; MAKEFILES/BASH_ENV/ENV unset; SHELL is a real shell")
        else:
            g.ok(f"[{mode}] every MAKEFLAGS/GNUMAKEFLAGS/MFLAGS word is one make exports itself; "
                 f"MAKEFILES/BASH_ENV/ENV unset; SHELL is a real shell")


def check_make_resolves_to_a_real_program(g: Guard) -> None:
    """`make` resolves to a FILE named make in a system directory — not what it DOES.

    Two of the verifier's thirteen evasions never touch a Makefile at all:

        make() { :; } ; make ci          a shell function shadowing make
        PATH=/tmp/sh:$PATH make ci       a stub `make` earlier on PATH

    Both are invisible to any amount of reading of the Makefile, and the second
    can be set up by an EARLIER step through $GITHUB_PATH. This asks the shell
    what `make` actually is, in this process, right before make runs.
    """
    try:
        proc = subprocess.run(
            ["bash", "-c", "type -t make; command -v make"],
            capture_output=True, text=True, timeout=30, env=clean_env(),
        )
    except (OSError, subprocess.SubprocessError) as exc:
        g.fail(f"could not ask the shell what `make` is: {exc}")
        return
    lines = [l.strip() for l in proc.stdout.splitlines() if l.strip()]
    if len(lines) < 2:
        g.fail(
            "the shell reports no `make` at all.",
            f"`type -t make; command -v make` printed {proc.stdout!r} (exit {proc.returncode}).",
        )
        return
    kind, path = lines[0], lines[1]

    if kind != "file":
        g.fail(
            f"`make` is a {kind}, not a program on disk (`type -t make` says {kind!r}).",
            "A shell function or alias named `make` shadows the real program entirely, so every",
            "recipe in this repository can be replaced by `:` without touching a single file here.",
        )
        return
    real = os.path.realpath(path)
    if not (os.path.isfile(real) and os.access(real, os.X_OK)):
        g.fail(f"`make` resolves to {path!r} -> {real!r}, which is not an executable file.")
        return
    if os.path.dirname(real) not in APPROVED_MAKE_DIRS:
        g.fail(
            f"`make` resolves to {real!r}, which is not in {list(APPROVED_MAKE_DIRS)}.",
            "A `make` outside the system directories is a stub someone put earlier on PATH — the",
            "shape an earlier workflow step creates by writing a directory to $GITHUB_PATH.",
        )
        return
    if os.path.basename(real) not in ("make", "gmake"):
        g.fail(
            f"`make` resolves to {path!r}, whose real file is {real!r} — not a program named make.",
            "A symlink named `make` pointing at `true` or `echo` sits in an approved directory",
            "and still makes every recipe a no-op.",
        )
        return
    # Say ONLY what was checked. A real FILE named make in an approved directory
    # that forwards some invocations and exits 0 on others would pass this — it
    # needs an earlier step to write to the machine, which is the stated
    # review-only boundary, and this line must not claim to have ruled it out.
    g.ok(f"`make` resolves to {path} (type -t: file; real file {real}, named make, in an approved "
         f"system directory). This does not inspect the program's CONTENT: a forwarding stub planted "
         f"there by an earlier step is outside what this guard can see.")


# ------------------------------------------------------------------- main ---


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent,
                    help="directory holding the Makefile (default: the repository root)")
    ap.add_argument("--targets", default=",".join(GATE_TARGETS),
                    help="comma-separated gate targets (default: every target a required CI lane invokes)")
    ap.add_argument("--workflow", action="store_true",
                    help="STRICT environment mode: the invocation pinned in .github/pinned-steps.yml as "
                         "the workflow anchor. Chosen by the invocation, never by the environment.")
    args = ap.parse_args()

    root = args.root.resolve()
    targets = [t.strip() for t in args.targets.split(",") if t.strip()]

    g = Guard()
    print(f"make-integrity-guard: {root}")
    print(f"  gate targets: {', '.join(targets)}")

    if not (root / "Makefile").exists():
        g.fail(f"{root}/Makefile does not exist")
        print("make-integrity-guard: FAILED — make was NOT invoked", file=sys.stderr)
        return 1

    # EVERYTHING below, up to the gate, runs WITHOUT invoking make. The digest
    # pin comes first because it is the control: make evaluates the file while
    # it reads it, so the bytes must be the reviewed bytes before make sees them.
    pinned = check_makefile_pin(g, root)
    check_environment(g, args.workflow)
    check_make_resolves_to_a_real_program(g)
    if pinned is not None:
        # The static read set is known now, so every TEXT check — the
        # environment overrides, and the SHELL / .SHELLFLAGS / MAKEFLAGS /
        # GNUMAKEFLAGS / .ONESHELL assignments, recipe prefixes and suffixes,
        # duplicate and conditional gate targets — runs before make as well.
        # Fix round 1 (PR#10 VERIFY FINDING 4): these used to run after four
        # make processes, while the docstring said "before make is ever invoked".
        check_environment_overrides(g, root, pinned[0], args.workflow)

        # The named targets are what CI invokes; the CLOSURE is what actually
        # runs. `make ci` is one word in a workflow and ten lanes here, and it
        # is `test-race`'s recipe — not `ci`'s, which has none — that runs the
        # tests.
        closure = prerequisite_closure(root, pinned[0], targets)
        derived = [t for t in closure if t not in targets]
        print(f"  closure: {len(closure)} target(s); {len(derived)} reached through prerequisites"
              + (f" ({', '.join(derived)})" if derived else ""))
        check_text(g, root, pinned[0], closure, targets)

    # THE GATE. Any failure so far — an unpinned or changed makefile, a
    # MAKEFILES / BASH_ENV / MAKEFLAGS in the environment, a `make` that is not
    # the system's, a neutering assignment or recipe line in the pinned text —
    # and make is never started. Before sweep B5 a failed
    # environment check was reported and make was run anyway.
    if g.failed:
        print(f"make-integrity-guard: FAILED — make was NOT invoked ({MAKE_INVOCATIONS} make process(es) "
              f"started). A make-driven gate is only run on reviewed bytes in a clean environment.",
              file=sys.stderr)
        return 1
    pinned_files, pinned_digests = pinned

    # make's first invocation, on the pinned bytes only: would it remake any of
    # them? ONE `make -q` naming every pinned makefile. If it would, stop —
    # nothing else runs make.
    check_no_pinned_makefile_would_be_remade(g, root, pinned_files)
    if g.failed:
        recheck_pinned_bytes(g, root, pinned_digests)
        print(f"make-integrity-guard: FAILED — make was invoked only as `make -q` ({MAKE_INVOCATIONS} "
              f"process(es)), which runs no ordinary recipe.", file=sys.stderr)
        return 1
    PINNED_MAKEFILE_GOALS[:] = pinned_files

    # One resolver pass names every file make reads, including everything an
    # `include` pulls in. The text reading then covers all of them.
    variables, files = resolve_database(g, root, targets[0])
    if variables is None:
        recheck_pinned_bytes(g, root, pinned_digests)
        print("make-integrity-guard: FAILED", file=sys.stderr)
        return 1

    # Corroborate the static read set with what make itself says it read.
    made = [os.path.normpath(f) for f in files]
    if sorted(set(made)) != sorted(set(pinned_files)):
        g.fail(
            f"make reports MAKEFILE_LIST {made}, but the pinned bytes name {pinned_files}.",
            "The static reading of the reviewed bytes did not predict what make read, so make read a file",
            "nobody pinned. That is a defect in this guard's reading, and it fails closed.",
        )
    else:
        g.ok(f"make's own MAKEFILE_LIST {made} is exactly the pinned set")

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

    # And the bytes make was run on are STILL the pinned bytes.
    recheck_pinned_bytes(g, root, pinned_digests)

    if g.failed:
        print("make-integrity-guard: FAILED", file=sys.stderr)
        print("A make-driven gate that can be turned into a no-op is not a gate. Fix the Makefile.", file=sys.stderr)
        return 1
    print(f"make-integrity-guard: passed ({len(targets)} gate target(s); make ran {MAKE_INVOCATIONS} time(s), "
          f"only on the pinned bytes of {', '.join(pinned_files)})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
