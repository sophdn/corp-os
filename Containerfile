# Corp-OS container image — a CGo-free static binary on a minimal non-root base.
#
# corpos is CGo-free (modernc.org/sqlite, pure Go), so CGO_ENABLED=0 yields a
# fully static binary with no libc dependency. We ship it on distroless/static
# rather than scratch because corpos speaks HTTPS to the Anthropic API and needs
# the CA-cert bundle, tzdata, and a non-root user that distroless/static carries
# — with no shell or package manager (smaller attack surface than alpine).
#
# Bases are pinned by DIGEST (not floating tags) so the build is reproducible.
# Refresh digests with: scripts/build-image.sh --refresh-bases (see that script).
#
#   builder : docker.io/library/golang:1.26.3
#   runtime : gcr.io/distroless/static-debian12:nonroot

# --- builder ---------------------------------------------------------------
FROM docker.io/library/golang:1.26.4@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS build

WORKDIR /src

# Module graph first so `go mod download` caches independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

# Then the source. .containerignore keeps VCS/build/doc cruft out of the context.
COPY . .

# Static, stripped, reproducible. CGO_ENABLED=0 is the invariant that lets the
# binary run on distroless/static with no C toolchain; -trimpath drops local
# paths; -ldflags '-w -s' strips DWARF + symbol table for a smaller binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags '-w -s' -o /out/corpos ./cmd/corpos

# --- runtime ---------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b669b9df05a88a085fefed6520c6d2268aabacf3008b149ddf877e752ae89400

# OCI provenance labels (cheap, aid `podman image inspect`/registry tooling).
LABEL org.opencontainers.image.title="corpos" \
      org.opencontainers.image.description="Corp-OS — a local, owned Go agent runtime driving toolkit-server over MCP" \
      org.opencontainers.image.source="https://github.com/sophdn/corp-os"

COPY --from=build /out/corpos /usr/local/bin/corpos

# distroless :nonroot already defaults to the nonroot user (uid 65532); make it
# explicit so the non-root guarantee survives a base-image change.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/corpos"]
