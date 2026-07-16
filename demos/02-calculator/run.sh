#!/usr/bin/env bash
# Ask corpos to build the calc package from a prose goal (feature pipeline).
# Prereqs: toolkit-server (../../docs/SETUP.md) + rungs (../../docs/TIERS.md).
set -euo pipefail

CORPOS="${CORPOS:-corpos}"
MCP_URL="${MCP_URL:-http://localhost:3000}"
WORKER_URL="${WORKER_URL:-http://localhost:8081/v1}"
WORKER_MODEL="${WORKER_MODEL:-Qwen2.5-32B-Instruct-Q4_K_M.gguf}"
REPO="$(cd "$(dirname "$0")/scenario" && pwd)"

"$CORPOS" \
  -provider openai -model-url "$WORKER_URL" -model "$WORKER_MODEL" \
  -mid-provider openrouter -mid-model google/gemini-3.1-flash-lite \
  -strong-provider anthropic -strong-model claude-opus-4-8 -strong-bound 4 \
  -mcp-url "$MCP_URL" -project corpos-demo -verify-dir "$REPO" \
  -risk-gate build-test \
  -prompt "Create a Go package 'calc' in this module with functions Add, Sub, Mul, and Div(a, b int) (int, error) where Div returns an error on divide-by-zero. Write a table-driven test calc/calc_test.go covering each function including the divide-by-zero error, then implement calc/calc.go so 'go test ./calc/' passes."
