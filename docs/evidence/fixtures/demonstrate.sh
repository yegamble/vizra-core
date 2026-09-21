#!/usr/bin/env bash
# VZ-FOUND-007 demonstrations: each required check shown RED against a
# controlled mutation and GREEN when the mutation is reverted.
#
# Re-runnable by a verifier from a clean checkout:
#
#     ./docs/evidence/fixtures/demonstrate.sh
#
# It mutates tracked files and restores them with `git checkout --` after each
# case, so it REFUSES to run with a dirty tree — otherwise the restore would
# throw away work that was not its to throw away.
set -uo pipefail
cd "$(dirname "$0")/../../.."

if [ -n "$(git status --porcelain)" ]; then
  echo "refusing to run: the working tree is dirty and this script restores files with 'git checkout --'." >&2
  git status --porcelain >&2
  exit 2
fi

rule() { printf '\n========================================================================\n%s\n========================================================================\n' "$1"; }
restore() { git checkout -- "$@"; }

echo "host:      $(uname -s)/$(uname -m)"
echo "go:        $(go version)"
echo "go.mod go: $(grep -E '^go ' go.mod | awk '{print $2}')"
echo "HEAD:      $(git rev-parse HEAD)"
echo "date:      $(date -u +%Y-%m-%dT%H:%M:%SZ)"

rule "GREEN baseline: make fixtures-verify"
make fixtures-verify
echo "exit=$?"

rule "RED 1: one byte of one fixture changed"
make fixtures >/dev/null
python3 - <<'EOF'
import pathlib
p = pathlib.Path("testdata/fixtures/jpeg-exif-orientation6-gps.jpg")
b = bytearray(p.read_bytes()); b[len(b)//2] ^= 0x01; p.write_bytes(bytes(b))
print("flipped one bit in", p)
EOF
make fixtures-verify
echo "exit=$?  (expected non-zero, naming the file)"

rule "GREEN 1: the fixture regenerated"
make fixtures >/dev/null && make fixtures-verify
echo "exit=$?"

rule "RED 2: the generator changed without the manifest being re-pinned"
printf '\n// demonstration: a change to the generator\n' >> internal/fixtures/raster.go
make fixtures-verify
echo "exit=$?  (expected non-zero: generator-source AND the fixture bytes)"

rule "GREEN 2: the generator change reverted"
restore internal/fixtures/raster.go
make fixtures-verify
echo "exit=$?"

rule "RED 3: a pinned byte-influencing version bumped without a re-pin"
echo "--- 3a. go.mod language version (a REAL bump: it changes deflate output) ---"
sed -i.bak 's/^go 1\.26\.0$/go 1.26.2/' go.mod && rm -f go.mod.bak
grep -E '^go ' go.mod
make fixtures-verify
echo "exit=$?  (expected non-zero: toolchain-pin, and the PNG/GIF fixture bytes move)"
restore go.mod

echo "--- 3b. the recorded codec tool version, with the bytes left alone ---"
python3 - <<'EOF'
import pathlib
p = pathlib.Path("internal/fixtures/codec.go")
s = p.read_text()
s = s.replace('ToolVersion: "ffmpeg 8.1, darwin/arm64"', 'ToolVersion: "ffmpeg 9.0, darwin/arm64"')
p.write_text(s)
print("bumped the recorded ffmpeg version in CodecInputs()")
EOF
make fixtures-verify
echo "exit=$?  (expected non-zero: codec-tool-pin)"

rule "GREEN 3: both pins restored"
restore go.mod internal/fixtures/codec.go
make fixtures-verify
echo "exit=$?"

rule "RED 4: the fixtures lane deleted from the required-checks manifest"
python3 - <<'EOF'
import pathlib
p = pathlib.Path(".github/required-checks.txt")
lines = p.read_text().split("\n")
p.write_text("\n".join(l for l in lines if l.strip() != "fixtures"))
print("removed the 'fixtures' line from .github/required-checks.txt")
EOF
./scripts/ci-required-guard.sh
echo "exit=$?  (expected non-zero: the guard FLOOR names the missing lane)"

rule "GREEN 4: the lane restored"
restore .github/required-checks.txt
./scripts/ci-required-guard.sh >/dev/null
echo "exit=$?"

rule "Final state"
make fixtures >/dev/null
make fixtures-verify
git status --porcelain
echo "(a clean status above means every mutation was restored)"
