#!/usr/bin/env bash
# lanes.sh <clone> <outdir> — wall-time every lane; each lane gets its own TMPDIR so leaked entries are countable.
set -uo pipefail
cd "$1"; out="$2"; mkdir -p "$out"
PGP=50869; VKP=50871; RDP=50924
export VIZRA_TEST_DATABASE_URL="postgres://vizra@127.0.0.1:$PGP/vizra_test?sslmode=disable"
lane() { # name cmd...
  local name="$1"; shift
  local tmp="$out/tmp-$name"; mkdir -p "$tmp"
  local t0=$(date +%s) l0="$(uptime | sed 's/.*averages: //')"
  ( export TMPDIR="$tmp"; "$@" ) > "$out/$name.log" 2>&1
  local rc=$? t1=$(date +%s)
  echo "$name exit=$rc wall=$((t1-t0))s load-start=[$l0] load-end=[$(uptime | sed 's/.*averages: //')] leftover-tmp-entries=$(ls -A "$tmp" | wc -l | tr -d ' ') vizra-entries=$(ls -A "$tmp" | grep -c '^vizra-') size=$(du -sh "$tmp" | cut -f1)"
}
unit_body='set -uo pipefail
rc=0
go test -race -count=1 -json ./... > unit-events.json || rc=$?
echo "$rc" > unit-exit.txt
echo "go test (unit) exited $rc"
python3 scripts/go-test-report.py --events unit-events.json --suite unit --floors scripts/test-floors.json --go-exit-file unit-exit.txt || exit 1
exit "$rc"'
int_body='set -uo pipefail
rc=0
go test -race -count=1 -tags=integration -json ./... > int-events.json || rc=$?
echo "$rc" > int-exit.txt
echo "go test (integration) exited $rc"
python3 scripts/go-test-report.py --events int-events.json --suite integration --floors scripts/test-floors.json --go-exit-file int-exit.txt || exit 1
exit "$rc"'
echo "tree $(git rev-parse HEAD) $(go version) $(make --version | head -1) cpus $(sysctl -n hw.ncpu)"
lane make-ci make ci
lane unit bash -c "$unit_body"; cp unit-events.json "$out/unit-events.json"
VIZRA_TEST_CACHE_URL="redis://127.0.0.1:$VKP/0" VIZRA_TEST_CACHE_FLAVOUR=valkey lane int-valkey bash -c "$int_body"; cp int-events.json "$out/int-valkey-events.json"
VIZRA_TEST_CACHE_URL="redis://127.0.0.1:$RDP/0" VIZRA_TEST_CACHE_FLAVOUR=redis lane int-redis bash -c "$int_body"; cp int-events.json "$out/int-redis-events.json"
echo "lanes done"
