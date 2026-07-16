#!/usr/bin/env bash
# scripts/guard-worktree-discipline.sh — pre-commit guard for the MAIN checkout.
#
# Refuses a commit made on a non-`main` branch IN THE MAIN CHECKOUT. Linked
# worktrees — where feature branches belong — are detected and skipped. This is
# the teeth behind the worktree-only workflow (skill: worktree-workflow): it
# makes the "main checkout parked on a stray feature branch" anti-pattern that
# accreted the old branch/worktree mess impossible to commit into.
#
# Wired as the FIRST step of the main checkout's pre-commit hook (called before
# the gate). Linked worktrees use a gate-only hook (scripts/worktree-setup.sh)
# that does NOT call this, so worktree commits proceed normally.
set -euo pipefail

git_dir="$(git rev-parse --absolute-git-dir)"
common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"

# A linked worktree's git dir lives under <common>/worktrees/<name>, so it
# differs from the common dir. Feature branches are EXPECTED there — allow.
if [ "$git_dir" != "$common_dir" ]; then
    exit 0
fi

branch="$(git symbolic-ref --short -q HEAD || echo DETACHED)"
if [ "$branch" != "main" ]; then
    repo_root="$(git rev-parse --show-toplevel)"
    cat >&2 <<EOF
✋ worktree-discipline: the MAIN checkout must stay on 'main'.
   Refusing to commit on '$branch' in $repo_root.

   Start work in a worktree instead:
       scripts/worktree-new.sh <slug>
   commit there (the gate still runs), then merge back with:
       scripts/worktree-merge.sh <branch>

   This keeps stray branches/worktrees from piling up. (skill: worktree-workflow)
   To bootstrap on main anyway (merges, hotfix), switch back: git checkout main
EOF
    exit 1
fi
exit 0
