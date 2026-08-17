# syntax reminder: no `# syntax=` directive on purpose — that would pull an
# unpinned frontend image, and CLAUDE.md §12 requires everything pinned by digest.
# BuildKit's built-in frontend (Docker 23+) already supports `RUN --mount=type=cache`.
#
# ---------------------------------------------------------------------------
# sharpline `tools` image  (CLAUDE.md §9 container inventory, row `tools`)
# ---------------------------------------------------------------------------
# "One-off tooling (linting, codegen, OpenAPI generation, `psql` and Kafka CLI
#  shells, Terraform, Locust, Playwright) is exposed as `make` targets that wrap
#  `docker run`." — CLAUDE.md prime directive
#
# Every developer action that would otherwise require a host toolchain runs
# inside THIS image. It carries:
#
#   go            (golangci-lint is a Go analysis driver — it cannot run without
#                  a real Go toolchain and GOROOT, so `go` is a dependency of the
#                  linter, not a convenience)
#   golangci-lint  lint
#   goose          migrations (postgres driver only)
#   sqlc           SQL -> Go codegen
#   oapi-codegen   OpenAPI -> Go codegen
#   kafka-*.sh     the real Apache Kafka CLI, copied from the digest-pinned
#                  broker image so CLI and broker are the same build
#   psql           postgres 17 client, matching timescale/timescaledb:latest-pg17
#   terraform      cluster / kafka topics / grafana (CLAUDE.md §9)
#   helm           THE deploy path. CLAUDE.md §9: "Kubernetes — Helm only… one
#                  chart, with values-dev.yaml and values-prod.yaml. Kustomize is
#                  deliberately not used." The host is allowed exactly one
#                  dependency (a container runtime), so Helm cannot live there.
#   kubectl        Helm needs a working cluster connection, and a rollout that
#                  cannot be inspected (`kubectl rollout status`, `get pods`,
#                  `logs`) is not a deploy path, it is a fire-and-forget. Same
#                  argument as Helm: it may not be a host binary.
#   curl jq git    glue
#
# Deliberately NO ENTRYPOINT: this image is driven as
#   docker run --rm <img> <cmd> ...
# from the Makefile, so the command must be the caller's, never ours.
#
# Every version below is explicit. A floating tool version is the same
# reproducibility hole as a floating base image.
# ---------------------------------------------------------------------------

# --- digest-pinned bases (verbatim from the ledger "Resolved base images") ---
ARG GOLANG_IMAGE=golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83
ARG KAFKA_IMAGE=apache/kafka:latest@sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837

# --- pinned tool versions ---
ARG GOLANGCI_LINT_VERSION=v2.12.2
ARG GOOSE_VERSION=v3.27.3
ARG SQLC_VERSION=v1.31.1
ARG OAPI_CODEGEN_VERSION=v2.8.0
ARG TERRAFORM_VERSION=1.15.8

# Helm 4 is the current stable major (helm/helm `latest` release at pin time).
# The chart under deploy/helm does not exist yet (CLAUDE.md §11, phase 10), so it
# will be authored against this version rather than migrated to it later. Chart
# `apiVersion: v2` renders on both the 3.x and 4.x lines; if phase 10 finds a
# reason to fall back, this ARG plus the two checksums below are the only edit.
ARG HELM_VERSION=v4.2.4
# Matches the kubectl CLAUDE.md §1 records on the author's host, so the container
# and the editor's client never disagree about API deprecations.
ARG KUBECTL_VERSION=v1.36.3

# --- pinned alpine 3.24 package versions (base image is alpine 3.24.1) ---
ARG APK_BASH=5.3.9-r1
ARG APK_CA_CERTIFICATES=20260611-r0
ARG APK_CURL=8.21.0-r0
ARG APK_GIT=2.54.0-r0
ARG APK_JQ=1.8.1-r0
ARG APK_POSTGRESQL_CLIENT=17.11-r0
ARG APK_TAR=1.35-r5
ARG APK_UNZIP=6.0-r16


# ---------------------------------------------------------------------------
# stage: kafka-dist — the Apache Kafka distribution + its musl Temurin JRE.
# Taken from the broker image itself so `kafka-topics.sh` can never drift from
# the broker it talks to. This is the only JVM in the repository and it runs a
# vendor-shipped CLI; CLAUDE.md §2's "no JVM code in this repo" is about source
# we write, and no Java source exists here.
# ---------------------------------------------------------------------------
FROM ${KAFKA_IMAGE} AS kafka-dist


# ---------------------------------------------------------------------------
# stage: go-tools — build the Go-based tooling as static binaries.
# Pinned to BUILDPLATFORM and cross-compiled with GOARCH, so `buildx --platform
# linux/amd64,linux/arm64` (CLAUDE.md §9) does not drag a Go compile through
# QEMU emulation once per architecture.
# ---------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS go-tools

ARG GOLANGCI_LINT_VERSION
ARG GOOSE_VERSION
ARG SQLC_VERSION
ARG OAPI_CODEGEN_VERSION
ARG APK_GIT
# supplied by buildx; default to linux/<host arch> under a plain `docker build`
ARG TARGETOS=linux
ARG TARGETARCH

RUN apk add --no-cache "git=${APK_GIT}"

# CGO_ENABLED=0        — same constraint as the service binaries (CLAUDE.md §9);
#                        all four tools build clean without cgo, which is also
#                        what makes the cross-compile above free.
# GOTOOLCHAIN=local    — never silently download a different Go toolchain, which
#                        would make the build non-reproducible.
# GOBIN is deliberately NOT set: `go install` refuses outright with
# "cannot install cross-compiled binaries when GOBIN is set", so setting it
# would break every non-native buildx platform. Instead the binaries are
# collected from Go's own output directory below, which is /go/bin when
# building natively and /go/bin/<goos>_<goarch> when cross-compiling.
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOPATH=/go \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# goose build tags: this project is Postgres-only (CLAUDE.md §3). Excluding the
# other drivers drops the cgo sqlite driver, which is what would otherwise force
# CGO_ENABLED=1 on this stage.
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go install \
      -tags='no_clickhouse no_libsql no_mssql no_mysql no_sqlite3 no_vertica no_ydb' \
      "github.com/pressly/goose/v3/cmd/goose@${GOOSE_VERSION}"

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go install "github.com/sqlc-dev/sqlc/cmd/sqlc@${SQLC_VERSION}"

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go install "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}"

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"

# Normalise the two possible output directories into one, so the final stage has
# a single stable path to COPY from on every platform.
RUN set -eux; \
    src="/go/bin/${GOOS}_${GOARCH}"; \
    [ -d "${src}" ] || src="/go/bin"; \
    mkdir -p /out/bin; \
    cp "${src}/golangci-lint" "${src}/goose" "${src}/sqlc" "${src}/oapi-codegen" /out/bin/; \
    chmod 0755 /out/bin/*; \
    ls -l /out/bin


# ---------------------------------------------------------------------------
# stage: terraform-dist — fetch + checksum-verify the official release archive.
# Checksums come from releases.hashicorp.com/terraform/${VERSION}/..._SHA256SUMS
# and are pinned here, so a swapped artifact fails the build rather than
# shipping silently.
# ---------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS terraform-dist

ARG TERRAFORM_VERSION
ARG APK_CURL
ARG APK_UNZIP
ARG APK_CA_CERTIFICATES
# TARGETARCH is supplied by buildx (amd64 on the server, arm64 on the dev Mac).
ARG TARGETARCH

RUN apk add --no-cache \
      "curl=${APK_CURL}" \
      "unzip=${APK_UNZIP}" \
      "ca-certificates=${APK_CA_CERTIFICATES}"

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) tf_sha256='d25ce7b6902013ad905db3d2eab0be4cd905887fe88b81a6171b8d5503c31f3d' ;; \
      arm64) tf_sha256='8891e9dcedc9e3b8950bc6af9d4d8af1f4cfade3062f53b9dc403a89f6ce8c9c' ;; \
      *) echo "no pinned terraform checksum for TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    url="https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip"; \
    curl -fsSL --retry 3 -o /tmp/terraform.zip "${url}"; \
    echo "${tf_sha256}  /tmp/terraform.zip" | sha256sum -c -; \
    mkdir -p /out/bin; \
    unzip -q /tmp/terraform.zip -d /out/bin; \
    rm /tmp/terraform.zip; \
    chmod 0755 /out/bin/terraform


# ---------------------------------------------------------------------------
# stage: kube-dist — Helm and kubectl, fetched from their official release
# channels and checksum-verified.
#
# Neither ships as an apk package on Alpine's index, and neither may be
# installed onto the host (prime directive), so they are fetched here and
# frozen by version + SHA-256.
#
# Checksum provenance — both are the vendors' own published sums, not
# hand-computed:
#   helm     https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz.sha256sum
#   kubectl  https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl.sha256
# Bumping either ARG WITHOUT re-fetching the matching sum from those URLs will
# fail the build, which is the entire point: a floating tool version is the same
# reproducibility hole as a floating base image.
# ---------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS kube-dist

ARG HELM_VERSION
ARG KUBECTL_VERSION
ARG APK_CURL
ARG APK_TAR
ARG APK_CA_CERTIFICATES
# TARGETARCH is supplied by buildx (amd64 on the server, arm64 on the dev Mac).
ARG TARGETARCH

RUN apk add --no-cache \
      "curl=${APK_CURL}" \
      "tar=${APK_TAR}" \
      "ca-certificates=${APK_CA_CERTIFICATES}"

# --- helm -------------------------------------------------------------------
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) helm_sha256='c306b46f719b0a4da32d0f78ee21bf90ce8d602f15b22ab753f0674d1670a7f3' ;; \
      arm64) helm_sha256='564de2191b881e9f71b5606b25345821ea1682f06ab90499d3ab22b530176da1' ;; \
      *) echo "no pinned helm checksum for TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    url="https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL --retry 3 -o /tmp/helm.tgz "${url}"; \
    echo "${helm_sha256}  /tmp/helm.tgz" | sha256sum -c -; \
    mkdir -p /out/bin /tmp/helm; \
    tar -xzf /tmp/helm.tgz -C /tmp/helm; \
    cp "/tmp/helm/linux-${TARGETARCH}/helm" /out/bin/helm; \
    rm -rf /tmp/helm /tmp/helm.tgz; \
    chmod 0755 /out/bin/helm

# --- kubectl ----------------------------------------------------------------
# dl.k8s.io publishes the raw binary, not an archive, so the sum is over the
# binary itself.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) kubectl_sha256='ebbd080e7c2e275093b55915722043257eb24004363e20acb3c4d71919f88336' ;; \
      arm64) kubectl_sha256='3d86f24401c41ae5a46ac50eef8865fe891d3647d324a0836f6c63757a126e62' ;; \
      *) echo "no pinned kubectl checksum for TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    url="https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    curl -fsSL --retry 3 -o /out/bin/kubectl "${url}"; \
    echo "${kubectl_sha256}  /out/bin/kubectl" | sha256sum -c -; \
    chmod 0755 /out/bin/kubectl

RUN ls -l /out/bin


# ---------------------------------------------------------------------------
# stage: tools — the image the Makefile actually runs.
# ---------------------------------------------------------------------------
FROM ${GOLANG_IMAGE} AS tools

ARG APK_BASH
ARG APK_CA_CERTIFICATES
ARG APK_CURL
ARG APK_GIT
ARG APK_JQ
ARG APK_POSTGRESQL_CLIENT
ARG APK_TAR
ARG APK_UNZIP

LABEL org.opencontainers.image.title="sharpline-tools" \
      org.opencontainers.image.description="Containerised developer toolchain: go, golangci-lint, goose, sqlc, oapi-codegen, kafka CLI, psql, terraform, helm, kubectl" \
      org.opencontainers.image.source="https://github.com/anpl1623/sharpline" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache \
      "bash=${APK_BASH}" \
      "ca-certificates=${APK_CA_CERTIFICATES}" \
      "curl=${APK_CURL}" \
      "git=${APK_GIT}" \
      "jq=${APK_JQ}" \
      "postgresql17-client=${APK_POSTGRESQL_CLIENT}" \
      "tar=${APK_TAR}" \
      "unzip=${APK_UNZIP}"

# --- Kafka CLI + its JRE, straight from the pinned broker image -------------
COPY --from=kafka-dist /opt/java/openjdk /opt/java/openjdk
COPY --from=kafka-dist /opt/kafka/bin    /opt/kafka/bin
COPY --from=kafka-dist /opt/kafka/libs   /opt/kafka/libs
COPY --from=kafka-dist /opt/kafka/config /opt/kafka/config
COPY --from=kafka-dist /opt/kafka/LICENSE /opt/kafka/NOTICE /opt/kafka/

# --- the Go tooling ---------------------------------------------------------
COPY --from=go-tools /out/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --from=go-tools /out/bin/goose         /usr/local/bin/goose
COPY --from=go-tools /out/bin/sqlc          /usr/local/bin/sqlc
COPY --from=go-tools /out/bin/oapi-codegen  /usr/local/bin/oapi-codegen

# --- terraform --------------------------------------------------------------
COPY --from=terraform-dist /out/bin/terraform /usr/local/bin/terraform

# --- the Kubernetes deploy path (CLAUDE.md §9: "Helm only") ------------------
COPY --from=kube-dist /out/bin/helm    /usr/local/bin/helm
COPY --from=kube-dist /out/bin/kubectl /usr/local/bin/kubectl

ENV JAVA_HOME=/opt/java/openjdk \
    KAFKA_HOME=/opt/kafka \
    PATH=/opt/java/openjdk/bin:/opt/kafka/bin:/usr/local/go/bin:/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

# Every cache a tool might want lives at an absolute path that is world-writable,
# so the image behaves identically whether the Makefile runs it as the baked-in
# non-root user or overrides it with `--user $(id -u):$(id -g)` to keep
# bind-mounted generated files owned by the caller.
ENV HOME=/home/sharpline \
    GOPATH=/go \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/home/sharpline/.cache/go-build \
    GOLANGCI_LINT_CACHE=/home/sharpline/.cache/golangci-lint \
    XDG_CACHE_HOME=/home/sharpline/.cache \
    XDG_CONFIG_HOME=/home/sharpline/.config \
    XDG_DATA_HOME=/home/sharpline/.local/share \
    TF_PLUGIN_CACHE_DIR=/home/sharpline/.terraform.d/plugin-cache \
    HELM_CACHE_HOME=/home/sharpline/.cache/helm \
    HELM_CONFIG_HOME=/home/sharpline/.config/helm \
    HELM_DATA_HOME=/home/sharpline/.local/share/helm \
    KUBECACHEDIR=/home/sharpline/.kube/cache \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0

# Alpine's /etc/profile hard-resets PATH, so a login shell (`sh -lc`, `bash -lc`)
# would lose go/java/kafka. Re-export there too, so the image behaves the same
# however the Makefile chooses to invoke it.
RUN printf '%s\n' \
      'export JAVA_HOME=/opt/java/openjdk' \
      'export KAFKA_HOME=/opt/kafka' \
      'export PATH="/opt/java/openjdk/bin:/opt/kafka/bin:/usr/local/go/bin:/go/bin:$PATH"' \
      > /etc/profile.d/sharpline-tools.sh \
 && chmod 0644 /etc/profile.d/sharpline-tools.sh

RUN addgroup -g 65532 -S sharpline \
 && adduser -u 65532 -S -G sharpline -h /home/sharpline -s /bin/bash sharpline \
 && mkdir -p /workspace \
             /go/pkg/mod \
             /home/sharpline/.cache/go-build \
             /home/sharpline/.cache/golangci-lint \
             /home/sharpline/.cache/helm \
             /home/sharpline/.config \
             /home/sharpline/.config/helm \
             /home/sharpline/.local/share/helm \
             /home/sharpline/.kube/cache \
             /home/sharpline/.terraform.d/plugin-cache \
 && chown -R 65532:65532 /workspace /go /home/sharpline \
 && chmod -R 0777 /workspace /go /home/sharpline \
 && git config --system --add safe.directory /workspace \
 && git config --system --add safe.directory '*'

WORKDIR /workspace
USER 65532:65532

# No ENTRYPOINT — the Makefile supplies the command.
CMD ["bash"]
