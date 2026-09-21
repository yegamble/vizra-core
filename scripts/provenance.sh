#!/usr/bin/env bash
# provenance — say which tree this lane actually tested, and prove it.
#
# WHY
# ---
# `actions/checkout` with no `ref:` on a `pull_request` event checks out
# `refs/pull/N/merge` — the PR head merged into the CURRENT tip of the base
# branch. That tree is not the head SHA, and it changes whenever the BASE moves,
# with no change to the pull request and no signal to anyone.
#
# A verifier caught the consequence in the meta repo (FINDING 5,
# docs/evidence/warroom/2026-09-21-meta-pr3-validate-lane-VERIFY.md): a step
# whose whole job was to record provenance printed `source SHA: 5dfa75c` while
# standing in tree `e85e875`, and a count read off "this SHA" was the merge
# tree's count, not the head's. The war-room merge rule reads "ci-required is
# green on the verified SHA" and "the head has not moved" — both can hold while
# what the lanes EXECUTED against is a tree nobody verified.
#
# So this prints three SHAs, each labelled, and does not guess which one the
# reader meant.
#
# WHY NOT github.event.pull_request.base.sha
# ------------------------------------------
# That field is the base as of when the PR was opened or last synchronised. It
# goes stale the moment the base branch advances, while the merge ref does not.
# `HEAD^1` on the merge ref is the base that was ACTUALLY merged in. They are
# routinely different, and only one of them describes the bytes that ran.
#
# SHALLOW CLONES — measured, not assumed
# --------------------------------------
# `actions/checkout` defaults to `fetch-depth: 1`. In a depth-1 clone the merge
# commit's PARENTS are grafted away: `git rev-parse HEAD^1` fails outright and
# `git rev-list --parents -n 1 HEAD` prints the commit alone. (Measured locally
# on git 2.50.1 against a real shallow clone.) Two consequences:
#
#   * the workflows that call this set `fetch-depth: 2`, which is enough for
#     `HEAD^1`/`HEAD^2` to resolve and is NOT the same as pinning `ref:` to the
#     head — the merge ref is still what is checked out and tested;
#   * this script falls back to reading the parent lines out of the commit
#     OBJECT with `git cat-file -p HEAD`, which works at any depth because the
#     merge commit itself is always present. Which method produced the answer is
#     printed, so a reader is never guessing.
#
# Inputs (all optional; the script degrades to what it can prove):
#   GITHUB_EVENT_NAME, GITHUB_SHA, GITHUB_REF, PR_HEAD_SHA
set -uo pipefail

event="${GITHUB_EVENT_NAME:-local}"
github_sha="${GITHUB_SHA:-}"
pr_head="${PR_HEAD_SHA:-}"

tested="$(git rev-parse HEAD)"
method="rev-parse"
p1="$(git rev-parse --verify --quiet HEAD^1 || true)"
p2="$(git rev-parse --verify --quiet HEAD^2 || true)"
if [ -z "$p1" ]; then
  # Shallow clone: the parent OBJECTS are absent, but the merge commit object
  # lists them, so read it directly.
  method="cat-file (shallow clone: parent objects are not present)"
  parents="$(git cat-file -p HEAD | awk '/^parent /{print $2} /^$/{exit}')"
  p1="$(printf '%s\n' "$parents" | sed -n 1p)"
  p2="$(printf '%s\n' "$parents" | sed -n 2p)"
fi

echo "provenance"
echo "  event:                        ${event}"
echo "  ref:                          ${GITHUB_REF:-(none)}"
echo "  TESTED TREE (git rev-parse HEAD):  ${tested}"
echo "      ^ this is what the commands in this job actually ran against"
echo "  parent method:                ${method}"
echo "  base actually merged in (HEAD^1): ${p1:-(none — HEAD is not a merge commit)}"
echo "  head actually merged in (HEAD^2): ${p2:-(none — HEAD is not a merge commit)}"
echo "  PR head SHA (github.event.pull_request.head.sha): ${pr_head:-(not a pull_request event)}"
echo "  github.sha:                   ${github_sha:-(unset)}"

rc=0
case "$event" in
  pull_request|pull_request_target)
    echo
    echo "  On a pull_request, actions/checkout with no \`ref:\` checks out refs/pull/N/merge:"
    echo "  the PR head merged into the CURRENT base tip. HEAD is therefore NEITHER the head SHA"
    echo "  nor the base SHA, and it changes whenever the base branch moves — with no change to"
    echo "  the pull request. github.event.pull_request.base.sha is NOT printed above because it"
    echo "  is the base as of the last synchronise and goes stale; HEAD^1 is the base that was"
    echo "  really merged in."
    if [ -n "$pr_head" ] && [ -n "$p2" ] && [ "$p2" != "$pr_head" ]; then
      echo "::error::HEAD^2 (${p2}) is not the PR head SHA (${pr_head}). The checkout is not the"
      echo "merge ref this lane assumes, so nothing above describes what was tested."
      rc=1
    elif [ -n "$pr_head" ] && [ -z "$p2" ]; then
      echo "::error::HEAD has no second parent, so this checkout is NOT refs/pull/N/merge — most"
      echo "likely \`ref:\` was pinned to the head. Pinning hides base-side breakage: the lane"
      echo "would then never test the merge that is about to land."
      rc=1
    else
      echo "  checked: HEAD^2 == the PR head SHA, so this really is the merge ref."
    fi
    ;;
  merge_group)
    echo
    echo "  On merge_group, HEAD is the merge-queue commit GitHub built for this entry. That is"
    echo "  the tree that will land, so HEAD is the authoritative SHA here and github.sha agrees"
    echo "  with it. There is no pull_request payload, so no PR head SHA is printed."
    if [ -n "$github_sha" ] && [ "$github_sha" != "$tested" ]; then
      echo "::error::github.sha (${github_sha}) is not the checked-out tree (${tested})."
      rc=1
    fi
    ;;
  push)
    echo
    echo "  On push, HEAD is the pushed commit itself: no merge ref exists, and HEAD^1 above is"
    echo "  simply its first parent on the branch. github.sha is the same commit."
    if [ -n "$github_sha" ] && [ "$github_sha" != "$tested" ]; then
      echo "::error::github.sha (${github_sha}) is not the checked-out tree (${tested})."
      rc=1
    fi
    ;;
  *)
    echo
    echo "  Event '${event}' has no merge-ref convention asserted here; the SHAs above are"
    echo "  reported without a cross-check."
    ;;
esac

echo
echo "  environment: $(uname -s)/$(uname -m), $(git --version)"
exit "$rc"
