#!/usr/bin/env bash
# Corp-OS gate — the single "can't launch broken code" entrypoint.
#
# Wired as the pre-commit hook (via core.hooksPath=.githooks) AND runnable
# standalone as the CI-equivalent (same checks, outside the hook). Dialed to 11
# from commit 1: gofmt, vet, golangci-lint, govulncheck, build, race-tested
# tests, and a coverage floor on the logic packages.
#
# Dev tools are pinned by version and run via `go run <tool>@<ver>` so go.mod
# stays dependency-free and the versions are reproducible.
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

COVERAGE_MIN=95
GOLANGCI_VERSION=v1.62.2
GOVULNCHECK_VERSION=v1.3.0

fail() { printf '[gate] FAIL: %b\n' "$*" >&2; exit 1; }

echo "[gate] 1/7 gofmt drift (whole tree)…"
# Check every .go file in the tree (gofmt skips .git/.githooks-style dot dirs),
# not just tracked files — so the manual gate matches the commit-time hook even
# for not-yet-staged files.
drift="$(gofmt -s -l .)"
[ -z "$drift" ] || fail "gofmt drift in:\n$drift\n(run: gofmt -s -w .)"

echo "[gate] 2/7 go vet…"
go vet ./... || fail "go vet"

echo "[gate] 3/7 golangci-lint ${GOLANGCI_VERSION}…"
go run "github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_VERSION}" run ./... || fail "golangci-lint"

echo "[gate] 4/7 govulncheck ${GOVULNCHECK_VERSION}…"
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./... || fail "govulncheck"

echo "[gate] 5/7 go build…"
go build ./... || fail "go build"

echo "[gate] 6/7 go test -race + coverage floor…"
go test -race ./... || fail "go test -race"
# Coverage threshold is measured over the logic packages (./internal/...);
# cmd/ entrypoints are thin wrappers, integration-tested, not unit-covered.
go test -covermode=atomic -coverprofile=coverage.out ./internal/... >/dev/null || fail "coverage run"
cov="$(go tool cover -func=coverage.out | awk '/^total:/{gsub("%","",$3);print $3}')"
echo "[gate]   internal/ coverage: ${cov}% (min ${COVERAGE_MIN}%)"
awk -v c="$cov" -v m="$COVERAGE_MIN" 'BEGIN{exit !(c+0>=m+0)}' \
  || fail "coverage ${cov}% < ${COVERAGE_MIN}% floor"

echo "[gate] 7/7 podman image build + smoke…"
# Phase C: the image is part of "can't launch broken code". Build it with
# rootless podman and prove the static binary runs on distroless. Set
# CORPOS_GATE_SKIP_IMAGE=1 to skip on boxes/CI without podman (the rest of the
# gate still fully runs).
if [ "${CORPOS_GATE_SKIP_IMAGE:-0}" = "1" ]; then
  echo "[gate]   skipped (CORPOS_GATE_SKIP_IMAGE=1)"
elif ! command -v podman >/dev/null 2>&1; then
  fail "podman not found — install it, or set CORPOS_GATE_SKIP_IMAGE=1 to skip the image stage"
else
  "$ROOT/scripts/build-image.sh" || fail "image build/smoke"
fi

echo "[gate] PASS — can't launch broken code."
