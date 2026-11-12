#!/usr/bin/env python3
"""VZ-FOUND-001 / VZ-FOUND-004: lockfile and toolchain/stack pins match ADR-001.

Fails (exit 1) when:
  * go.sum is missing or empty;
  * go.mod's `go` or `toolchain` line differs from the ADR-001 pin;
  * any actions/setup-go step gets its Go version from anywhere except
    `go-version-file: go.mod` (setup-go v6+ honours the `toolchain` line,
    which is go1.27.4 as measured in the govulncheck job log 2026-11-12) or
    `go-version: <exact pin>`;
  * Echo, go-redis or govips in go.mod, or sqlc in the workflows, do not
    match their ADR-001 rows.

The PINS table is copied from meta docs/adr/ADR-001-stack-pins-and-licences.md
(as amended by ADR-010). Change both together.
Usage: check-stack-pins.py [--root DIR]
"""
import pathlib
import re
import sys

GO_LINE = "1.26.0"          # language floor, fixed in core go.mod
TOOLCHAIN = "1.27.4"        # ADR-001 "Go toolchain | 1.27.x (1.27.4)"
# module -> (ADR-001 row, predicate on version string)
def _ver(v):
    return tuple(int(x) for x in re.findall(r"\d+", v)[:3])

MODULES = {
    "github.com/labstack/echo/v5": ("echo/v5 >= v5.4.4", lambda v: _ver(v) >= (5, 4, 4) and _ver(v)[0] == 5),
    "github.com/redis/go-redis/v9": ("go-redis/v9 v9.22.x", lambda v: _ver(v)[:2] == (9, 22)),
    "github.com/davidbyttow/govips/v2": ("govips/v2 v2.18.0", lambda v: _ver(v) == (2, 18, 0)),
}
REQUIRED_MODULES = {"github.com/labstack/echo/v5", "github.com/redis/go-redis/v9"}
SQLC = "v1.31.1"            # ADR-001 "sqlc | 1.31.1"


def main(argv):
    root = pathlib.Path(__file__).resolve().parent.parent
    if len(argv) == 2 and argv[0] == "--root":
        root = pathlib.Path(argv[1])
    errs = []
    gosum = root / "go.sum"
    if not gosum.is_file() or gosum.stat().st_size == 0:
        errs.append("go.sum is missing or empty: the lockfile must be committed")
    gomod = (root / "go.mod").read_text() if (root / "go.mod").is_file() else ""
    if not gomod:
        errs.append("go.mod is missing")
    m = re.search(r"^go\s+(\S+)\s*$", gomod, re.M)
    if not m or m.group(1) != GO_LINE:
        errs.append(f"go.mod `go` line is {m.group(1) if m else 'absent'}, want {GO_LINE}")
    m = re.search(r"^toolchain\s+go(\S+)\s*$", gomod, re.M)
    if not m or m.group(1) != TOOLCHAIN:
        errs.append(f"go.mod `toolchain` line is {('go' + m.group(1)) if m else 'absent'}, want go{TOOLCHAIN} (ADR-001)")
    for mod, (row, ok) in MODULES.items():
        mm = re.search(r"^\s*(?:require\s+)?" + re.escape(mod) + r"\s+(v\S+)", gomod, re.M)
        if not mm:
            if mod in REQUIRED_MODULES:
                errs.append(f"{mod} not in go.mod (ADR-001 row: {row})")
            continue
        if not ok(mm.group(1)):
            errs.append(f"{mod} is {mm.group(1)}, ADR-001 row says {row}")
    wf_dir = root / ".github" / "workflows"
    sqlc_seen = False
    for wf in sorted(wf_dir.glob("*.y*ml")):
        lines = wf.read_text().splitlines()
        for i, line in enumerate(lines):
            if "actions/setup-go@" in line:
                # Step indent = column of the dash that opens this step.
                j = i
                while j >= 0 and not re.match(r"^\s*- ", lines[j]):
                    j -= 1
                dash = len(lines[j]) - len(lines[j].lstrip(" "))
                src = []
                for nxt in lines[i + 1:]:
                    s = nxt.strip()
                    if not s or s.startswith("#"):
                        continue
                    if len(nxt) - len(nxt.lstrip(" ")) <= dash:
                        break
                    km = re.match(r"(go-version(?:-file)?)\s*:\s*(.*?)\s*(#.*)?$", s)
                    if km:
                        src.append((km.group(1), km.group(2).strip("'\"")))
                good = src in ([("go-version-file", "go.mod")], [("go-version", TOOLCHAIN)])
                if not good:
                    errs.append(f"{wf.name}:{i + 1}: setup-go reads {src or 'nothing'}; want go-version-file: go.mod or go-version: {TOOLCHAIN}")
            for sm in re.finditer(r"sqlc-dev/sqlc/cmd/sqlc@(\S+)", line):
                sqlc_seen = True
                if sm.group(1) != SQLC:
                    errs.append(f"{wf.name}:{i + 1}: sqlc {sm.group(1)}, ADR-001 pins {SQLC}")
    if not sqlc_seen:
        errs.append(f"no workflow installs sqlc@{SQLC} (ADR-001 row: sqlc 1.31.1)")
    for e in errs:
        print(f"  FAIL  {e}")
    if errs:
        return 1
    print(f"  ok    go.sum present; go {GO_LINE}; toolchain go{TOOLCHAIN}; setup-go reads the pin; Echo/go-redis/sqlc match ADR-001 (govips not yet a dependency)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
