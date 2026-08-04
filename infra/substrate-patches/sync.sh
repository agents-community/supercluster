#!/usr/bin/env bash
# Take a newer Substrate and re-apply our deltas on top of it.
#
#   ./infra/substrate-patches/sync.sh                 # sync to upstream main
#   ./infra/substrate-patches/sync.sh 860250b         # sync to a specific commit
#   ./infra/substrate-patches/sync.sh --check         # verify patches still apply, change nothing
#
# Why patches and not a fork: the delta is ~46 lines across two files. A fork
# means a branch someone must remember to push and rebase; patches live beside
# the code that depends on them and are reviewed in the same PR.
#
# What the deltas are, and why they exist:
#
#   0001-atenet-stream-timeout  Upstream sets a 10s route timeout, which cuts
#                               every SSE stream mid-turn. Ours is Timeout: 0
#                               with IdleTimeout: 300s, so a heartbeating stream
#                               stays open and a dead one is still reaped.
#   0002-demo-claude-code-multiplex  Demo template we build against.
#
# Losing 0001 does not fail loudly — streams just start dying at 10s and it
# looks like a new bug. That is the reason this script verifies instead of
# assuming.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
substrate="${SUBSTRATE_DIR:-$(cd "$here/../../.." && pwd)/substrate}"
base_file="$here/BASE_COMMIT"

[ -d "$substrate/.git" ] || { echo "no substrate checkout at $substrate (set SUBSTRATE_DIR)" >&2; exit 1; }

patches=("$here"/0*.patch)
[ -e "${patches[0]}" ] || { echo "no patches in $here" >&2; exit 1; }

# --check: do the patches still apply to the CURRENT base? Cheap CI guard.
if [ "${1:-}" = "--check" ]; then
  base="$(cat "$base_file")"
  tmp="$(mktemp -d)"; trap 'git -C "$substrate" worktree remove --force "$tmp" 2>/dev/null || true' EXIT
  git -C "$substrate" worktree add -f "$tmp" "$base" >/dev/null 2>&1
  rc=0
  for p in "${patches[@]}"; do
    if git -C "$tmp" apply --check "$p" 2>/dev/null; then
      echo "  ok      $(basename "$p")"
    else
      echo "  FAILED  $(basename "$p")"; rc=1
    fi
  done
  exit $rc
fi

target="${1:-}"
cd "$substrate"

git fetch origin --quiet
if [ -z "$target" ]; then
  target="$(git rev-parse origin/main)"
fi
target="$(git rev-parse "$target")"
echo "==> syncing to ${target:0:12}"

# Refuse to run over uncommitted work: this checks out a different commit, and
# silently discarding someone's edits would be worse than stopping.
if ! git diff-index --quiet HEAD -- 2>/dev/null; then
  echo "substrate checkout has uncommitted changes — commit or stash first" >&2
  exit 1
fi

git checkout --quiet --detach "$target"

failed=0
for p in "${patches[@]}"; do
  if git apply --check "$p" 2>/dev/null; then
    git apply "$p"
    echo "  applied $(basename "$p")"
  else
    echo "  CONFLICT $(basename "$p") — upstream changed the code it touches" >&2
    failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo >&2
  echo "Resolve by hand, then regenerate the patch so the next sync is clean:" >&2
  echo "  cd $substrate && git diff > $here/00NN-name.patch" >&2
  echo "  echo $target > $base_file" >&2
  exit 1
fi

echo "$target" > "$base_file"
echo "==> BASE_COMMIT updated to ${target:0:12}"
cat <<EOF

Next, because a patched atenet is only live once it is rebuilt:
  export KO_DOCKER_REPO="\${IMAGE_REPO:?source infra/config.env}"
  ./hack/install-ate.sh --deploy-ate-system
  kubectl -n ate-system rollout status deploy/atenet-router

Then confirm streaming still survives past 10s — that is what 0001 protects:
  ./test/smoke-live.sh
EOF
