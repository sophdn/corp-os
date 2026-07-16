#!/usr/bin/env bash
# Drive corpos against the ./scenario repo to fix the bug red -> green.
# Prereqs: a toolkit-server (see ../../docs/SETUP.md), a worker endpoint, and
# keys for the mid/strong rungs (see ../../docs/TIERS.md). Adjust the ladder to
# your setup — the values below match what the committed transcript used.
set -euo pipefail

CORPOS="${CORPOS:-corpos}"                       # path to the corpos binary
MCP_URL="${MCP_URL:-http://localhost:3000}"      # your toolkit-server
WORKER_URL="${WORKER_URL:-http://localhost:8081/v1}"
WORKER_MODEL="${WORKER_MODEL:-Qwen2.5-32B-Instruct-Q4_K_M.gguf}"

REPO="$(cd "$(dirname "$0")/scenario" && pwd)"

"$CORPOS" \
  -provider openai -model-url "$WORKER_URL" -model "$WORKER_MODEL" \
  -mid-provider openrouter -mid-model google/gemini-3.1-flash-lite \
  -strong-provider anthropic -strong-model claude-opus-4-8 -strong-bound 2 \
  -mcp-url "$MCP_URL" -project corpos-demo -verify-dir "$REPO" \
  -risk-gate build-test \
  -prompt "The Go test in the ./calc package is failing. Find and fix the bug in calc/calc.go so that 'go test ./calc/' passes. Do not modify the test file."
