#!/usr/bin/env bash
# measure.sh — the first live measurement of the B5b constructs (the PR#10
# verifier's own attempt was stopped by its safety classifier). Inert Makefiles
# in a scratch directory; the only "gate" is a stub that FAILS. Prints the exit
# status of raw `make ci` (no anchor) and the special-target lines of make's
# `-pn` database. Run on the host (3.81) and in an ubuntu:24.04 container (4.3).
set -uo pipefail
here="$(cd "$(dirname "$0")/../../../.." && pwd)"
G="$here/scripts/testdata/makeguard/good/Makefile"
t="$(mktemp -d)"; cd "$t"
printf '#!/bin/sh\necho "the gate FAILED"\nexit 1\n' > run-the-real-tests.sh; chmod +x run-the-real-tests.sh
cp "$G" clean.mk
{ cat "$G"; printf '.IGNORE:\n'; } > ignore-bare.mk
{ cat "$G"; printf '.IGNORE: ci\n'; } > ignore-per-target.mk
{ cat "$G"; printf 'I := .IGN\n$(I)ORE:\n'; } > ignore-computed.mk
{ cat "$G"; printf '.DEFAULT:\n\t@true\n'; } > default-recipe.mk
{ cat "$G"; printf 'D := .DEFA\n$(D)ULT:\n\t@true\n'; } > default-computed.mk
{ cat "$G"; printf '.EXTRA_PREREQS := Makefile\n'; } > extra-prereqs.mk
printf '.PHONY: ci\nci:\n%%:\n\t@echo PATTERN-RECIPE-RAN-for-$@\n.DEFAULT:\n\t@echo DEFAULT-RECIPE-RAN-for-$@\n' > phony-with-pattern-and-default.mk
printf 'ci:\n%%:\n\t@echo PATTERN-RECIPE-RAN-for-$@\n.DEFAULT:\n\t@echo DEFAULT-RECIPE-RAN-for-$@\n' > notphony-with-pattern-and-default.mk
printf '.PHONY: ci\nci: helper\n.DEFAULT:\n\t@echo DEFAULT-RECIPE-RAN-for-$@\n' > phony-with-ruleless-prerequisite.mk
make --version | head -1
for f in clean ignore-bare ignore-per-target ignore-computed default-recipe default-computed extra-prereqs; do
  printf '%-18s raw make ci exit=%s | db: %s\n' "$f" "$(make -f $f.mk ci >/dev/null 2>&1; echo $?)" \
    "$(make -pn -f $f.mk ci 2>/dev/null | awk '/^\.IGNORE:|^\.EXTRA_PREREQS/{print} /^\.DEFAULT:/{b=1;next} b&&/^$/{b=0} b&&/(commands|recipe) to execute/{print ".DEFAULT has a recipe"}' | tr '\n' ';')"
done
for f in phony-with-pattern-and-default notphony-with-pattern-and-default phony-with-ruleless-prerequisite; do
  printf '%-36s make ci -> %s\n' "$f" "$(make -f $f.mk ci 2>&1 | tr '\n' ' ')"
done
cd /; rm -rf "$t"
