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
    for path, doc in workflows.items():
        for key, job in (doc.get("jobs") or {}).items():
            if isinstance(job, dict):
                jobs[job_display_name(key, job)] = (path, doc, job)

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
