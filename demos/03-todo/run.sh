#!/usr/bin/env bash
# Ask corpos to build a backend+frontend todo app (multi-file feature).
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
  -strong-provider anthropic -strong-model claude-opus-4-8 -strong-bound 2 \
  -max-spawns 4 \
  -mcp-url "$MCP_URL" -project corpos-demo -verify-dir "$REPO" \
  -risk-gate build-test \
  -prompt "Build a small todo app in this Go module. Backend: a package 'todo' with an in-memory store (Add(title), List(), Complete(id)) and a net/http API (POST /todos, GET /todos, POST /todos/{id}/complete) with table-driven tests for the store. Frontend: a minimal static index.html + app.js served by the backend that lists todos and adds one via the API. Make 'go test ./...' pass."
