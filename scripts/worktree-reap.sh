#!/usr/bin/env bash
# scripts/worktree-reap.sh — delete local branches already merged into main and
# prune their worktrees. Run from the main checkout. NEVER touches main or any
# unmerged branch (uses `git branch -d`, which refuses unmerged). The standalone
# cleanup for stragglers that worktree-merge.sh didn't reap. (skill: worktree-workflow)
set -euo pipefail

# The Agent-tool's isolation=worktree can flip core.bare=true on the shared
# config; reset it so work-tree commands work. Idempotent.
if [ "$(git config core.bare 2>/dev/null || echo false)" = "true" ]; then
    git config core.bare false
fi

git worktree prune

reaped=0
while IFS= read -r b; do
    [ -z "$b" ] && continue
    [ "$b" = "main" ] && continue
    wt="$(git worktree list --porcelain | awk -v r="refs/heads/$b" '
        /^worktree /{p=$2} /^branch /{ if($2==r) print p }')"
    if [ -n "$wt" ] && [ -d "$wt" ]; then
        if git worktree remove --force "$wt" 2>/dev/null; then
            echo "reap: removed worktree $wt"
        else
            echo "reap: could not remove worktree $wt — skipping branch $b" >&2
            continue
        fi
    fi
    if git branch -d "$b" 2>/dev/null; then
        echo "reap: deleted merged branch $b"
        reaped=$((reaped + 1))
    fi
done < <(git branch --merged main --format='%(refname:short)')

git worktree prune
echo "reap: done ($reaped branch(es) reaped)."
