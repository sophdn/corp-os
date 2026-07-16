#!/usr/bin/env bash
# scripts/sync-userlib.sh — (re)populate the gitignored operator skills overlay
# (internal/skills/userlib/<slug>/SKILL.md) from the on-disk ~/.claude/skills tree.
#
# The userlib/ skills are gitignored (see internal/skills/userlib/README.md): they
# embed into THIS operator's corpos build but never ship in a vanilla clone. Because
# they are not version-controlled, a fresh clone / new machine starts empty — run
# this to rebuild them from your canonical skills tree (~/.claude/skills), which
# stays the source of truth.
#
# Operator PROFILES (internal/profile/userlib/*.toml) are hand-authored and NOT
# touched by this script — keep your own backup of those.
#
# Usage:
#   scripts/sync-userlib.sh                 refresh every slug already in userlib/,
#                                           OR every slug in scripts/userlib.skills
#                                           (a gitignored manifest, one slug per line)
#   scripts/sync-userlib.sh <slug> [slug…]  add/refresh exactly these slugs
#
# Env: SKILLS_SRC overrides the source tree (default ~/.claude/skills).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${SKILLS_SRC:-$HOME/.claude/skills}"
DEST="$ROOT/internal/skills/userlib"
MANIFEST="$ROOT/scripts/userlib.skills"

[ -d "$SRC" ] || { echo "[sync-userlib] source tree not found: $SRC" >&2; exit 1; }
mkdir -p "$DEST"

# Resolve the slug list: CLI args > manifest > existing userlib subdirs.
slugs=()
if [ "$#" -gt 0 ]; then
  slugs=("$@")
elif [ -f "$MANIFEST" ]; then
  while IFS= read -r line; do
    line="${line%%#*}"; line="$(echo "$line" | tr -d '[:space:]')"
    [ -n "$line" ] && slugs+=("$line")
  done < "$MANIFEST"
else
  for d in "$DEST"/*/; do
    [ -d "$d" ] || continue
    slugs+=("$(basename "$d")")
  done
fi

if [ "${#slugs[@]}" -eq 0 ]; then
  echo "[sync-userlib] nothing to sync: no args, no $MANIFEST, and $DEST is empty."
  echo "[sync-userlib] pass slugs explicitly the first time, e.g.:"
  echo "    scripts/sync-userlib.sh go-conventions rust-conventions python-conventions"
  exit 0
fi

ok=0; miss=0
for slug in "${slugs[@]}"; do
  s="$SRC/$slug/SKILL.md"
  if [ ! -f "$s" ]; then
    echo "[sync-userlib] MISS  $slug (no $s)" >&2; miss=$((miss+1)); continue
  fi
  mkdir -p "$DEST/$slug"
  cp "$(readlink -f "$s")" "$DEST/$slug/SKILL.md"
  echo "[sync-userlib] ok    $slug"
  ok=$((ok+1))
done

echo "[sync-userlib] synced $ok skill(s) into userlib/${miss:+, $miss missing}"
[ "$miss" -eq 0 ]
