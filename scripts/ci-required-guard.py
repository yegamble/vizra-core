#!/usr/bin/env python3
"""ci-required-guard — the guard on the gate.

`ci-required` reads .github/required-checks.txt from the checkout UNDER TEST, so
the pull request being gated can edit its own gate. This guard is the mechanical
control that stops it. CODEOWNERS is the backstop, and it is a real one — but
the ruleset that makes CODEOWNERS mandatory is not applied yet, so today this is
the only mechanical control.

It was a shell script grepping YAML with a regex, and that had two holes a
reviewer found:

  * `continue-on-error` was matched as the bare lowercase literal
    `continue-on-error: true`. GitHub Actions accepts other spellings of the
    same thing — quoted, capitalised, and an expression that evaluates to true —
    and any of them turns a failing job's conclusion into success, so the fan-in
    would record SUCCESS for a lane that failed. A guard that reports ok for
    something it cannot see is the false-positive CI that AGENTS.md names.

  * The "empty test selection" check grepped only the workflows, and no workflow
    contains `go test` at all — every selection lives in the Makefile. It
    printed a reassuring ok line for a condition it never tested.

So: parse the YAML, and read the Makefile the lanes actually run.

THE CHECKED SET (sweep B1). Checks 3, 4, 8, 8b and 10 run over
`set(FLOOR_LANES) | set(required)` — not over FLOOR_LANES alone. FLOOR_LANES is
the floor that cannot be REMOVED; it is not also the ceiling on what gets
checked. Before B1 a lane that was REQUIRED but not on the floor got exactly one
line of attention — "ok … resolves to a job" — while carrying
`continue-on-error: true` and an unanchored `make -i ci`
(docs/evidence/warroom/2026-09-21-vizra-core-pr6-hardening-a-VERIFY.md,
FINDING 6). Widening the gate by adding a manifest line looks virtuous and was
the likeliest way to introduce an unchecked lane.

Checks, all of which must pass:

  1. FLOOR        every lane in FLOOR_LANES is present and not commented out.
  2. RESOLVABLE   every manifest name maps to a real job.
  3. TRIGGERED    every checked lane's workflow runs on `pull_request`.
  4. NO OPT-OUT   `continue-on-error` is absent AT ALL — any value, any
                  spelling — from a checked lane's job and from all of its steps.
  5. SELECTION    the Makefile lanes select a non-empty set: test-race covers
                  ./... and every -run pattern is non-empty.
  6. RUNNER       every job runs on GitHub-hosted ubuntu-24.04.
  7. PINNED       every action is pinned to a 40-character commit SHA.
  8. ANCHOR       every checked-lane job that invokes `make` runs
                  scripts/make-integrity-guard.sh FIRST, as its own step,
                  unconditionally and without continue-on-error — and neither
                  the workflow nor the job overrides `defaults.run.shell`, which
                  is the Actions analogue of `SHELL := /usr/bin/true`.
  8b. ARGV        a checked lane's `make` step may not carry, ON THE WORKFLOW
                  LINE, a flag or variable override that no-ops the recipes, and
                  neither the step, the job nor the workflow may set a
                  MAKEFLAGS-family `env:`. See "CHECK 8b" below.
  9. DIRECT       a required lane runs the UNIT suite and the INTEGRATION suite
                  over `./...` WITHOUT make, so a no-opped Makefile cannot make
                  either silent.
  10. PROVENANCE  every checked-lane job that checks the repository out also
                  runs scripts/provenance.sh, unconditionally. A lane that
                  prints a SHA without saying which tree it stood in is the
                  shape of meta-PR3 FINDING 5.

Checks 8, 8b and 9 exist because every required lane here runs through `make`,
and a verifier measured that ONE line in a Makefile — `SHELL := /usr/bin/true`
or `MAKEFLAGS += -i` — makes every recipe exit 0 without running
(docs/evidence/warroom/2026-09-20-vizra-search-pr2-revendor-VERIFY.md, FINDING
8), and that ONE WORD on a workflow line did the same with both guards green
(PR#6 VERIFY, FINDING 1). No check written inside a Makefile can prevent
either; these say the out-of-make controls are present and armed.

CHECK 8b/8c — DEFAULT-DENY ON THE SHAPE, NOT A BLACKLIST OF SHELL
-----------------------------------------------------------------
The anchor (check 8) runs `make -pn` in its OWN process. It cannot see the argv
or the `env:` of a DIFFERENT workflow step.

Sweep B1 answered that by reading the step's `run:` as shell and refusing a
blacklist of flags, `VAR=value` overrides and `env:` names. A verifier then
found THIRTEEN spellings that left both guards green (PR#9 VERIFY, § 3b):
`make -j -i ci` and `make -l -i ci` (the `-i` eaten as `-j`'s optional
argument, which getopt only accepts attached), `export MAKEFLAGS=-i` on the
line above, a `$GITHUB_ENV`/`$GITHUB_PATH` write by an earlier step,
`M=make; $M -i ci`, `${MAKE:-make}`, a shell function named `make`, a PATH
shadow, backticks, a step-level `if:`, `working-directory:`, and
`shell: bash -c '{0} || true'`.

A blacklist over arbitrary shell CANNOT be exhaustive — the shell has unbounded
ways to name a command — so the control was inverted:

  8b  A step in a checked lane whose `run:` mentions `make` at all (the wide,
      fail-closed classifier below decides that, a token in a comment included)
      must be BYTE-EQUAL to a literal in .github/pinned-steps.yml after
      trimming one trailing newline, may carry no key but `name`/`run`/`id`,
      and must be IMMEDIATELY preceded by the anchor step — which must itself
      carry no key but those. No flags, no overrides, no chains, one invocation
      per step. Every one of the thirteen fails this by construction, because
      every one changes the bytes or adds a key.

  8c  The lane must actually RUN the invocations pinned-steps.yml records for
      it, byte-equal. Pinning a shape does not stop a step being DELETED or
      replaced by one that reaches make through an indirection no classifier
      can see. This is the positive half, and it does not care what replaced
      them.

Adjacency is what makes the anchor's environment assertions mean anything: a
`$GITHUB_ENV` or `$GITHUB_PATH` write applies to LATER steps, so with a step in
between, the anchor would inspect a clean environment and make would run in a
poisoned one.

`timeout-minutes` is refused on these steps too. Not because it is dangerous —
it can only make a step fail sooner — but because nothing here uses one, and an
unused key is surface for no benefit.

The tokeniser that used to be the control is KEPT, but it decides nothing: it
only adds a friendlier sentence to a red.

WHAT THESE CHECKS ARE, AND WHAT THEY ARE NOT
---------------------------------------------
They are DEFAULT-DENY on the shape and surroundings of make steps and direct
test steps in floor lanes. That is the whole claim. This list is deliberately
not called exhaustive: the previous one was, and thirteen things were missing
from it.

  * They do not constrain what any OTHER step does to the machine. A checked
    lane may contain arbitrary `run:` steps and `uses:` actions, and through
    them anything at all before the anchor runs — replacing the Go toolchain,
    rewriting the Makefile or the test files on disk, installing a different
    python3. The anchor observes what its own assertions cover and nothing
    else. REVIEW is the control for that, and CODEOWNERS is ADVISORY until the
    owner's ruleset exists (it currently returns 403 on their plan).
  * A wrapper script or composite action that calls make is not read: it
    carries no `make` token on the workflow line. Check 8c bounds the damage —
    the required invocations must still be present — but a lane may run one in
    addition.
  * A reusable workflow (`jobs.<id>.uses:`) has no `steps:` here at all, so
    checks 4, 8, 8b, 8c, 9 and 10 have nothing to read.
  * The pins, the floors and the skip allowlist are committed files. Widening
    .github/pinned-steps.yml is a visible, reviewed diff in a file whose only
    purpose is to be a gate — the same posture as FLOOR_LANES. It is not
    prevented; it is made visible.
  * A `run:` this guard cannot tokenise is a FAILURE, not a skip.
  * This file, the workflows, go-test-report.py and test-floors.json are all
    checked out from the pull request under test and can be edited in it.

Usage:
    ci-required-guard.py [--workflows DIR] [--manifest FILE] [--makefile FILE]

scripts/scripts_test.go drives it against scripts/testdata/guard/, which holds
one crafted workflow per evasion, so the guard has negative cases of its own.
"""

from __future__ import annotations

import argparse
import re
import shlex
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the CI image always has it
    print("ci-required-guard: PyYAML is required. This lane is BLOCKED, not passed.", file=sys.stderr)
    raise SystemExit(2)

# ---------------------------------------------------------------------------
# THE FLOOR.
#
# These lanes are not optional and not removable by the pull request they gate.
# Each is here because dropping it would let a whole class of defect merge:
#
#   append-only   the merge-base diff that makes merged migrations actually
#                 frozen. `migration-manifest.sh check` alone is only
#                 self-consistency: regenerating the manifest turns it green.
#   build-test    the unit suite, the both-direction OpenAPI contract check, the
#                 sqlc drift check, migrate-lint and the manifest self-check.
#   cache-matrix  the permanent two-image Valkey/Redis-7.2 matrix. ADR-001 Q-004
#                 makes it permanent precisely because no upstream guarantees
#                 the compatibility it asserts.
#   fixtures      the deterministic fixture corpus (VZ-FOUND-007, ADR-009).
#                 Every M1 media assertion — upload validation, EXIF and
#                 orientation handling, GPS stripping, derivative budgets,
#                 decoder resource bounds — is compared against these exact
#                 bytes. If the corpus stops being reproducible, every one of
#                 those results becomes unfalsifiable while still looking
#                 green, which is the worst failure mode this repository has.
#   govulncheck   a known-vulnerable dependency set.
#   docker-build  the release image, its digest-pinned base and its loader list.
#
# Adding a lane to the floor is a deliberate widening. REMOVING one requires
# editing this file, which CODEOWNERS also protects, so the removal is visible
# in review instead of hiding in a one-line manifest diff.
# ---------------------------------------------------------------------------
FLOOR_LANES = ["append-only", "build-test", "cache-matrix", "fixtures", "govulncheck", "docker-build"]

RUNNER = "ubuntu-24.04"
SHA_PIN = re.compile(r"^[^@\s]+@[0-9a-f]{40}(\s|$)")


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


def load_workflows(d: Path) -> dict[Path, dict]:
    out: dict[Path, dict] = {}
    for p in sorted(list(d.glob("*.yml")) + list(d.glob("*.yaml"))):
        try:
            doc = yaml.safe_load(p.read_text())
        except yaml.YAMLError as e:
            print(f"  FAIL  {p} is not valid YAML: {e}", file=sys.stderr)
            raise SystemExit(1)
        if isinstance(doc, dict):
            out[p] = doc
    return out


def job_display_name(job_key: str, job: dict) -> str:
    """GitHub reports a job's `name:` when set, otherwise its key."""
    if isinstance(job, dict) and isinstance(job.get("name"), str):
        return job["name"]
    return job_key


def triggers(doc: dict) -> set[str]:
    # PyYAML parses the bare key `on:` as the boolean True (YAML 1.1), which is
    # how a naive reader concludes a workflow has no triggers at all.
    raw = doc.get("on", doc.get(True))
    if isinstance(raw, str):
        return {raw}
    if isinstance(raw, list):
        return {str(x) for x in raw}
    if isinstance(raw, dict):
        return {str(k) for k in raw}
    return set()


def continue_on_error_sites(job: dict) -> list[str]:
    """Every place `continue-on-error` appears, whatever its value or spelling.

    PRESENT-AT-ALL is the rule. A required lane has no legitimate use for it,
    and testing the VALUE is what let `true`-by-expression through: the guard
    cannot evaluate `${{ ... }}` and must not pretend to.
    """
    sites: list[str] = []
    for key in job:
        if str(key).strip().lower().replace("_", "-") == "continue-on-error":
            sites.append(f"job-level ({key}: {job[key]!r})")
    for i, step in enumerate(job.get("steps") or []):
        if not isinstance(step, dict):
            continue
        for key in step:
            if str(key).strip().lower().replace("_", "-") == "continue-on-error":
                label = step.get("name") or step.get("uses") or f"step {i}"
                sites.append(f"step {label!r} ({key}: {step[key]!r})")
    return sites


MAKE_INTEGRITY_GUARD = "make-integrity-guard"
# `make` as a command, not as a word inside one (`cmake`, `makefile`, `make-up`).
#
# The leading class includes the QUOTE characters from sweep B1: without them
# `run: bash -c "make -i ci"` was not classified as a make step at all, so it
# needed no anchor and its argv was never read. This heuristic is deliberately
# WIDER than the tokeniser — a bare `make` token in a comment counts — because
# its job is to demand the anchor, and over-demanding fails closed.
MAKE_INVOCATION = re.compile(r"""(?:^|[\s;&|("'`])make(?=\s|$)""", re.M)


def step_runs_make(step: dict) -> bool:
    run = step.get("run")
    return isinstance(run, str) and bool(MAKE_INVOCATION.search(run))


def step_is_the_anchor(step: dict) -> bool:
    run = step.get("run")
    return isinstance(run, str) and MAKE_INTEGRITY_GUARD in run and not MAKE_INVOCATION.search(run)


# ---------------------------------------------------------------------------
# CHECK 8b: the make step's own command line and environment.
#
# The anchor cannot see either — it runs `make -pn` in its own process and reads
# its own environment. Everything below is about the WORKFLOW LINE.
# ---------------------------------------------------------------------------

# Short flags that stop the recipes being the gate. The reason is printed, so a
# red names what the flag does rather than only that it is on a list.
DANGEROUS_SHORT = {
    "i": "-i/--ignore-errors: every recipe's failure is ignored and make exits 0",
    "k": "-k/--keep-going: make carries on past a failed target instead of stopping at it",
    "t": "-t/--touch: targets are TOUCHED, the recipes never run",
    "q": "-q/--question: no recipe runs at all; make only reports whether a target is up to date",
    "n": "-n/--dry-run: recipes are printed, not executed",
    "e": "-e/--environment-overrides: the environment beats the makefile's own assignments",
    "f": "-f/--file: make reads a DIFFERENT makefile from the one make-integrity-guard read",
    "C": "-C/--directory: make changes directory first, so it reads a different makefile",
    "o": "-o/--old-file: the named file is treated as old, so what depends on it is never remade",
    "W": "-W/--what-if: the named file is treated as new, which rewrites what make decides to do",
}

# GNU make short options that consume an argument. In a cluster the letter takes
# the rest of the token (`-fMakefile`) or the next token (`-f Makefile`).
SHORT_TAKES_ARG = set("CfIjloWE")

DANGEROUS_LONG = {
    "--ignore-errors": DANGEROUS_SHORT["i"],
    "--keep-going": DANGEROUS_SHORT["k"],
    "--touch": DANGEROUS_SHORT["t"],
    "--question": DANGEROUS_SHORT["q"],
    "--dry-run": DANGEROUS_SHORT["n"],
    "--just-print": DANGEROUS_SHORT["n"],
    "--recon": DANGEROUS_SHORT["n"],
    "--environment-overrides": DANGEROUS_SHORT["e"],
    "--file": DANGEROUS_SHORT["f"],
    "--makefile": DANGEROUS_SHORT["f"],
    "--directory": DANGEROUS_SHORT["C"],
    "--old-file": DANGEROUS_SHORT["o"],
    "--assume-old": DANGEROUS_SHORT["o"],
    "--what-if": DANGEROUS_SHORT["W"],
    "--new-file": DANGEROUS_SHORT["W"],
    "--assume-new": DANGEROUS_SHORT["W"],
}

# Environment names that reach INTO make. MAKEFLAGS/GNUMAKEFLAGS/MFLAGS are read
# by make as if they had been typed on the command line; MAKEFILES makes it read
# extra makefiles nothing scanned. SHELL is belt-and-braces — see the docstring.
MAKE_ENV_NAMES = {"MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS", "MAKEFILES", "SHELL"}

SHELL_SEPARATORS = {";", "&&", "||", "|", "(", ")", "&", "|&"}

ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_.]*=")
MAKE_COMMAND = re.compile(r"^(?:.*/)?g?make$")


class TokeniseError(Exception):
    """A `run:` this guard could not read as shell. Fail closed, never skip."""


def _commands(run: str) -> list[list[str]]:
    """Split a `run:` block into command token lists.

    Line continuations are joined, each line is tokenised with quotes honoured
    and `#` comments stripped, and the tokens are split on shell separators.
    """
    joined = re.sub(r"\\\n\s*", " ", run)
    out: list[list[str]] = []
    for line in joined.splitlines():
        if not line.strip():
            continue
        lex = shlex.shlex(line, posix=True, punctuation_chars=True)
        lex.whitespace_split = True
        lex.commenters = "#"
        try:
            tokens = list(lex)
        except ValueError as exc:  # unbalanced quote, unterminated string
            raise TokeniseError(f"{exc} (line: {line.strip()!r})") from exc
        current: list[str] = []
        for tok in tokens:
            if tok in SHELL_SEPARATORS:
                if current:
                    out.append(current)
                current = []
                continue
            current.append(tok)
        if current:
            out.append(current)
    return out


def _scan_make_argv(argv: list[str]) -> list[str]:
    """Problems in the arguments make itself would receive. `argv` excludes `make`."""
    problems: list[str] = []
    i = 0
    end_of_options = False
    while i < len(argv):
        tok = argv[i]
        i += 1
        if end_of_options:
            if ASSIGNMENT.match(tok):
                problems.append(f"the variable override {tok!r} (after `--`)")
            continue
        if tok == "--":
            end_of_options = True
            continue
        if tok.startswith("--"):
            name = tok.split("=", 1)[0]
            for full, why in DANGEROUS_LONG.items():
                # GNU make accepts any unambiguous abbreviation of a long
                # option, so a PREFIX match is the honest test: `--ign` is
                # `--ignore-errors`.
                if len(name) > 2 and full.startswith(name):
                    problems.append(f"{tok!r} — {why}")
                    break
            continue
        if tok.startswith("-") and len(tok) > 1:
            for pos, ch in enumerate(tok[1:], start=1):
                if ch in DANGEROUS_SHORT:
                    problems.append(f"{tok!r} carries -{ch} — {DANGEROUS_SHORT[ch]}")
                if ch in SHORT_TAKES_ARG:
                    # This letter eats the rest of the token, or the next one.
                    if pos == len(tok) - 1 and i < len(argv):
                        i += 1
                    break
            continue
        if ASSIGNMENT.match(tok):
            problems.append(
                f"the variable override {tok!r} — make applies a command-line override "
                f"over the makefile's own value, wherever it sits relative to the target"
            )
    return problems


def make_command_line_problems(run: str, depth: int = 0) -> tuple[list[str], int]:
    """Every refusal in a `run:` block, plus how many make COMMANDS were found.

    A count of 0 while MAKE_INVOCATION matched means the `make` token is in a
    comment or inside a word — reported, never silently passed.
    """
    problems: list[str] = []
    found = 0
    for argv in _commands(run):
        make_at = next((n for n, t in enumerate(argv) if MAKE_COMMAND.match(t)), None)
        if make_at is None:
            # `bash -c "make -i ci"` — the make command is inside a quoted word.
            if depth < 3:
                for tok in argv:
                    if (" " in tok or "\n" in tok) and MAKE_INVOCATION.search(tok):
                        sub, subfound = make_command_line_problems(tok, depth + 1)
                        problems.extend(f"inside the quoted script {tok!r}: {p}" for p in sub)
                        found += subfound
            continue
        found += 1
        for tok in argv[:make_at]:
            if ASSIGNMENT.match(tok):
                name = tok.split("=", 1)[0]
                if name.upper() in MAKE_ENV_NAMES:
                    problems.append(
                        f"the environment prefix {tok!r} before `make` — make reads "
                        f"{name} as if it had been typed on the command line"
                    )
        problems.extend(_scan_make_argv(argv[make_at + 1:]))
    return problems, found


def env_problems(scope: str, env) -> list[str]:
    if not isinstance(env, dict):
        return []
    out = []
    for key, value in env.items():
        if str(key).strip().upper() in MAKE_ENV_NAMES:
            out.append(f"{scope} env sets {key}: {value!r}")
    return out


# --- the pinned-shape control (sweep B1 round 2) ----------------------------

STEP_KEYS_ALLOWED = {"name", "run", "id"}
# Environment names that reach into make, or into the shell that runs it.
# PATH is here because a stub `make` earlier on PATH is a complete bypass;
# BASH_ENV/ENV because a non-interactive shell sources them and can define a
# `make` function before the recipe is ever reached.
DANGEROUS_ENV_NAMES = {
    "MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS", "MAKEFILES",
    "SHELL", "PATH", "BASH_ENV", "ENV",
}


def trim_one_newline(text: str) -> str:
    return text[:-1] if text.endswith("\n") else text


def load_pins(path: Path) -> tuple[list[str], list[str]]:
    """The committed allowlist of `run:` bodies. Missing or empty is a FAILURE."""
    if not path.exists():
        raise SystemExit(
            f"ci-required-guard: {path} is missing. It is the allowlist that makes a floor "
            f"lane's make step and direct test step default-deny; without it there is no gate."
        )
    doc = yaml.safe_load(path.read_text()) or {}
    make_steps = [trim_one_newline(x) for x in (doc.get("make_steps") or [])]
    direct = [trim_one_newline(x) for x in (doc.get("direct_test_steps") or [])]
    required_inv = {
        str(k): [trim_one_newline(x) for x in (v or [])]
        for k, v in (doc.get("required_invocations") or {}).items()
    }
    unknown = {b for bodies in required_inv.values() for b in bodies} - set(make_steps)
    if unknown:
        raise SystemExit(
            f"ci-required-guard: {path} requires invocation(s) {sorted(unknown)} that are not in "
            f"make_steps, so they could never satisfy check 8b."
        )
    if not make_steps or not direct or not required_inv:
        raise SystemExit(
            f"ci-required-guard: {path} lists no make_steps, direct_test_steps or "
            f"required_invocations. "
            f"An empty allowlist would refuse everything or assert nothing."
        )
    return make_steps, direct, required_inv


def extra_keys(step: dict) -> list[str]:
    return sorted(k for k in step if str(k) not in STEP_KEYS_ALLOWED)


def env_problems(scope: str, env) -> list[str]:
    if not isinstance(env, dict):
        return []
    return [
        f"{scope} env sets {key}: {value!r}"
        for key, value in env.items()
        if str(key).strip().upper() in DANGEROUS_ENV_NAMES
    ]


def check_pinned_make_steps(g: Guard, lane: str, path: Path, doc, job: dict, pins: list[str]) -> None:
    """Check 8b: a make step's SHAPE, default-deny.

    This does not parse flags. A step in a checked lane whose `run:` mentions
    `make` at all — the wide, fail-closed ANCHOR classifier decides that, a
    token in a comment included — must be BYTE-EQUAL to one of the literals in
    .github/pinned-steps.yml, and may carry no keys but name/run/id.

    The tokeniser that used to be the control is kept ONLY to add a friendlier
    sentence to the red; it decides nothing.
    """
    steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
    make_steps = [(i, s) for i, s in enumerate(steps) if step_runs_make(s) and not step_is_the_anchor(s)]
    if not make_steps:
        return

    failed_here = False
    for i, step in make_steps:
        label = step.get("name") or f"step {i}"
        body = trim_one_newline(step.get("run") or "")
        if body not in pins:
            detail = []
            try:
                problems, _ = make_command_line_problems(step.get("run") or "")
                detail = [f"(for what it is worth: {p})" for p in problems[:2]]
            except TokeniseError:
                detail = ["(and its `run:` cannot even be tokenised as shell)"]
            g.fail(
                f"checked lane {lane!r} ({path.name}) step {label!r} mentions `make` but its `run:` "
                f"is not byte-equal to any entry in .github/pinned-steps.yml.",
                f"got: {body!r}",
                *detail,
                "A floor lane's make step is DEFAULT-DENY on its whole body: no flags, no variable",
                "overrides, no chains, no indirection, one invocation per step. Thirteen spellings",
                "defeated the flag-parsing version of this check, so the bytes are the control now.",
                "If this invocation is legitimate, add it to .github/pinned-steps.yml in a reviewed diff.",
            )
            failed_here = True
        extra = extra_keys(step)
        if extra:
            g.fail(
                f"checked lane {lane!r} ({path.name}) make step {label!r} carries {extra}.",
                "A make step may carry only `name`, `run` and `id`. `if:` skips it while the job stays",
                "green; `env:` reaches make; `shell:` can discard its exit status; `working-directory:`",
                "is `-C` by another name; `continue-on-error:` discards the failure. `timeout-minutes`",
                "is refused too — not because it is dangerous (it can only make a step fail sooner) but",
                "because nothing here uses it, and an unused key is surface for no benefit.",
            )
            failed_here = True

        # ADJACENCY. The anchor must be the step IMMEDIATELY before this one, so
        # that nothing can run in between and write MAKEFLAGS (or a stub `make`
        # onto PATH) through $GITHUB_ENV / $GITHUB_PATH — those persist into
        # LATER steps, so a non-adjacent anchor would observe a clean
        # environment and make would not.
        if i and step_is_the_anchor(steps[i - 1]):
            anchor_extra = extra_keys(steps[i - 1])
            if anchor_extra:
                g.fail(
                    f"checked lane {lane!r} ({path.name}) the anchor step before {label!r} carries "
                    f"{anchor_extra}.",
                    "The anchor may carry only `name`, `run` and `id`. An `if:` skips it, a",
                    "`continue-on-error:` discards its verdict, an `env:` or `shell:` changes the very",
                    "environment it exists to inspect — and it is the anchor ADJACENT to this make",
                    "step that matters, not merely the first one in the job.",
                )
                failed_here = True
        if i == 0 or not step_is_the_anchor(steps[i - 1]):
            before = (steps[i - 1].get("name") if i else None) or (f"step {i - 1}" if i else "(nothing)")
            g.fail(
                f"checked lane {lane!r} ({path.name}) make step {label!r} is not IMMEDIATELY preceded "
                f"by the {MAKE_INTEGRITY_GUARD} step; the step before it is {before!r}.",
                "Any step between the anchor and make can write MAKEFLAGS to $GITHUB_ENV or a stub",
                "`make` to $GITHUB_PATH, which GitHub applies to LATER steps only — so the anchor",
                "would check a clean environment and make would run in a poisoned one.",
            )
            failed_here = True

    for scope, holder in (("workflow-level", doc or {}), ("job-level", job)):
        for problem in env_problems(scope, (holder or {}).get("env")):
            g.fail(
                f"checked lane {lane!r} ({path.name}) {problem}, and this job invokes `make`.",
                "It reaches every step, the anchor and the make steps included.",
            )
            failed_here = True

    if not failed_here:
        g.ok(
            f"checked lane {lane!r}: {len(make_steps)} make step(s) are byte-equal to a pinned body, "
            f"carry no key but name/run/id, and each is immediately preceded by the anchor"
        )


def check_required_invocations(g: Guard, lane: str, path: Path, job_key: str, job: dict,
                              required_inv: dict) -> None:
    """Check 8c: the lane actually RUNS its gate invocations.

    Pinning the shape of a make step stops one being turned into something else.
    It does not stop one being DELETED, or replaced by a step that reaches make
    through an indirection no classifier can see:

        M=make ; $M -i ci        ${MAKE:-make} -i ci

    Neither carries a `make` token, so neither is classified as a make step and
    neither is pinned — but both leave the lane with no `make ci` step at all.
    This is the positive assertion: what MUST be there, byte-equal. It closes
    the indirection class for the gate invocations without parsing shell,
    because it does not care what replaced them.
    """
    wanted = required_inv.get(job_key)
    if not wanted:
        return
    bodies = {trim_one_newline(s.get("run") or "") for s in (job.get("steps") or []) if isinstance(s, dict)}
    missing = [w for w in wanted if w not in bodies]
    if missing:
        g.fail(
            f"checked lane {lane!r} ({path.name}) does not run required invocation(s) {missing}.",
            ".github/pinned-steps.yml lists these as invocations this job MUST contain, byte-equal.",
            "Deleting one, or replacing it with a step that reaches make through a variable, a",
            "function, an alias or a PATH edit, leaves the gate uninvoked — and no amount of",
            "reading the replacement's shell could tell you that.",
        )
    else:
        g.ok(f"checked lane {lane!r} runs all {len(wanted)} required invocation(s) for {job_key!r}")


def check_job_surroundings(g: Guard, lane: str, path: Path, doc, job: dict) -> None:
    """Checks that apply to a checked lane whether or not it invokes make."""
    for scope, holder in (("workflow", doc or {}), ("job", job)):
        defaults_run = ((holder or {}).get("defaults") or {}).get("run")
        if defaults_run is not None:
            g.fail(
                f"checked lane {lane!r} ({path.name}) sets a {scope}-level defaults.run: {defaults_run!r}.",
                "It rewrites how EVERY `run:` step is executed — shell, working directory — so it",
                "no-ops the anchor and every pinned step at once. Nothing here needs one.",
            )
    if job.get("container") is not None:
        g.fail(
            f"checked lane {lane!r} ({path.name}) runs in a job-level `container:` "
            f"({job['container']!r}).",
            "The whole lane would then execute inside an image this repository does not pin or",
            "assert, with its own make, go and shell. Nothing here needs one.",
        )
    for name, svc in (job.get("services") or {}).items():
        image = svc.get("image") if isinstance(svc, dict) else None
        if isinstance(image, str) and "@sha256:" not in image:
            g.fail(
                f"checked lane {lane!r} ({path.name}) service {name!r} image {image!r} is not "
                f"digest-pinned.",
                "A floating tag lets the database or cache this lane tests against change underneath it.",
            )


PROVENANCE = "provenance.sh"


def check_provenance(g: Guard, lane: str, path: Path, job: dict) -> None:
    """Check 10: a lane that checks the repository out says which tree it stood in.

    meta-PR3 FINDING 5 was a step whose whole job was to record provenance
    printing a SHA that was not the tree it described. `append-only` was the
    last required lane with that shape (PR#6 VERIFY, FINDING 4): it checks out
    with full history, computes a merge base and echoes it, with no statement of
    which tree it is standing in.
    """
    steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
    checkouts = [s for s in steps if "actions/checkout" in str(s.get("uses") or "")]
    if not checkouts:
        g.ok(f"checked lane {lane!r} performs no checkout, so it needs no provenance step")
        return

    prov = next((s for s in steps if PROVENANCE in str(s.get("run") or "")), None)
    if prov is None:
        g.fail(
            f"checked lane {lane!r} ({path.name}) checks the repository out and has NO "
            f"scripts/{PROVENANCE} step.",
            "On a pull_request the checked-out tree is refs/pull/N/merge — neither the head SHA nor",
            "the base SHA — and it moves whenever the base branch moves. A lane that prints a SHA",
            "without saying which tree it stood in is meta-PR3 FINDING 5.",
        )
        return
    if "if" in prov:
        g.fail(
            f"checked lane {lane!r} ({path.name}) makes the provenance step CONDITIONAL "
            f"(if: {prov['if']!r}).",
            "A skipped step records nothing, and this guard cannot evaluate an Actions expression.",
        )
        return
    for key in prov:
        if str(key).strip().lower().replace("_", "-") == "continue-on-error":
            g.fail(
                f"checked lane {lane!r} ({path.name}) marks the provenance step "
                f"{key}: {prov[key]!r}.",
                "Its cross-check — HEAD^2 == the PR head SHA — would then be discarded.",
            )
            return
    g.ok(f"checked lane {lane!r} runs scripts/{PROVENANCE} after checking out, unconditionally")


def check_anchor(g: Guard, lane: str, path: Path, doc, job: dict) -> None:
    """Check 8: the out-of-make guard runs before make, and can actually fail.

    Four ways to remove the control without deleting the guard program, each of
    which this refuses by name: deleting the step, giving it an `if:`, marking it
    continue-on-error, or moving it after the first `make`.
    """
    steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
    make_at = next((i for i, s in enumerate(steps) if step_runs_make(s)), None)
    if make_at is None:
        g.ok(f"checked lane {lane!r} invokes no `make`, so it needs no make-integrity anchor")
        return

    anchor_at = next((i for i, s in enumerate(steps) if step_is_the_anchor(s)), None)
    label = steps[make_at].get("name") or f"step {make_at}"
    if anchor_at is None:
        g.fail(
            f"checked lane {lane!r} ({path.name}) invokes `make` (step {label!r}) with NO "
            f"{MAKE_INTEGRITY_GUARD} step before it.",
            "One line in the Makefile — `SHELL := /usr/bin/true` or `MAKEFLAGS += -i` — makes every",
            "recipe exit 0 without running, and no check inside a Makefile can stop that, because the",
            "neutering disarms that check too. The out-of-make step is the control; it must be present.",
        )
        return
    if anchor_at > make_at:
        g.fail(
            f"checked lane {lane!r} ({path.name}) runs the {MAKE_INTEGRITY_GUARD} step at position "
            f"{anchor_at}, AFTER `make` at position {make_at} ({label!r}).",
            "A neutered `make` step would already have reported success by then.",
        )
        return

    anchor = steps[anchor_at]
    if "if" in anchor:
        g.fail(
            f"checked lane {lane!r} ({path.name}) makes the {MAKE_INTEGRITY_GUARD} step CONDITIONAL "
            f"(if: {anchor['if']!r}).",
            "A skipped step is not a control, and this guard cannot evaluate an Actions expression.",
        )
    for key in anchor:
        if str(key).strip().lower().replace("_", "-") == "continue-on-error":
            g.fail(
                f"checked lane {lane!r} ({path.name}) marks the {MAKE_INTEGRITY_GUARD} step "
                f"{key}: {anchor[key]!r}.",
                "Its failure would be discarded. Present at all is the rule, whatever the value.",
            )

    # The Actions analogue of `SHELL := /usr/bin/true`: `defaults.run.shell`
    # replaces the shell every `run:` step uses, the anchor's included.
    for scope, holder in (("workflow", doc or {}), ("job", job)):
        shell = ((holder.get("defaults") or {}).get("run") or {}).get("shell")
        if shell is not None:
            g.fail(
                f"checked lane {lane!r} ({path.name}) sets a {scope}-level defaults.run.shell: {shell!r}.",
                "It replaces the shell EVERY `run:` step uses, so it no-ops the anchor and every make",
                "step at once — the Actions analogue of `SHELL := /usr/bin/true`.",
            )

    if not g.failed:
        g.ok(f"checked lane {lane!r} runs the {MAKE_INTEGRITY_GUARD} anchor (step {anchor_at}) before "
             f"`make` (step {make_at}), unconditionally and without continue-on-error")


def expand_needs(lane: str, path: Path, doc: dict, job: dict, jobs_by_key: dict):
    """The named job plus every job it transitively `needs`, within its workflow.

    Returns [(label, (path, doc, job)), ...] with the named job first.
    """
    out = [(lane, (path, doc, job))]
    seen = set()
    queue = list(_needs_of(job))
    while queue:
        key = queue.pop(0)
        if key in seen:
            continue
        seen.add(key)
        entry = (jobs_by_key.get(path) or {}).get(key)
        if entry is None:
            continue
        out.append((f"{lane} -> needs:{key}", entry))
        queue.extend(_needs_of(entry[2]))
    return out


def _needs_of(job: dict) -> list[str]:
    raw = job.get("needs")
    if isinstance(raw, str):
        return [raw]
    if isinstance(raw, list):
        return [str(x) for x in raw]
    return []


INTEGRATION_TAG = "-tags=integration"


def check_direct_test_lane(g: Guard, required: list[str], jobs: dict, pins: list[str]) -> None:
    """Check 9: a required lane runs BOTH suites without make, from a PINNED body.

    The anchor refuses a neutered Makefile BY NAME. This is the separate control
    for what the anchor cannot cover: whatever make did, a real failing test must
    still fail a required lane.

    Why the body is pinned rather than pattern-matched. At main the step was a
    bare `go test -race -count=1 ./...`, so a failing test failed it directly.
    Sweep B1 replaced that with a script that CAPTURES `go test`'s status into
    `$rc` in order to hand it to the report — and so deleting the single
    `go-test-report.py` line left a planted `t.Fatal` exiting 0 while this check
    still printed `ok` (PR#9 VERIFY, FINDING 6 — a regression against main). A
    substring test cannot see that; byte-equality can. The pinned bodies end
    `exit "$rc"`, so `go test`'s own failure fails the step even if the report
    line were gone, and `|| exit 1` on the report makes its verdict fail the step
    too.
    """
    unit_pins = [p for p in pins if INTEGRATION_TAG not in p]
    int_pins = [p for p in pins if INTEGRATION_TAG in p]
    found: dict[str, str] = {}

    for name in required:
        entry = jobs.get(name)
        if entry is None:
            continue
        path, _, job = entry
        steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
        for i, step in enumerate(steps):
            run = step.get("run")
            if not isinstance(run, str):
                continue
            body = trim_one_newline(run)
            if body in pins:
                extra = extra_keys(step)
                if extra:
                    g.fail(
                        f"required lane {name!r} ({path.name}) direct test step "
                        f"{(step.get('name') or f'step {i}')!r} carries {extra}.",
                        "A direct test step may carry only `name`, `run` and `id`, for the same reasons",
                        "a make step may: `if:` skips it, `shell:` can discard its exit status.",
                    )
                    continue
                kind = "integration" if INTEGRATION_TAG in body else "unit"
                found.setdefault(kind, f"{name!r} step {(step.get('name') or i)!r}")
                continue
            # A step that LOOKS like a direct suite invocation but is not pinned.
            # Named separately so an edited body reds with the real reason rather
            # than only through the "no required lane runs …" message below.
            if "go test" in run and "./..." in run and not MAKE_INVOCATION.search(run):
                g.fail(
                    f"required lane {name!r} ({path.name}) step "
                    f"{(step.get('name') or f'step {i}')!r} invokes `go test` over `./...` but its "
                    f"`run:` is not byte-equal to any entry in .github/pinned-steps.yml.",
                    f"got: {body!r}",
                    "The direct test steps are DEFAULT-DENY on their whole body: the exit handling",
                    "(`|| exit 1` on the report, `exit \"$rc\"` last) is what makes a failing test fail",
                    "this lane, and a substring check cannot see it being removed.",
                )

    for kind, kind_pins in (("unit", unit_pins), ("integration", int_pins)):
        if not kind_pins:
            g.fail(f".github/pinned-steps.yml has no {kind} direct-test body; nothing can satisfy check 9.")
        elif kind in found:
            g.ok(f"the {kind} suite runs directly, without make, from a pinned body in lane {found[kind]}")
        else:
            g.fail(
                f"no required lane runs the {kind.upper()} suite directly from a pinned body; every "
                f"{kind} test invocation goes through `make` or has been edited.",
                "A Makefile turned into a no-op would then make that suite SILENT rather than red, and",
                "`ci-required` would be green with nothing tested.",
            )


def check_makefile_selection(g: Guard, makefile: Path) -> None:
    """Check 5: the lanes actually select something.

    The previous check grepped the workflows for `go test`, and no workflow
    contains that string — every selection lives here. It printed ok for a file
    class it never read.
    """
    if not makefile.exists():
        g.fail(f"{makefile} is missing; the test selection cannot be checked")
        return
    text = makefile.read_text()

    # test-race must cover the whole module. PKGS is the indirection that makes
    # narrowing it a one-word edit.
    pkgs = re.search(r"^PKGS\s*:?=\s*(.+)$", text, re.M)
    if not pkgs:
        g.fail("the Makefile defines no PKGS; the test-race lane's selection is unknown")
    elif pkgs.group(1).strip() != "./...":
        g.fail(
            f"PKGS is {pkgs.group(1).strip()!r}, not './...'",
            "The test-race lane would run a subset of the module while every gate stayed green.",
        )
    else:
        g.ok("the test-race lane selects the whole module (PKGS = ./...)")

    race = re.search(r"^test-race:.*?\n((?:\t.*\n|\s*\n)+)", text, re.M)
    if not race or "$(PKGS)" not in race.group(1):
        g.fail("the test-race target does not use $(PKGS)", "Its selection is not the one checked above.")
    else:
        g.ok("the test-race target uses $(PKGS)")

    # Every -run pattern must be non-empty. An empty or unmatched pattern makes
    # `go test` report success while running nothing.
    runs = re.findall(r"-run\s+'([^']*)'", text)
    if not runs:
        g.fail("no -run selections found in the Makefile", "Either the lanes changed shape or this check is stale.")
        return
    empty = [r for r in runs if not r.strip()]
    if empty:
        g.fail(f"{len(empty)} -run pattern(s) are empty", "`go test -run ''` matches nothing and still exits 0.")
    else:
        g.ok(f"all {len(runs)} -run selections are non-empty")


def main() -> int:
    ap = argparse.ArgumentParser()
    root = Path(__file__).resolve().parent.parent
    ap.add_argument("--workflows", type=Path, default=root / ".github" / "workflows")
    ap.add_argument("--manifest", type=Path, default=root / ".github" / "required-checks.txt")
    ap.add_argument("--makefile", type=Path, default=root / "Makefile")
    ap.add_argument("--pins", type=Path, default=root / ".github" / "pinned-steps.yml")
    ap.add_argument("--skip-makefile", action="store_true", help="for fixture runs that ship no Makefile")
    args = ap.parse_args()

    g = Guard()
    print("ci-required-guard:")

    if not args.manifest.exists():
        g.fail(f"{args.manifest} is missing; ci-required has no gate to enforce")
        return 1

    required: list[str] = []
    commented: list[str] = []
    for line in args.manifest.read_text().splitlines():
        stripped = line.strip()
        if not stripped:
            continue
        if stripped.startswith("#"):
            commented.append(stripped.lstrip("#").strip())
            continue
        required.append(stripped)

    if not required:
        g.fail(f"{args.manifest} lists no checks; ci-required would pass with nothing verified")
        return 1

    make_pins, direct_pins, required_inv = load_pins(args.pins)
    g.ok(f"pinned-steps.yml: {len(make_pins)} make body(ies), {len(direct_pins)} direct-test "
         f"body(ies), required invocations for {sorted(required_inv)}")

    workflows = load_workflows(args.workflows)

    # name -> (path, doc, job)
    jobs: dict[str, tuple[Path, dict, dict]] = {}
    # workflow path -> {job KEY -> (path, doc, job)}. `needs:` names job KEYS,
    # not display names, and is scoped to one workflow file.
    jobs_by_key: dict[Path, dict[str, tuple[Path, dict, dict]]] = {}
    for path, doc in workflows.items():
        jobs_by_key[path] = {}
        for key, job in (doc.get("jobs") or {}).items():
            if isinstance(job, dict):
                jobs[job_display_name(key, job)] = (path, doc, job)
                jobs_by_key[path][key] = (path, doc, job)

    # --- 1. floor ----------------------------------------------------------
    for lane in FLOOR_LANES:
        if lane in required:
            continue
        if any(lane in c for c in commented):
            g.fail(
                f"required lane {lane!r} is COMMENTED OUT in {args.manifest.name}.",
                "It is a floor lane: it cannot be made optional by the pull request it gates.",
            )
        else:
            g.fail(
                f"required lane {lane!r} is MISSING from {args.manifest.name}.",
                "It is a floor lane (scripts/ci-required-guard.py, FLOOR_LANES).",
                "Deleting a manifest line does not shrink the gate; it turns this lane red.",
            )
    if not g.failed:
        g.ok(f"all {len(FLOOR_LANES)} floor lane(s) are present and non-optional")

    # --- 2. resolvable ------------------------------------------------------
    for name in required:
        if name in jobs:
            g.ok(f"required check {name!r} resolves to a job")
        else:
            g.fail(
                f"required check {name!r} matches no job in {args.workflows}.",
                "ci-required would wait forever for a check nothing produces.",
            )

    # --- 3, 4, 8, 8b and 10: over the CHECKED SET ---------------------------
    #
    # floor ∪ required, not FLOOR_LANES alone. FLOOR_LANES is the floor that
    # cannot be REMOVED; it must not also be the ceiling on what gets checked.
    # Before sweep B1 a lane that was required but not on the floor got one line
    # — "ok … resolves to a job" — while carrying continue-on-error and an
    # unanchored `make -i ci` (PR#6 VERIFY, FINDING 6): the guard printed a
    # reassuring ok for something it had not looked at, which is the exact
    # false-positive CI this program's opening paragraph exists to prevent.
    checked = sorted(set(FLOOR_LANES) | set(required))
    extra = [c for c in checked if c not in FLOOR_LANES]
    if extra:
        g.ok(f"checked set is floor ∪ required ({len(checked)} lanes); beyond the floor: {extra}")

    for lane in checked:
        entry = jobs.get(lane)
        if entry is None:
            continue  # already reported by check 2
        path, doc, job = entry

        if "pull_request" not in triggers(doc):
            g.fail(
                f"checked lane {lane!r} ({path.name}) is not triggered on pull_request.",
                "It would never run on a PR, so the fan-in would block until it timed out — "
                "fails closed, but it is not a gate.",
            )
        else:
            g.ok(f"checked lane {lane!r} runs on pull_request")

        # A lane is often an AGGREGATE whose `needs:` leg does the work —
        # `cache-matrix` needs `cache-matrix-leg`, and it is the LEG that runs
        # `make test-integration`. Checking only the named job would leave the
        # job that actually invokes make unanchored while this printed ok.
        for label, entry2 in expand_needs(lane, path, doc, job, jobs_by_key):
            check_anchor(g, label, entry2[0], entry2[1], entry2[2])
            check_pinned_make_steps(g, label, entry2[0], entry2[1], entry2[2], make_pins)
            check_required_invocations(
                g, label, entry2[0], job_display_name(label.split(":")[-1], entry2[2]),
                entry2[2], required_inv)
            check_job_surroundings(g, label, entry2[0], entry2[1], entry2[2])
            check_provenance(g, label, entry2[0], entry2[2])

        sites = continue_on_error_sites(job)
        if sites:
            g.fail(
                f"checked lane {lane!r} ({path.name}) carries continue-on-error:",
                *sites,
                "Present at all is the rule, whatever the value: a required lane that cannot "
                "fail is not a gate, and an expression form cannot be evaluated here.",
            )
        else:
            g.ok(f"checked lane {lane!r} carries no continue-on-error")

    # --- 9. one required lane that does not go through make -----------------
    check_direct_test_lane(g, required, jobs, direct_pins)

    # --- 5. the test selection the lanes actually run -----------------------
    if not args.skip_makefile:
        check_makefile_selection(g, args.makefile)

    # --- 6 and 7. runner and action pinning ---------------------------------
    bad_runners: list[str] = []
    unpinned: list[str] = []
    for path, doc in workflows.items():
        for key, job in (doc.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            runs_on = job.get("runs-on")
            if runs_on != RUNNER:
                bad_runners.append(f"{path.name}:{key} runs-on: {runs_on!r}")
            for step in job.get("steps") or []:
                if not isinstance(step, dict):
                    continue
                uses = step.get("uses")
                if isinstance(uses, str) and not uses.startswith("./") and not SHA_PIN.match(uses):
                    unpinned.append(f"{path.name}:{key} uses: {uses}")

    if bad_runners:
        g.fail("job(s) on a runner other than " + RUNNER + ":", *bad_runners)
    else:
        g.ok(f"every job runs on GitHub-hosted {RUNNER}")

    if unpinned:
        g.fail(
            "action(s) not pinned to a 40-character commit SHA:",
            *unpinned,
            "A moving tag is a supply-chain hole: the tag can be repointed.",
        )
    else:
        g.ok("every third-party action is pinned to a commit SHA")

    if g.failed:
        print("ci-required-guard: FAILED", file=sys.stderr)
        return 1
    print(f"ci-required-guard: passed ({len(required)} required check(s))")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
