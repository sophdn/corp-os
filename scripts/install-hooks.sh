#!/usr/bin/env bash
# Wire the Corp-OS gate as the git pre-commit hook. Run once after cloning.
# core.hooksPath is a local config (not carried by a clone), so each checkout
# wires it explicitly; the hook + gate scripts themselves are tracked.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git config core.hooksPath .githooks
echo "core.hooksPath -> .githooks ; the gate (scripts/gate.sh) now runs on every commit."
