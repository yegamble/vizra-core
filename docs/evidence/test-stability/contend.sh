#!/usr/bin/env bash
# contend.sh <clone> <outdir>: the unit suite and the valkey integration suite AT THE SAME TIME (two builders' lanes).
set -uo pipefail
cd "$1"; out="$2"; mkdir -p "$out/tu" "$out/ti"
export VIZRA_TEST_DATABASE_URL="postgres://vizra@127.0.0.1:50869/vizra_test?sslmode=disable"
echo "tree $(git rev-parse HEAD) load-start=[$(uptime | sed 's/.*averages: //')]"
t0=$(date +%s)
( TMPDIR="$out/tu" go test -race -count=1 -json ./... > "$out/unit-events.json" 2>"$out/unit.err"; echo "unit exit=$? wall=$(( $(date +%s)-t0 ))s" ) &
( TMPDIR="$out/ti" VIZRA_TEST_CACHE_URL="redis://127.0.0.1:50871/0" VIZRA_TEST_CACHE_FLAVOUR=valkey go test -race -count=1 -tags=integration -json ./... > "$out/int-events.json" 2>"$out/int.err"; echo "int-valkey exit=$? wall=$(( $(date +%s)-t0 ))s" ) &
wait
echo "load-end=[$(uptime | sed 's/.*averages: //')] leftover unit=$(ls -A $out/tu | wc -l | tr -d ' ') int=$(ls -A $out/ti | wc -l | tr -d ' ')"
