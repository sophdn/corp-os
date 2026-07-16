#!/usr/bin/env bash
# scripts/worktree-new.sh <slug> — start a unit of work in a linked worktree.
#
# Creates ../<repo>-wt-<slug> on a fresh branch <slug> off the current main and
# runs worktree-setup.sh inside it so commits there are gate-only (full gate, no
# main-checkout guard, no shared-daemon restart). This is the ONLY sanctioned way
# to start code work — the main checkout stays on `main`. Run from the main
# checkout. (skill: worktree-workflow)
#
#   scripts/worktree-new.sh fix-thing
#   cd ../<repo>-wt-fix-thing && …work…
set -euo pipefail

slug="${1:-}"
if [ -z "$slug" ]; then
    echo "usage: scripts/worktree-new.sh <slug>" >&2
    exit 2
fi

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
repo_name="$(basename "$repo_root")"
wt_path="$(dirname "$repo_root")/${repo_name}-wt-${slug}"
branch="$slug"

if git show-ref --verify --quiet "refs/heads/$branch"; then
    echo "worktree-new: branch '$branch' already exists — pick another slug or reap it first." >&2
    exit 1
fi
if [ -e "$wt_path" ]; then
    echo "worktree-new: path '$wt_path' already exists." >&2
    exit 1
fi

git worktree add -b "$branch" "$wt_path" main
( cd "$wt_path" && ./scripts/worktree-setup.sh )
echo
echo "worktree-new: ready → cd $wt_path  (branch '$branch', gate-only hooks)"
