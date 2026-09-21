#!/usr/bin/env python3
"""image-scan-verdict.py — judge a Trivy image report, and refuse to pass vacuously.

WHY THIS IS A SCRIPT AND NOT THREE LINES OF `jq` IN A WORKFLOW.

A vulnerability scan is the one lane whose GREEN is indistinguishable, at a
glance, from its NOT HAVING RUN. `trivy image` exits 0 and writes a
syntactically perfect report when:

  * it could not identify the operating system in the image, in which case it
    reports zero OS-package vulnerabilities — a clean bill of health for a layer
    it never looked at;
  * it scanned a DIFFERENT image than the one the lane built, because a stale
    `trivy-image.json` from an earlier step or an earlier run was left on disk;
  * its results array is empty or `null` because the analysis produced nothing.

And the scanner's own failure — a vulnerability-database download that 429s, a
daemon it cannot reach, an image reference that does not resolve — is a non-zero
EXIT with no findings, which a `|| true`, a `continue-on-error:` or an
unexamined pipeline turns into "no findings".

So this script takes the scanner's exit code as an INPUT it must judge, and
asserts the report describes the image the lane meant to scan, produced by the
scanner, over a distribution the scanner recognised. Only then does it count.

Exit codes
  0   scanned, and nothing at or above the failing severities
  1   scanned, and there are findings at or above the failing severities
  3   THE LANE DID NOT PRODUCE A VALID SCAN — scanner error, missing, empty,
      malformed, vacuous, or a report about the wrong image. Not a findings
      verdict: nobody may read this as "clean".

Usage
  image-scan-verdict.py --report trivy-image.json --image-ref vizra-core:scan
                        --scanner-exit-code-file trivy-exit-code.txt
                        [--fail-on HIGH,CRITICAL]
                        [--min-packages N]
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

EXIT_CLEAN = 0
EXIT_FINDINGS = 1
EXIT_NO_VALID_SCAN = 3

SEVERITY_ORDER = ["UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL"]


def die(*lines: str) -> None:
    print("::error::image-scan: " + lines[0])
    for extra in lines[1:]:
        print(f"         {extra}")
    print()
    print("  This is exit 3: THE LANE DID NOT PRODUCE A VALID SCAN.")
    print("  It is NOT a statement that the image is clean.")
    sys.exit(EXIT_NO_VALID_SCAN)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", type=Path, required=True)
    ap.add_argument("--image-ref", required=True)
    ap.add_argument("--scanner-exit-code-file", type=Path, required=True)
    ap.add_argument("--fail-on", default="HIGH,CRITICAL")
    # A Debian runtime image has dozens of packages. A report claiming to have
    # scanned one and finding a handful is a report about something else.
    ap.add_argument("--min-results", type=int, default=1)
    args = ap.parse_args()

    fail_on = {s.strip().upper() for s in args.fail_on.split(",") if s.strip()}
    unknown = fail_on - set(SEVERITY_ORDER)
    if unknown:
        die(f"--fail-on names severities Trivy does not use: {sorted(unknown)}")

    # --- 0. the scanner's own exit code, judged rather than discarded -------
    if not args.scanner_exit_code_file.is_file():
        die(
            f"{args.scanner_exit_code_file} does not exist.",
            "The lane must record the scanner's exit code so this step can judge it.",
            "A scan step whose exit code nothing reads is a scan step that cannot fail.",
        )
    raw = args.scanner_exit_code_file.read_text(encoding="utf-8", errors="replace").strip()
    if not raw:
        die(f"{args.scanner_exit_code_file} is empty; the scanner's exit code was not recorded.")
    try:
        scanner_exit = int(raw)
    except ValueError:
        die(f"{args.scanner_exit_code_file} does not contain an integer exit code: {raw!r}")
    if scanner_exit != 0:
        die(
            f"THE SCANNER FAILED: trivy exited {scanner_exit}.",
            "A scanner that errored reports no findings, which is not the same as no",
            "vulnerabilities. The lane is red because the scan did not happen.",
        )
    print(f"  ok    the scanner exited 0 (recorded in {args.scanner_exit_code_file.name})")

    # --- 1. the report exists and is a report ------------------------------
    if not args.report.is_file():
        die(f"{args.report} does not exist; the scanner produced no report.")
    blob = args.report.read_text(encoding="utf-8", errors="replace")
    if not blob.strip():
        die(f"{args.report} is empty; the scanner produced no report.")
    try:
        doc = json.loads(blob)
    except json.JSONDecodeError as exc:
        die(f"{args.report} is not valid JSON: {exc}")
    if not isinstance(doc, dict):
        die(f"{args.report} is not a Trivy report object (top level is {type(doc).__name__}).")
    if "SchemaVersion" not in doc:
        die(
            f"{args.report} has no SchemaVersion.",
            "That field is Trivy's own; a file without it was not written by the scanner.",
        )
    print(f"  ok    the report is Trivy schema {doc['SchemaVersion']}")

    # --- 2. it is a report about THE image the lane built -------------------
    artifact = doc.get("ArtifactName")
    if artifact != args.image_ref:
        die(
            f"the report describes {artifact!r}, not {args.image_ref!r}.",
            "A stale report from an earlier step or an earlier run must never be",
            "mistaken for this build's scan.",
        )
    kind = doc.get("ArtifactType")
    if kind not in ("container_image", "image"):
        die(
            f"the report's ArtifactType is {kind!r}, not a container image.",
            "A filesystem or repository scan does not cover the layers that ship.",
        )
    print(f"  ok    the report describes {artifact} ({kind})")

    # --- 3. the scanner recognised the distribution -------------------------
    # THE vacuous pass. With an unidentified OS, Trivy reports zero OS-package
    # vulnerabilities and exits 0 — a green lane over an unexamined layer.
    os_meta = (doc.get("Metadata") or {}).get("OS") or {}
    family = os_meta.get("Family")
    if not family:
        die(
            "the scanner did not identify the image's operating system.",
            "With no OS family it reports zero OS-package vulnerabilities and exits 0,",
            "which is a green lane over a layer nothing looked at.",
        )
    print(f"  ok    the scanner identified the OS: {family} {os_meta.get('Name', '')}".rstrip())

    # --- 4. it actually analysed something ----------------------------------
    results = doc.get("Results")
    if results is None:
        die("the report's Results is null; the scanner analysed nothing.")
    if not isinstance(results, list):
        die(f"the report's Results is a {type(results).__name__}, not a list.")
    if len(results) < args.min_results:
        die(
            f"the report carries {len(results)} result section(s), fewer than the "
            f"{args.min_results} this lane requires.",
            "An image with no analysed target is not an image that came back clean.",
        )
    classes = {r.get("Class") for r in results if isinstance(r, dict)}
    if "os-pkgs" not in classes:
        die(
            f"no result section has Class 'os-pkgs' (saw {sorted(c for c in classes if c)}).",
            "The distribution package layer is the one the runtime image is made of;",
            "a scan that did not cover it has not covered the image.",
        )
    print(f"  ok    {len(results)} result section(s), classes {sorted(c for c in classes if c)}")

    # --- 5. only now, the findings -----------------------------------------
    counts: dict[str, int] = {}
    offenders: list[str] = []
    for r in results:
        if not isinstance(r, dict):
            continue
        for v in r.get("Vulnerabilities") or []:
            sev = str(v.get("Severity", "UNKNOWN")).upper()
            counts[sev] = counts.get(sev, 0) + 1
            if sev in fail_on:
                offenders.append(
                    f"{sev} {v.get('VulnerabilityID', '?')} "
                    f"{v.get('PkgName', '?')} {v.get('InstalledVersion', '?')} "
                    f"({r.get('Target', '?')})"
                )

    total = sum(counts.values())
    print()
    print(f"Trivy, {artifact} — findings by severity:")
    for sev in SEVERITY_ORDER:
        if counts.get(sev):
            print(f"  {sev}: {counts[sev]}")
    print(f"  TOTAL: {total}")

    if offenders:
        print()
        print(f"::error::image-scan: {len(offenders)} finding(s) at or above {sorted(fail_on)}")
        for line in sorted(offenders):
            print(f"  {line}")
        return EXIT_FINDINGS

    print()
    print(f"A valid scan of {artifact} found nothing at or above {sorted(fail_on)}.")
    return EXIT_CLEAN


if __name__ == "__main__":
    sys.exit(main())
