#!/usr/bin/env bash
# lanes.sh <clone> <outdir> — wall-time every test lane. Each lane gets its own TMPDIR, so what it
# leaves behind is countable. The direct lanes run the EXACT bodies in .github/pinned-steps.yml
# (direct_test_steps: unit, integration, integration shuffled), read from the clone's own file.
# Needs PGP, VKP and RDP: host ports of the caller's own PostgreSQL 18, Valkey 9.1.2 and Redis 7.2.
set -uo pipefail
cd "$1"; out="$2"; mkdir -p "$out"
: "${PGP:?PGP}" "${VKP:?VKP}" "${RDP:?RDP}"
export VIZRA_TEST_DATABASE_URL="postgres://vizra@127.0.0.1:$PGP/vizra_test?sslmode=disable"
body() { python3 -c 'import sys,yaml; print(yaml.safe_load(open(".github/pinned-steps.yml"))["direct_test_steps"][int(sys.argv[1])], end="")' "$1"; }
lane() { # name cmd...
  local name="$1"; shift
  local tmp="$out/tmp-$name"; mkdir -p "$tmp"
  local t0 l0; t0=$(date +%s); l0="$(uptime | sed 's/.*averages: //')"
  ( export TMPDIR="$tmp"; "$@" ) > "$out/$name.log" 2>&1
  local rc=$? t1; t1=$(date +%s)
  echo "$name exit=$rc wall=$((t1-t0))s load-start=[$l0] load-end=[$(uptime | sed 's/.*averages: //')] leftover-tmp-entries=$(ls -A "$tmp" | wc -l | tr -d ' ') vizra-entries=$(ls -A "$tmp" | grep -c '^vizra-') size=$(du -sh "$tmp" | cut -f1)"
}
echo "tree $(git rev-parse HEAD) $(go version) $(make --version | head -1) cpus $(sysctl -n hw.ncpu 2>/dev/null || nproc)"
for i in 0 1 2; do echo "pinned body $i: $(body $i | grep -m1 'go test')"; done
lane make-ci make ci
lane fixtures-verify make fixtures-verify
lane unit bash -c "$(body 0)"; cp unit-events.json "$out/unit-events.json"
export VIZRA_TEST_CACHE_URL="redis://127.0.0.1:$VKP/0" VIZRA_TEST_CACHE_FLAVOUR=valkey
lane int-valkey bash -c "$(body 1)"; cp int-events.json "$out/int-valkey-events.json"
lane int-shuffle-valkey bash -c "$(body 2)"; cp int-shuffle-events.json "$out/int-shuffle-valkey-events.json"
export VIZRA_TEST_CACHE_URL="redis://127.0.0.1:$RDP/0" VIZRA_TEST_CACHE_FLAVOUR=redis
lane int-redis bash -c "$(body 1)"; cp int-events.json "$out/int-redis-events.json"
lane int-shuffle-redis bash -c "$(body 2)"; cp int-shuffle-events.json "$out/int-shuffle-redis-events.json"
echo "lanes done"
