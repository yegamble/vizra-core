#!/usr/bin/env bash
# assert-runtime-image — the runtime image carries no build toolchain, no -dev
# packages and no source tree, asserted so that a `docker run` FAILURE is red.
#
# WHY THIS IS A SCRIPT AND NOT THREE LINES IN THE WORKFLOW
# --------------------------------------------------------
# It was three lines in the workflow, and they had one shape between them
# (docker-build.yml:189, :197, :205 at 6829285 — PR#7 VERIFY, FINDING 1):
#
#     present=$(docker run --rm … vizra-core:ci sh -c '…; true') || true
#     if [ -n "$present" ]; then … fi
#
# The in-container script already ends `true`, so the container exits 0 on the
# happy path; the trailing `|| true` was therefore load-bearing ONLY when
# `docker run` ITSELF failed — no shell in the image, an exec-format error, a
# daemon that went away, a tag that does not resolve. In that case `present` is
# empty, the `if` is skipped, and the step prints "no build tooling on PATH" and
# PASSES. The lane reported the toolchain absent WITHOUT HAVING LOOKED. The
# third (`if docker run … test -e /src; then`) took its else branch on any
# docker failure and printed "no /src" for the same reason.
#
# So every `docker run` below has its exit status captured and judged:
#
#   * the toolchain probe must exit 0. Anything else is a failed assertion, not
#     an absent toolchain.
#   * the dpkg probe keeps its IN-CONTAINER `|| true` — `grep` finding no match
#     exits 1 legitimately — but the `docker run` exit is captured separately.
#   * `test -e /src` is the one command whose non-zero IS the answer, so exactly
#     0 (present) and 1 (absent) are verdicts and everything else is a failure.
#
# `$DOCKER` is injectable so scripts/scripts_test.go can drive this against a
# stub that fails, with no daemon and no image — which is the only way to
# demonstrate the control without a broken image to hand.
#
# Usage: assert-runtime-image.sh <image-ref> [denylist-file]
set -euo pipefail

image="${1:?usage: assert-runtime-image.sh <image-ref> [denylist-file]}"
denylist="${2:-scripts/runtime-toolchain-denylist.txt}"
DOCKER="${DOCKER:-docker}"

# STDERR, not stdout: `run_or_die` is called inside a command substitution, and
# anything it wrote to stdout would be captured into that variable and never
# seen. The subshell's non-zero exit still kills the script through `set -e`, so
# the lane went red with no reason printed until this was fixed — which is the
# same class of defect as the one this script exists to close.
die() {
  echo "::error::assert-runtime-image: $1" >&2
  shift
  for line in "$@"; do echo "         $line" >&2; done
  exit 1
}

# A `docker run` that did not run is never an assertion about the image.
run_or_die() {
  local what="$1"; shift
  local out rc=0
  out="$("$DOCKER" "$@" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    die "the container did not run while checking $what (docker exited $rc)." \
        "This is a FAILED ASSERTION, not a clean image: nothing looked inside it." \
        "docker output:" "  ${out}"
  fi
  printf '%s' "$out"
}

if [ ! -f "$denylist" ]; then
  die "$denylist does not exist; there is no toolchain list to check against."
fi
# `|| true` on the EXTRACTION only: grep exits 1 on an empty or comments-only
# file, and under `set -e` that killed the script before `die` could say why.
# A red with no reason is its own defect. The emptiness test below is what
# refuses the vacuous list, and it says so.
tools="$(grep -v '^#' "$denylist" 2>/dev/null | grep -v '^$' | tr '\n' ' ' || true)"
if [ -z "${tools// /}" ]; then
  die "$denylist lists no tool names; the toolchain assertion would check nothing and pass." \
      "An empty or comments-only denylist is a vacuous assertion, not a clean image."
fi

echo "checking for: $tools"
present="$(run_or_die "the build toolchain" run --rm -e TOOLS="$tools" "$image" sh -c '
  for t in $TOOLS; do
    command -v "$t" >/dev/null 2>&1 && echo "$t"
  done
  true')"
if [ -n "$present" ]; then
  die "the runtime image carries build tooling:" "$present"
fi
echo "  ok   no build tooling on PATH ($(printf '%s' "$tools" | wc -w | tr -d ' ') names checked)"

# `grep` exiting 1 on no match is legitimate, so that `|| true` stays INSIDE the
# container. The docker exit is judged by run_or_die.
devpkgs="$(run_or_die "-dev packages" run --rm "$image" sh -c \
  "dpkg-query -W -f='\${binary:Package}\n' 2>/dev/null | grep -- '-dev\$' || true")"
if [ -n "$devpkgs" ]; then
  die "the runtime image carries -dev packages:" "$devpkgs"
fi
echo "  ok   no -dev packages installed"

# Here a non-zero exit IS the answer, so 0 and 1 are verdicts and nothing else.
rc=0
srcout="$("$DOCKER" run --rm "$image" test -e /src 2>&1)" || rc=$?
case "$rc" in
  0) die "the libvips source tree is still in the runtime image (/src exists)" ;;
  1) echo "  ok   no /src" ;;
  *) die "the container did not run while checking /src (docker exited $rc)." \
         "0 and 1 are the verdicts here; anything else means nothing looked." \
         "docker output:" "  ${srcout}" ;;
esac

echo "assert-runtime-image: ok — $image carries no toolchain, no -dev packages and no source tree"
