#!/usr/bin/env bash
# Build and smoke-test the corpos container image with rootless Podman.
#
# This is the "image build is verified" half of Phase C: it builds the
# multi-stage Containerfile, tags the result, asserts the runtime stage runs as
# non-root, and runs `corpos -version` inside the container as an end-to-end
# proof that the static binary actually executes on distroless. The gate calls
# this as its final stage; it is also runnable standalone.
#
# Usage:
#   scripts/build-image.sh                 build + smoke-test (tags corpos:dev + corpos:<version>)
#   scripts/build-image.sh --refresh-bases pull floating base tags + print digests to pin in Containerfile
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

IMAGE="${CORPOS_IMAGE:-corpos}"
GOLANG_TAG="docker.io/library/golang:1.26.4"
DISTROLESS_TAG="gcr.io/distroless/static-debian12:nonroot"
SIZE_BUDGET_MB=20

fail() { printf '[build-image] FAIL: %b\n' "$*" >&2; exit 1; }

command -v podman >/dev/null 2>&1 || fail "podman not found (chain mandates rootless podman; \`sudo apt install podman\`)"

# --refresh-bases: re-pull the floating tags and print their current digests so
# they can be pasted back into the Containerfile's pinned FROM lines.
if [ "${1:-}" = "--refresh-bases" ]; then
  echo "[build-image] pulling floating base tags to read current digests…"
  podman pull "$GOLANG_TAG" >/dev/null
  podman pull "$DISTROLESS_TAG" >/dev/null
  echo "[build-image] pin these digests in Containerfile:"
  podman image inspect "$GOLANG_TAG"     --format '  builder : {{index .RepoDigests 0}}'
  podman image inspect "$DISTROLESS_TAG" --format '  runtime : {{index .RepoDigests 0}}'
  exit 0
fi

VERSION="$(go run ./cmd/corpos -version | awk '{print $2}')"
[ -n "$VERSION" ] || fail "could not determine corpos version"

echo "[build-image] building ${IMAGE}:dev (+ ${IMAGE}:${VERSION})…"
podman build -t "${IMAGE}:dev" -t "${IMAGE}:${VERSION}" -f Containerfile . \
  || fail "podman build"

# Non-root guarantee: distroless has no shell, so assert via the image config
# rather than running `id` inside the container.
user="$(podman image inspect "${IMAGE}:dev" --format '{{.Config.User}}')"
case "$user" in
  nonroot:nonroot|nonroot|65532*) echo "[build-image]   runs as non-root user: ${user}" ;;
  ""|root|0|0:0) fail "image runs as root (User=${user:-<empty>}) — non-root invariant broken" ;;
  *) echo "[build-image]   WARN: unexpected User=${user} (expected nonroot)" ;;
esac

# End-to-end proof the static binary executes on distroless.
echo "[build-image] running \`${IMAGE}:dev -version\` in the container…"
out="$(podman run --rm "${IMAGE}:dev" -version)"
echo "[build-image]   -> ${out}"
[ "$out" = "corpos ${VERSION}" ] || fail "container -version output %q != %q" "$out" "corpos ${VERSION}"

# Size is a target (not a hard contract): report it, warn if over budget.
bytes="$(podman image inspect "${IMAGE}:dev" --format '{{.Size}}')"
mb=$(( bytes / 1024 / 1024 ))
if [ "$mb" -le "$SIZE_BUDGET_MB" ]; then
  echo "[build-image]   image size: ${mb}MB (budget ${SIZE_BUDGET_MB}MB) ✓"
else
  echo "[build-image]   WARN: image size ${mb}MB exceeds ${SIZE_BUDGET_MB}MB budget"
fi

echo "[build-image] OK — ${IMAGE}:dev built, non-root, runs."
