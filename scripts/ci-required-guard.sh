#!/usr/bin/env bash
# Thin wrapper so `make ci-guard`, the ci-required workflow and any existing
# muscle memory keep working. The guard itself is Python, because the checks it
# makes need a YAML PARSER rather than a regex — see scripts/ci-required-guard.py
# for why the regex version had holes.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v python3 >/dev/null 2>&1; then
  echo "ci-required-guard: python3 is required. This lane is BLOCKED, not passed." >&2
  exit 2
fi

# TOMBSTONE (VZ-CI-001): removals are judged against the manifest at the merge
# base, read from git, never from the tree under test. CI passes the base in
# CI_REQUIRED_BASE_SHA (pull_request.base.sha / merge_group.base_sha); locally
# it is `git merge-base HEAD origin/main`. A shallow checkout lacks the base
# commit, so it is fetched (depth 1) by SHA. Anything that prevents reading it
# fails CLOSED. A caller that passes --manifest or --base-manifest (the fixture
# tests) supplies its own comparison and skips the git lookup.
manifest=.github/required-checks.txt
for arg in "$@"; do
  case "$arg" in
    --manifest | --manifest=* | --base-manifest | --base-manifest=*)
      exec python3 scripts/ci-required-guard.py "$@" ;;
  esac
done
base_unavailable() {
  echo "BASE MANIFEST UNAVAILABLE: $1"
  echo "  Removals from $manifest cannot be checked against the merge base, so this fails closed."
  exit 1
}
base_sha="${CI_REQUIRED_BASE_SHA:-}"
if [ -z "$base_sha" ]; then
  base_sha="$(git merge-base HEAD origin/main 2>/dev/null)" \
    || base_unavailable "no CI_REQUIRED_BASE_SHA and no merge base with origin/main (git fetch origin main?)"
fi
case "$base_sha" in
  *[!0-9a-f]* | "") base_unavailable "base '$base_sha' is not a commit SHA" ;;
esac
if ! git cat-file -e "${base_sha}^{commit}" 2>/dev/null; then
  git fetch --quiet --no-tags --depth=1 origin "$base_sha" 2>/dev/null \
    || base_unavailable "base commit $base_sha is not in this checkout and could not be fetched"
fi
base_manifest="$(mktemp)"
trap 'rm -f "$base_manifest"' EXIT
git show "${base_sha}:$manifest" > "$base_manifest" 2>/dev/null \
  || base_unavailable "$manifest does not exist at base $base_sha"
echo "base manifest: $manifest at $base_sha"
python3 scripts/ci-required-guard.py --base-manifest "$base_manifest" "$@"
