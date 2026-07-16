#!/usr/bin/env bash
# scripts/worktree-merge.sh — merge linked-worktree branches back into the
# current checkout. Run from the MAIN checkout (on main):
#   conflict-surface check → ordered merge → gate (scripts/gate.sh) → reap the
#   merged worktrees + branches. Aborts cleanly on a real conflict. Never pushes.
# (skill: worktree-workflow)
#
# Usage: scripts/worktree-merge.sh [--check-only|--no-gate|--no-reap] <branch|path>...
set -euo pipefail

DO_GATE=1
DO_REAP=1
CHECK_ONLY=0
BRANCHES=()
while [ $# -gt 0 ]; do
    case "$1" in
        --check-only) CHECK_ONLY=1 ;;
        --no-gate)    DO_GATE=0 ;;
        --no-reap)    DO_REAP=0 ;;
        --*) echo "worktree-merge: unknown option: $1" >&2; exit 2 ;;
        *) BRANCHES+=("$1") ;;
    esac
    shift
done
if [ ${#BRANCHES[@]} -eq 0 ]; then
    echo "usage: scripts/worktree-merge.sh [--check-only|--no-gate|--no-reap] <branch|path>..." >&2
    exit 2
fi

# core.bare auto-reset (the Agent-tool's isolation=worktree flips it). Idempotent.
if [ "$(git config core.bare 2>/dev/null || echo false)" = "true" ]; then
    git config core.bare false
    echo "worktree-merge: reset core.bare true → false"
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
target="$(git branch --show-current 2>/dev/null || echo DETACHED)"
echo "worktree-merge: integration target = $target ($REPO_ROOT)"

# Resolve each arg to a branch; remember its worktree path for reaping.
declare -a MB=()
declare -A WT_OF=()
for arg in "${BRANCHES[@]}"; do
    if [ -d "$arg" ]; then
        b="$(git -C "$arg" branch --show-current 2>/dev/null || true)"
        [ -n "$b" ] || { echo "worktree-merge: $arg not on a branch — skipping" >&2; continue; }
        MB+=("$b"); WT_OF["$b"]="$(cd "$arg" && pwd)"
    else
        MB+=("$arg")
        wt="$(git worktree list --porcelain | awk -v r="refs/heads/$arg" '
            /^worktree /{p=$2} /^branch /{ if($2==r) print p }')"
        [ -n "$wt" ] && WT_OF["$arg"]="$wt"
    fi
done
[ ${#MB[@]} -gt 0 ] || { echo "worktree-merge: no mergeable branches resolved" >&2; exit 2; }

# Conflict-surface check: files touched by 2+ branches vs their merge-base.
echo "worktree-merge: conflict-surface check across ${#MB[@]} branch(es)…"
tmp="$(mktemp)"; : > "$tmp"
for b in "${MB[@]}"; do
    base="$(git merge-base HEAD "$b" 2>/dev/null || echo HEAD)"
    git diff --name-only "$base" "$b" | sed "s|\$|\t$b|"
done > "$tmp"
overlap="$(cut -f1 "$tmp" | sort | uniq -d || true)"
conflicts=0
if [ -n "$overlap" ]; then
    echo "  ⚠ files modified by more than one branch (textual merge may conflict):"
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        echo "      $f →$(awk -F'\t' -v f="$f" '$1==f{printf " %s",$2}' "$tmp")"
        conflicts=1
    done <<< "$overlap"
else
    echo "  ✓ no file touched by more than one branch"
fi
rm -f "$tmp"

if [ "$CHECK_ONLY" -eq 1 ]; then
    [ "$conflicts" -eq 1 ] && { echo "worktree-merge: --check-only → CONFLICTS PRESENT"; exit 1; }
    echo "worktree-merge: --check-only → clean (safe to merge-back)"; exit 0
fi
if [ "$conflicts" -eq 1 ]; then
    echo "worktree-merge: conflict surface non-empty — refusing to auto-merge." >&2
    echo "  Resolve the overlaps above (or merge by hand) and re-run." >&2
    exit 1
fi

# Ordered merge: fast-forward the first when possible, --no-ff the rest.
first=1
for b in "${MB[@]}"; do
    if [ "$first" -eq 1 ] && git merge --ff-only "$b" 2>/dev/null; then
        echo "worktree-merge: fast-forwarded to $b"
    else
        if ! git merge --no-ff -m "merge(worktree): integrate $b" "$b"; then
            echo "worktree-merge: MERGE CONFLICT on $b — aborting this merge." >&2
            git merge --abort 2>/dev/null || true
            echo "  Resolve manually; already-merged branches were kept." >&2
            exit 1
        fi
        echo "worktree-merge: merged $b (--no-ff)"
    fi
    first=0
done

# Gate the merged result.
if [ "$DO_GATE" -eq 1 ]; then
    echo ""
    echo "worktree-merge: gate → scripts/gate.sh"
    ./scripts/gate.sh
else
    echo "worktree-merge: --no-gate — skipping gate."
fi

# Reap merged worktrees + branches.
if [ "$DO_REAP" -eq 1 ]; then
    echo ""
    for b in "${MB[@]}"; do
        wt="${WT_OF[$b]:-}"
        if [ -n "$wt" ] && [ -d "$wt" ]; then
            git worktree remove --force "$wt" 2>/dev/null \
                && echo "worktree-merge: removed worktree $wt" \
                || echo "worktree-merge: could not remove worktree $wt (left in place)" >&2
        fi
        git branch -d "$b" 2>/dev/null \
            && echo "worktree-merge: deleted merged branch $b" \
            || echo "worktree-merge: branch $b not fully merged or in use — left in place" >&2
    done
    git worktree prune
else
    echo "worktree-merge: --no-reap — worktrees/branches left in place."
fi

echo ""
echo "worktree-merge: done — ${#MB[@]} branch(es) integrated into $target."
