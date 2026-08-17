# =============================================================================
# Sharpline — one parameterized build for all six Go binaries.
#
# CLAUDE.md §8:  deploy/docker/go.Dockerfile — "one parameterized build for all
#                6 Go binaries"  (api ingest pricer stream settle migrate)
# CLAUDE.md §9:  multi-stage everywhere; static Go binaries (CGO_ENABLED=0) into
#                gcr.io/distroless/static:nonroot; non-root UID; no shell in the
#                final layer; multi-arch via buildx; BuildKit cache mounts on the
#                Go module and build caches.
# CLAUDE.md §12: base images pinned by digest, not by floating tag.
#
# Usage (SERVICE is the only required build arg):
#
#   docker build -f deploy/docker/go.Dockerfile \
#     --build-arg SERVICE=api \
#     -t sharpline/api:dev .
#
#   docker buildx build -f deploy/docker/go.Dockerfile \
#     --platform linux/amd64,linux/arm64 \
#     --build-arg SERVICE=stream \
#     --build-arg VERSION="$(git describe --tags --always --dirty)" \
#     --build-arg REVISION="$(git rev-parse HEAD)" \
#     --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
#     -t ghcr.io/anpl1623/sharpline-stream:dev --push .
#
# NOTE — no `# syntax=` parser directive on purpose. Everything used here
# (RUN --mount=type=cache, --platform=$BUILDPLATFORM, TARGET* args) is supported
# by the BuildKit frontend built into the daemon, so the build pulls zero
# unpinned images. Adding `# syntax=docker/dockerfile:1` would reintroduce a
# floating tag, which CLAUDE.md §12 forbids and which a parser directive cannot
# carry a digest-pending comment on.
# =============================================================================


# -----------------------------------------------------------------------------
# Stage 1 — builder.
#
# Pinned to BUILDPLATFORM so the toolchain always runs natively and Go
# cross-compiles to TARGETPLATFORM. Building the builder itself for the target
# platform would run the whole compile under QEMU emulation, which is roughly an
# order of magnitude slower and is exactly the friction that makes people cheat
# and run `go build` on the host.
# -----------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

# Auto-populated by BuildKit. These MUST be declared bare — giving any of them a
# default (`ARG TARGETARCH=amd64`) makes the default win over the injected
# platform value, and the build silently cross-compiles to the wrong
# architecture. Verified the hard way: with a default present, a plain
# `docker build` on the arm64 dev Mac produced "building ./cmd/api for
# linux/amd64 on linux/arm64" and an amd64 binary inside an arm64 image.
ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

# Which cmd/ to build. No default — an unset SERVICE must fail loudly (CLAUDE.md
# §12: "fail fast and loudly on a bad config").
ARG SERVICE

# Stamped into the binary and into the OCI labels.
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOPATH=/go \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/root/.cache/go-build \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src

# Dependency layer first so a source-only edit does not re-resolve modules.
# `go.su[m]` is a glob: it makes go.sum optional so the image still builds
# against a module that has no external dependencies yet.
COPY go.mod go.su[m] ./
RUN --mount=type=cache,id=sharpline-gomod,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY . .

# BuildKit cache mounts on BOTH caches — CLAUDE.md §9 calls these out explicitly
# and non-optionally. The module cache is architecture-independent and shared;
# the compile cache is keyed per target arch so a cross-build for amd64 does not
# evict the arm64 objects (and vice versa) on the dev Mac.
#
# The two trailing checks are the prime-directive guard: a cgo-linked binary
# carries an ELF INTERP segment and cannot run on distroless/static. Failing
# here beats failing at container start.
#
# DO NOT REVERT THESE TO `go tool nm /out/sharpline | grep ' _cgo_'`. That was
# the original guard and it was VACUOUS, measured on 2026-08-17 during the phase
# 3a gate: `-ldflags="-s -w"` twelve lines below strips the symbol table, so
# `go tool nm` exits with "no symbol section / no symbols" and prints NOTHING.
# The grep therefore had no input and could never match. Proven by building
# ./cmd/api with CGO_ENABLED=1 and the identical `-s -w` flags in a builder with
# gcc installed: the result was dynamically linked against
# /lib/ld-musl-aarch64.so.1 -- i.e. it would have crashed on distroless/static
# -- and it passed the old guard cleanly. The guard was also wrong in the other
# direction: an UNSTRIPPED CGO_ENABLED=0 binary carries twelve `_cgo_*` runtime
# DATA symbols (_cgo_mmap, _cgo_sigaction, ...) that exist in every Go binary,
# so dropping `-s -w` would have made the old guard fail on a perfectly static
# build.
#
# What replaces it:
#   1. `go version -m` reads the build settings the toolchain EMBEDS in the
#      binary. That record survives `-s -w`, is written by the compiler rather
#      than inferred, and is exact.
#   2. the dynamic-loader string check catches the actual runtime failure the
#      comment above describes -- a statically linked Go binary never names
#      /lib/ld-musl-* or /lib64/ld-linux-*.
#
# No comments inside the RUN body — the Dockerfile parser handles whole-line
# comments inside a continued instruction inconsistently across frontends.
#
# VERSION/REVISION STAMPING — LIVE SINCE PHASE 3A. It used to be inert.
# The Go linker silently ignores `-X` for a symbol that does not exist, so these
# stamps did nothing until `internal/platform/buildinfo` landed and all six mains
# were wired to it. They now land: re-verified on 2026-08-17 by extracting
# /usr/local/bin/sharpline from sharpline/api:local and grepping the binary for
# the BUILD_DATE and REVISION passed on that build (both present, exactly once).
# Two symbol homes are stamped — `main.{version,commit,buildDate}` and
# `internal/platform/buildinfo.{Version,Commit,BuildDate}` — so either wiring
# works with no Dockerfile change. buildinfo is the one actually consumed; it is
# what lets every service export one shared sharpline_build_info gauge.
#
# `-X main.service` is deliberately NOT set: cmd/*/main.go declares
# `const service = "api"`, and `-X` cannot write to a const. The service name
# travels as the io.sharpline.service label instead.
RUN --mount=type=cache,id=sharpline-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=sharpline-gobuild-${TARGETOS}-${TARGETARCH}${TARGETVARIANT},target=/root/.cache/go-build,sharing=locked \
    set -eu; \
    if [ -z "${SERVICE:-}" ]; then \
        echo "ERROR: --build-arg SERVICE=<api|ingest|pricer|stream|settle|migrate> is required" >&2; \
        exit 64; \
    fi; \
    if [ ! -d "./cmd/${SERVICE}" ]; then \
        echo "ERROR: no such service: ./cmd/${SERVICE} does not exist" >&2; \
        echo "available services:" >&2; \
        ls -1 ./cmd >&2 || echo "  (no ./cmd directory in the build context)" >&2; \
        exit 64; \
    fi; \
    case "${TARGETVARIANT:-}" in \
        v5) export GOARM=5 ;; \
        v6) export GOARM=6 ;; \
        v7) export GOARM=7 ;; \
    esac; \
    echo "building ./cmd/${SERVICE} for ${TARGETOS}/${TARGETARCH}${TARGETVARIANT:-} on ${BUILDPLATFORM}"; \
    go build \
        -trimpath \
        -buildvcs=false \
        -ldflags="-s -w -buildid= \
                  -X main.version=${VERSION} \
                  -X main.commit=${REVISION} \
                  -X main.buildDate=${BUILD_DATE} \
                  -X github.com/anpl1623/sharpline/internal/platform/buildinfo.Version=${VERSION} \
                  -X github.com/anpl1623/sharpline/internal/platform/buildinfo.Commit=${REVISION} \
                  -X github.com/anpl1623/sharpline/internal/platform/buildinfo.BuildDate=${BUILD_DATE}" \
        -o /out/sharpline \
        ./cmd/${SERVICE}; \
    chmod 0555 /out/sharpline; \
    if ! go version -m /out/sharpline | grep -qE '^[[:space:]]*build[[:space:]]+CGO_ENABLED=0$'; then \
        echo "ERROR: CGO_ENABLED=0 was not honoured; the toolchain recorded:" >&2; \
        go version -m /out/sharpline | grep -E 'CGO_ENABLED' >&2 || \
            echo "  (no CGO_ENABLED build setting at all)" >&2; \
        exit 65; \
    fi; \
    if strings /out/sharpline | grep -qE '^/lib(64)?/ld-(musl|linux)'; then \
        echo "ERROR: binary names a dynamic loader, so it carries an ELF INTERP" >&2; \
        echo "       segment and cannot run on distroless/static" >&2; \
        exit 65; \
    fi; \
    ls -l /out/sharpline


# -----------------------------------------------------------------------------
# Stage 2 — runtime.
#
# gcr.io/distroless/static:nonroot — no shell, no package manager, no libc.
#
# Verified by exporting the pinned image: it ships /etc/passwd (nonroot = 65532),
# /etc/ssl/certs/ca-certificates.crt, and a full /usr/share/zoneinfo (1247
# entries). That is why the builder does NOT pass `-tags=timetzdata` — it would
# add ~450KB of duplicate tzdata to every one of the six images.
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

ARG SERVICE
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

LABEL org.opencontainers.image.title="sharpline-${SERVICE}"
LABEL org.opencontainers.image.description="Sharpline ${SERVICE} service — self-hosted real-time sports odds platform (simulation; no real money)"
LABEL org.opencontainers.image.source="https://github.com/anpl1623/sharpline"
LABEL org.opencontainers.image.url="https://github.com/anpl1623/sharpline"
LABEL org.opencontainers.image.documentation="https://github.com/anpl1623/sharpline/blob/main/CLAUDE.md"
LABEL org.opencontainers.image.revision="${REVISION}"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.vendor="anpl1623"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.base.name="gcr.io/distroless/static:nonroot"
LABEL org.opencontainers.image.base.digest="sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6"
LABEL io.sharpline.service="${SERVICE}"

COPY --from=builder --chown=65532:65532 /out/sharpline /usr/local/bin/sharpline

# 65532 is distroless' `nonroot` user. Numeric so Kubernetes
# runAsNonRoot / runAsUser can be enforced without a passwd lookup.
USER 65532:65532
WORKDIR /

ENTRYPOINT ["/usr/local/bin/sharpline"]
