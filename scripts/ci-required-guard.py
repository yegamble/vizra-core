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

Checks, all of which must pass:

  1. FLOOR        every lane in FLOOR_LANES is present and not commented out.
  2. RESOLVABLE   every manifest name maps to a real job.
  3. TRIGGERED    every floor lane's workflow runs on `pull_request`.
  4. NO OPT-OUT   `continue-on-error` is absent AT ALL — any value, any
                  spelling — from a floor lane's job and from all of its steps.
  5. SELECTION    the Makefile lanes select a non-empty set: test-race covers
                  ./... and every -run pattern is non-empty.
  6. RUNNER       every job runs on GitHub-hosted ubuntu-24.04.
  7. PINNED       every action is pinned to a 40-character commit SHA.
  8. ANCHOR       every floor-lane job that invokes `make` runs
                  scripts/make-integrity-guard.sh FIRST, as its own step,
                  unconditionally and without continue-on-error — and neither
                  the workflow nor the job overrides `defaults.run.shell`, which
                  is the Actions analogue of `SHELL := /usr/bin/true`.
  9. DIRECT       at least one floor lane runs `go test` over `./...` WITHOUT
                  make, so a no-opped Makefile cannot make the UNIT suite
                  silent. UNIT only: that lane carries no `-tags=integration`,
                  and every integration invocation here goes through make.

Checks 8 and 9 exist because every required lane here runs through `make`, and a
verifier measured that ONE line in a Makefile — `SHELL := /usr/bin/true` or
`MAKEFLAGS += -i` — makes every recipe exit 0 without running
(docs/evidence/warroom/2026-09-20-vizra-search-pr2-revendor-VERIFY.md, FINDING
8). No check written inside a Makefile can prevent that; these two say the
out-of-make controls are present and armed.

What checks 8 and 9 do NOT cover, stated so nobody infers it:

  * Neither check reads the TEXT of a make step's `run:`, nor its step-level
    `env:`. `run: make -i ci`, `run: make SHELL=/usr/bin/true ci`,
    `run: make MAKEFLAGS=-i ci` and `env: MAKEFLAGS: -i` all leave BOTH guards
    exiting 0 while every make-driven lane goes silent (measured at f56dc03).
    Everything but the unit suite is in the blast radius, both integration
    lanes and both cache-matrix legs included. Queued for core hardening
    sweep B.
  * Check 9 does not assert that any test EXECUTED: with every `*_test.go`
    moved aside the lane exits 0 on `[no test files]`. Queued for sweep B.
  * `append-only` is a floor lane that prints a merge-base SHA and has no
    provenance step. Queued.

Usage:
    ci-required-guard.py [--workflows DIR] [--manifest FILE] [--makefile FILE]

scripts/guard_test.go drives it against scripts/testdata/, which holds one
crafted workflow per evasion, so the guard has negative cases of its own.
"""

from __future__ import annotations

import argparse
import re
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
MAKE_INVOCATION = re.compile(r"(?:^|[\s;&|(])make(?=\s|$)", re.M)


def step_runs_make(step: dict) -> bool:
    run = step.get("run")
    return isinstance(run, str) and bool(MAKE_INVOCATION.search(run))


def step_is_the_anchor(step: dict) -> bool:
    run = step.get("run")
    return isinstance(run, str) and MAKE_INTEGRITY_GUARD in run and not MAKE_INVOCATION.search(run)


def check_anchor(g: Guard, lane: str, path: Path, doc, job: dict) -> None:
    """Check 8: the out-of-make guard runs before make, and can actually fail.

    Four ways to remove the control without deleting the guard program, each of
    which this refuses by name: deleting the step, giving it an `if:`, marking it
    continue-on-error, or moving it after the first `make`.
    """
    steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
    make_at = next((i for i, s in enumerate(steps) if step_runs_make(s)), None)
    if make_at is None:
        g.ok(f"floor lane {lane!r} invokes no `make`, so it needs no make-integrity anchor")
        return

    anchor_at = next((i for i, s in enumerate(steps) if step_is_the_anchor(s)), None)
    label = steps[make_at].get("name") or f"step {make_at}"
    if anchor_at is None:
        g.fail(
            f"floor lane {lane!r} ({path.name}) invokes `make` (step {label!r}) with NO "
            f"{MAKE_INTEGRITY_GUARD} step before it.",
            "One line in the Makefile — `SHELL := /usr/bin/true` or `MAKEFLAGS += -i` — makes every",
            "recipe exit 0 without running, and no check inside a Makefile can stop that, because the",
            "neutering disarms that check too. The out-of-make step is the control; it must be present.",
        )
        return
    if anchor_at > make_at:
        g.fail(
            f"floor lane {lane!r} ({path.name}) runs the {MAKE_INTEGRITY_GUARD} step at position "
            f"{anchor_at}, AFTER `make` at position {make_at} ({label!r}).",
            "A neutered `make` step would already have reported success by then.",
        )
        return

    anchor = steps[anchor_at]
    if "if" in anchor:
        g.fail(
            f"floor lane {lane!r} ({path.name}) makes the {MAKE_INTEGRITY_GUARD} step CONDITIONAL "
            f"(if: {anchor['if']!r}).",
            "A skipped step is not a control, and this guard cannot evaluate an Actions expression.",
        )
    for key in anchor:
        if str(key).strip().lower().replace("_", "-") == "continue-on-error":
            g.fail(
                f"floor lane {lane!r} ({path.name}) marks the {MAKE_INTEGRITY_GUARD} step "
                f"{key}: {anchor[key]!r}.",
                "Its failure would be discarded. Present at all is the rule, whatever the value.",
            )

    # The Actions analogue of `SHELL := /usr/bin/true`: `defaults.run.shell`
    # replaces the shell every `run:` step uses, the anchor's included.
    for scope, holder in (("workflow", doc or {}), ("job", job)):
        shell = ((holder.get("defaults") or {}).get("run") or {}).get("shell")
        if shell is not None:
            g.fail(
                f"floor lane {lane!r} ({path.name}) sets a {scope}-level defaults.run.shell: {shell!r}.",
                "It replaces the shell EVERY `run:` step uses, so it no-ops the anchor and every make",
                "step at once — the Actions analogue of `SHELL := /usr/bin/true`.",
            )

    if not g.failed:
        g.ok(f"floor lane {lane!r} runs the {MAKE_INTEGRITY_GUARD} anchor (step {anchor_at}) before "
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


def check_direct_test_lane(g: Guard, required: list[str], jobs: dict) -> None:
    """Check 9: at least one required lane runs the UNIT suite without make.

    The anchor refuses a neutered Makefile BY NAME. This is the separate control
    for what the anchor cannot cover: whatever make did, a real failing UNIT test
    must still fail a required lane. Every other test invocation here goes
    through a make recipe.

    Scope, because the name of this check is broader than what it enforces: it
    accepts a `go test` over `./...` with no make. It does NOT require
    `-tags=integration` (so the integration suite is not covered), and it does
    not assert that any test actually ran. Both are queued for sweep B.
    """
    for name in required:
        entry = jobs.get(name)
        if entry is None:
            continue
        _, _, job = entry
        for step in job.get("steps") or []:
            if not isinstance(step, dict):
                continue
            run = step.get("run")
            if not isinstance(run, str):
                continue
            if "go test" not in run or MAKE_INVOCATION.search(run):
                continue
            if "./..." not in run:
                continue
            if "if" in step:
                continue  # a conditional lane is not a floor
            g.ok(f"required lane {name!r} runs the suite directly, without make: {run.strip()!r}")
            return
    g.fail(
        "no required lane runs `go test ./...` directly; every test invocation goes through `make`.",
        "A Makefile turned into a no-op would then make the whole suite SILENT rather than red, and",
        "`ci-required` would be green with nothing tested. One lane must invoke `go test` itself.",
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

    # --- 3 and 4. floor lanes: triggered, and no opt-out --------------------
    for lane in FLOOR_LANES:
        entry = jobs.get(lane)
        if entry is None:
            continue  # already reported
        path, doc, job = entry

        if "pull_request" not in triggers(doc):
            g.fail(
                f"floor lane {lane!r} ({path.name}) is not triggered on pull_request.",
                "It would never run on a PR, so the fan-in would block until it timed out — "
                "fails closed, but it is not a gate.",
            )
        else:
            g.ok(f"floor lane {lane!r} runs on pull_request")

        # A floor lane is often an AGGREGATE whose `needs:` leg does the work —
        # `cache-matrix` needs `cache-matrix-leg`, and it is the LEG that runs
        # `make test-integration`. Checking only the named job would leave the
        # job that actually invokes make unanchored while this printed ok.
        for label, entry2 in expand_needs(lane, path, doc, job, jobs_by_key):
            check_anchor(g, label, entry2[0], entry2[1], entry2[2])

        sites = continue_on_error_sites(job)
        if sites:
            g.fail(
                f"floor lane {lane!r} ({path.name}) carries continue-on-error:",
                *sites,
                "Present at all is the rule, whatever the value: a required lane that cannot "
                "fail is not a gate, and an expression form cannot be evaluated here.",
            )
        else:
            g.ok(f"floor lane {lane!r} carries no continue-on-error")

    # --- 9. one required lane that does not go through make -----------------
    check_direct_test_lane(g, required, jobs)

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
