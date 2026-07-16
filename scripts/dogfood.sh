#!/usr/bin/env bash
# dogfood.sh — run one real mechanical task through corpos under a job-profile and
# append a structured record to the dogfood ledger. This is the graduated-
# dogfooding harness (chain corpos-capability-scoping-foundation, task
# graduated-dogfooding-of-mechanical-profiles): it lets corpos carry real
# mechanical work under the risk gate NOW, and accrues the two signals the
# harness-swap decision weighs (docs/SWAP_VALIDATION_CRITERIA.md §3a):
#   - run-rate : session cost + tokens, per task (the economic bar)
#   - coverage : did corpos CARRY the task, or FALL BACK? (+ which capability)
#
# Start with the lowest-risk mechanical profiles (task-lifecycle, doc-filing,
# file-sort); ramp to the executing profiles (git-process, atomic-coding-chain)
# only once the exec substrate (gap #6) and an automated risk-gate approver land.
#
# Usage:
#   scripts/dogfood.sh <profile> "<task prompt>" [carried|fallback] [missing-capability]
# Env:
#   CORPOS_MCP_URL   toolkit-server HTTP (default http://localhost:3001)
#   CORPOS_PROJECT   project scope      (default mcp-servers)
#   DOGFOOD_LEDGER   JSONL ledger path  (default $XDG_STATE_HOME|~/.local/state/corpos/dogfood.jsonl)
#   CORPOS_TIMEOUT   per-turn timeout   (default 150s)
set -euo pipefail

PROFILE="${1:?usage: dogfood.sh <profile> \"<task>\" [carried|fallback] [missing-capability]}"
TASK="${2:?missing task prompt}"
OUTCOME="${3:-carried}"      # carried | fallback
MISSING="${4:-}"            # capability that forced a fallback (when OUTCOME=fallback)

MCP_URL="${CORPOS_MCP_URL:-http://localhost:3001}"
PROJECT="${CORPOS_PROJECT:-mcp-servers}"
TIMEOUT="${CORPOS_TIMEOUT:-150s}"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/corpos"
LEDGER="${DOGFOOD_LEDGER:-$STATE_DIR/dogfood.jsonl}"
mkdir -p "$STATE_DIR" "$(dirname "$LEDGER")"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
export PATH="$PATH:/usr/local/go/bin"

ERR="$(mktemp)"; OUT="$(mktemp)"
trap 'rm -f "$ERR" "$OUT"' EXIT

# Risk gate ENFORCED — dogfooding runs under the same safety posture the swap will.
set +e
timeout 300 go run ./cmd/corpos \
  -mcp-url "$MCP_URL" -project "$PROJECT" -profile "$PROFILE" \
  -risk-gate enforce -timeout "$TIMEOUT" \
  -session-dir "$STATE_DIR/dogfood-sessions" \
  -prompt "$TASK" >"$OUT" 2>"$ERR"
RC=$?
set -e

# Extract run-rate signals from the cost summary (stderr).
COST="$(grep -oE 'session cost \$[0-9.]+' "$ERR" | grep -oE '[0-9.]+' | head -1 || true)"
TOKLINE="$(grep -E 'in \+ [0-9]+ out tok' "$ERR" | head -1 || true)"
IN_TOK="$(echo "$TOKLINE"  | grep -oE '[0-9]+ in'  | grep -oE '[0-9]+' || true)"
OUT_TOK="$(echo "$TOKLINE" | grep -oE '[0-9]+ out' | grep -oE '[0-9]+' || true)"
PROJLINE="$(grep -oE 'projected [0-9]+ of [0-9]+ surfaces' "$ERR" | head -1 || true)"
TOOLS="$(grep -oE '\[tool [a-z_]+\.[a-z_]+ -> [a-z_]+\]' "$ERR" | sed -E 's/\[tool (.*) -> (.*)\]/\1:\2/' | paste -sd, - || true)"
ANSWER="$(head -c 400 "$OUT" | tr '\n' ' ')"

TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
jq -cn \
  --arg ts "$TS" --arg profile "$PROFILE" --arg project "$PROJECT" \
  --arg task "$TASK" --arg outcome "$OUTCOME" --arg missing "$MISSING" \
  --arg cost "${COST:-0}" --arg in "${IN_TOK:-0}" --arg out "${OUT_TOK:-0}" \
  --arg proj "$PROJLINE" --arg tools "$TOOLS" --arg answer "$ANSWER" \
  --argjson rc "$RC" \
  '{ts:$ts, profile:$profile, project:$project, task:$task, outcome:$outcome,
    missing_capability:$missing, exit:$rc, cost_usd:($cost|tonumber),
    in_tok:($in|tonumber), out_tok:($out|tonumber), projection:$proj,
    tools:$tools, answer_excerpt:$answer}' >>"$LEDGER"

echo "[dogfood] $PROFILE ($OUTCOME) — \$${COST:-0}, ${IN_TOK:-?} in tok, ${PROJLINE:-no projection}, tools=[${TOOLS:-none}], exit=$RC"
echo "[dogfood] ledger: $LEDGER"
[ "$RC" -eq 0 ] || echo "[dogfood] NOTE non-zero exit — inspect; mark this run a fallback if corpos couldn't carry it."
