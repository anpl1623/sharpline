# =============================================================================
# Sharpline -- container-only Makefile
# =============================================================================
#
# PRIME DIRECTIVE (CLAUDE.md, top of file):
#
#   "Every process in this system runs in a container. The host is allowed
#    exactly one dependency: a container runtime."
#
# Therefore EVERY recipe in this file is a `docker build`, `docker run`,
# `docker buildx` or `docker compose` invocation. There is no host Go, Node,
# npm, psql, goose, terraform or golangci-lint anywhere in this file.
#
# `make verify-no-host-toolchain` mechanically proves that claim by parsing
# this very file. CI runs it. If you add a recipe that shells out to a host
# toolchain, that target fails and the build is red -- which is the whole
# point (CLAUDE.md section 9: "the runner is treated as a bare machine with
# Docker and nothing else").
#
# Two targets -- `help` and `verify-no-host-toolchain` -- use POSIX `awk`,
# which is base-system plumbing present on any machine that already has
# `make`. They only read text out of this file; they never compile, run or
# test project code. Everything that touches project code goes to a container.
#
# WHICH TARGETS USE COMPOSE, AND WHY
#   Anything that needs the stack -- a healthy Postgres, the Kafka broker, the
#   proxy, the shared web node_modules volume -- is wrapped around a service in
#   deploy/compose/compose.tools.yaml, so the wiring lives in exactly one place.
#   Anything that only needs a Go toolchain and the source tree (test, vet,
#   fmt, lint, codegen) is a plain `docker run`, because compose buys it
#   nothing and a direct run lets this file guarantee cache-volume ownership.
#   See `cache-init` for why that guarantee is load-bearing.
#
# Requires: Docker (with Compose v2+ and buildx). Nothing else.
# GNU Make 3.81 compatible (macOS ships 3.81 -- do not use .SHELLFLAGS,
# `file`, `undefine` or other 3.82+/4.x features here).
# =============================================================================

SHELL := /bin/sh
.DEFAULT_GOAL := help

# BuildKit is required for the cache mounts CLAUDE.md section 9 mandates on the
# Go module/build caches. Exported so plain `docker build` uses it too.
export DOCKER_BUILDKIT := 1
export COMPOSE_DOCKER_CLI_BUILD := 1
export BUILDKIT_PROGRESS ?= auto

# -----------------------------------------------------------------------------
# Paths
# -----------------------------------------------------------------------------
MAKEFILE_PATH := $(abspath $(lastword $(MAKEFILE_LIST)))
ROOT_DIR      := $(patsubst %/,%,$(dir $(MAKEFILE_PATH)))

COMPOSE_DIR        := $(ROOT_DIR)/deploy/compose
COMPOSE_FILE       := $(COMPOSE_DIR)/compose.yaml
COMPOSE_DEV_FILE   := $(COMPOSE_DIR)/compose.dev.yaml
COMPOSE_OBS_FILE   := $(COMPOSE_DIR)/compose.obs.yaml
COMPOSE_TOOLS_FILE := $(COMPOSE_DIR)/compose.tools.yaml

DOCKER_DIR     := $(ROOT_DIR)/deploy/docker
GO_DOCKERFILE  := $(DOCKER_DIR)/go.Dockerfile
WEB_DOCKERFILE := $(ROOT_DIR)/web/Dockerfile

# -----------------------------------------------------------------------------
# Identity / images
# -----------------------------------------------------------------------------
PROJECT      ?= sharpline
REGISTRY     ?= ghcr.io/anpl1623
IMAGE_PREFIX := $(REGISTRY)/$(PROJECT)
VERSION      ?= dev

# Local tag deploy/compose/compose.yaml expects (image: sharpline/api:local).
# `make build` tags BOTH names, so one build primes the compose stack and is
# simultaneously pushable to the registry.
LOCAL_IMAGE_PREFIX ?= $(PROJECT)
LOCAL_TAG          ?= local

# OCI label inputs. Both degrade to a constant when the host has no git/date, so
# no build step is ever *coupled* to a host tool. CI overrides them explicitly.
REVISION   ?= $(shell git -C $(ROOT_DIR) rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo 1970-01-01T00:00:00Z)

# The six Go binaries (CLAUDE.md section 3), all built from ONE parameterized
# Dockerfile via --build-arg SERVICE=<name>.
GO_SERVICES := api ingest pricer stream settle migrate

# Base images -- pinned by digest, copied VERBATIM from the ledger's
# "Resolved base images" table (CLAUDE.md section 12: pin by digest, never by
# floating tag). Do not edit a digest by hand; Renovate/Dependabot proposes bumps.
GO_IMAGE     ?= golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83
ALPINE_IMAGE ?= alpine:latest@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Trivy is required by CLAUDE.md section 9 ("trivy image scan") but is NOT in
# the ledger's resolved-digest table. Floating tag until a digest lands.
TRIVY_IMAGE ?= aquasec/trivy:latest # DIGEST-PENDING

# Built from deploy/docker/tools.Dockerfile by the compose `lint` service, so
# there is exactly one build definition for it.
TOOLS_IMAGE ?= $(PROJECT)/tools:local

# Throwaway database engine for `migrate-dry-run`. Deliberately the SAME image and
# the SAME digest as the compose stack's `postgres` service, verbatim from the
# ledger: a stock postgres image has no TimescaleDB, so a phase-2 hypertable
# migration would pass the dry run and then fail against the real stack -- which is
# the single failure mode this target exists to prevent.
POSTGRES_IMAGE ?= timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6

# -----------------------------------------------------------------------------
# Compose wiring
# -----------------------------------------------------------------------------
# No --project-directory is passed, so Compose's project directory is the
# directory of the FIRST -f file (deploy/compose/). Relative build contexts and
# env_file paths inside the compose files resolve against deploy/compose/ --
# which is what they already assume (`context: ../..`).
# DEFERRED TO RECIPE TIME ON PURPOSE -- note the `=`, not `:=`.
#
# `up`, `up-dev` and `up-obs` all depend on `env`, whose recipe CREATES .env from
# .env.example on a fresh clone. A parse-time test would have already decided the
# file was absent, compose would run with no --env-file, and none of .env's values
# would apply on the very first `make up` -- which is exactly the first-run
# experience a reviewer gets.
#
# `$(shell test -f ...)` rather than `$(wildcard ...)`: GNU Make caches directory
# listings, so a file created during this same run can stay invisible to
# `$(wildcard)` even when the expansion itself is deferred. A shell stat cannot be
# fooled that way.
#
# Everything downstream of this is therefore recursively expanded too (`=`), or the
# parse-time value would be baked straight back in.
COMPOSE_ENV_ARG = $(shell test -f '$(ROOT_DIR)/.env' && printf '%s %s' --env-file '$(ROOT_DIR)/.env')

COMPOSE_BASE  = docker compose --project-name $(PROJECT) $(COMPOSE_ENV_ARG)
COMPOSE       = $(COMPOSE_BASE) -f $(COMPOSE_FILE)
# kafka-ui carries `profiles: [dev]` in compose.yaml, so the dev overlay must
# enable that profile or the Kafka inspector never starts -- CLAUDE.md section 9
# calls it "non-negotiable while learning Kafka".
COMPOSE_DEV   = $(COMPOSE) -f $(COMPOSE_DEV_FILE) --profile dev
COMPOSE_OBS   = $(COMPOSE) -f $(COMPOSE_OBS_FILE) --profile obs
# compose.tools.yaml services are all profile-gated; `docker compose run`
# auto-enables the profile of the service it is given, so no --profile needed.
COMPOSE_TOOLS = $(COMPOSE) -f $(COMPOSE_TOOLS_FILE)

# down/ps/logs/restart should see every overlay that exists on disk and every
# profile, so nothing is left orphaned. $(wildcard) keeps this working even if
# an overlay is absent.
COMPOSE_OVERLAYS := $(addprefix -f ,$(wildcard $(COMPOSE_DEV_FILE) $(COMPOSE_OBS_FILE) $(COMPOSE_TOOLS_FILE)))
COMPOSE_ALL       = $(COMPOSE) $(COMPOSE_OVERLAYS) --profile "*"

# -----------------------------------------------------------------------------
# Container run scaffolding
# -----------------------------------------------------------------------------
# Targets that WRITE INTO THE REPO (fmt, codegen, migrate-create) run as the
# invoking host user so generated files are not root-owned on Linux. Targets
# that only read, or that need the Docker socket, run as the image's own user.
DOCKER_USER ?= $(shell id -u):$(shell id -g)
DOCKER_AS_USER := --user $(DOCKER_USER)

GOMOD_VOLUME       ?= $(PROJECT)-go-mod-cache
GOBUILD_VOLUME     ?= $(PROJECT)-go-build-cache
GOBIN_VOLUME       ?= $(PROJECT)-go-bin
TOOLS_CACHE_VOLUME ?= $(PROJECT)-tools-cache

CACHE_VOLUMES := $(GOMOD_VOLUME) $(GOBUILD_VOLUME) $(GOBIN_VOLUME) $(TOOLS_CACHE_VOLUME) $(PROJECT)-trivy-cache

# Mounts/env for any container running the Go toolchain from the golang image.
# CGO_ENABLED=0 everywhere (CLAUDE.md section 9) except the race detector.
# The volume is mounted at /go/pkg, NOT /go/pkg/mod, because the module cache is
# not the only thing cmd/go writes under there: the checksum-database tile cache
# lives at $GOPATH/pkg/sumdb, and a non-root uid cannot create it inside a
# root-owned /go/pkg. Mounting the parent covers mod/ and sumdb/ together.
DOCKER_GO_FLAGS := \
	-v $(ROOT_DIR):/src -w /src \
	-v $(GOMOD_VOLUME):/go/pkg \
	-v $(GOBUILD_VOLUME):/gocache \
	-v $(GOBIN_VOLUME):/go/bin \
	-e HOME=/tmp \
	-e GOPATH=/go \
	-e GOMODCACHE=/go/pkg/mod \
	-e GOCACHE=/gocache \
	-e GOFLAGS=-buildvcs=false \
	-e CGO_ENABLED=0

# Mounts for the tools image. Volumes are mounted at the paths tools.Dockerfile
# already creates and chmods 0777, so the cache is writable no matter which uid
# the container runs as -- and no GOCACHE/GOMODCACHE override is needed, because
# the image's own ENV already points at exactly these paths.
DOCKER_TOOLS_FLAGS := \
	-v $(ROOT_DIR):/workspace -w /workspace \
	-v $(GOMOD_VOLUME):/go/pkg \
	-v $(TOOLS_CACHE_VOLUME):/home/sharpline/.cache

# Docker socket, mounted into the test container so testcontainers-go can spawn
# SIBLING containers on the host daemon (CLAUDE.md section 10). Those siblings
# publish ephemeral ports on the HOST, not inside the test container, so
# testcontainers must dial the host gateway rather than 127.0.0.1.
#
# The socket inside the container is root:root mode 0660 (verified), so the
# test container deliberately runs as root -- a non-root uid cannot open it.
#
# THE PATH IS DISCOVERED, NEVER ASSUMED. Docker Desktop's "Allow the default Docker
# socket to be used" setting is off on some machines; there the only socket is
# ~/.docker/run/docker.sock and /var/run/docker.sock does not exist at all. That
# matters more than it sounds: Docker silently creates a DIRECTORY at a missing bind
# source, so a hardcoded path would hand testcontainers-go a directory where it
# expects a socket, and the failure surfaces as an unrelated-looking dial error.
#
# Order: the endpoint of the CURRENT docker context (which is also what honours
# DOCKER_HOST), then /var/run/docker.sock. Both are validated with `-S`, so a stale
# context entry pointing at a deleted socket falls through instead of being trusted.
# An empty result is not silently tolerated -- `make docker-socket` fails loudly, and
# every target that mounts the socket depends on it.
#
# Resolved ONCE per make invocation (`:=` inside the origin guard) rather than on
# every expansion, because it is EXPORTED: deploy/compose/compose.tools.yaml
# interpolates ${DOCKER_SOCKET:-/var/run/docker.sock} for its `test` and `terraform`
# services, and compose can only see it if it is in the environment. A recursively
# expanded export would re-probe for every recipe line. The `origin` guard keeps an
# explicit `make test DOCKER_SOCKET=...` or an exported value from the environment
# authoritative -- neither is "undefined", so neither gets overwritten.
#
# `docker context inspect` reads local context metadata, so this costs no daemon
# round trip and works even when the daemon is down.
ifeq ($(origin DOCKER_SOCKET), undefined)
DOCKER_SOCKET := $(shell \
    s=$$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null | sed -n 's|^unix://||p' | head -n 1); \
    if [ -n "$$s" ] && [ -S "$$s" ]; then printf '%s' "$$s"; \
    elif [ -S /var/run/docker.sock ]; then printf '%s' /var/run/docker.sock; \
    fi)
endif
export DOCKER_SOCKET

RYUK_DISABLED ?= false
DOCKER_TESTCONTAINERS_FLAGS = \
	-v $(DOCKER_SOCKET):/var/run/docker.sock \
	--add-host host.docker.internal:host-gateway \
	-e DOCKER_HOST=unix:///var/run/docker.sock \
	-e TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
	-e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal \
	-e TESTCONTAINERS_RYUK_DISABLED=$(RYUK_DISABLED)

# -----------------------------------------------------------------------------
# Tunables
# -----------------------------------------------------------------------------
PKG        ?= ./...
SERVICE    ?=
TAIL       ?= 200
ARGS       ?=
NAME       ?=
KAFKA_TOPIC ?= odds.normalized
GOVULNCHECK_VERSION ?= latest

# `migrate-dry-run` scaffolding. Nothing here touches the compose project: the
# network and the container are created and destroyed by the target itself, so a
# dry run can never reach a real database, and running it does not disturb a stack
# that happens to be up. Credentials are throwaway by construction -- this database
# exists for a few seconds and is destroyed with its volume.
MIGRATE_DRYRUN_NET      ?= $(PROJECT)-migrate-dryrun-net
MIGRATE_DRYRUN_PG       ?= $(PROJECT)-migrate-dryrun-pg
MIGRATE_DRYRUN_USER     ?= sharpline
MIGRATE_DRYRUN_PASSWORD ?= throwaway-dry-run
MIGRATE_DRYRUN_DB       ?= sharpline_dryrun
MIGRATE_DRYRUN_WAIT     ?= 90
PLATFORMS  ?= linux/amd64,linux/arm64
BUILDX_BUILDER ?= $(PROJECT)-builder
BUILDX_OUTPUT ?=

# Registry-backed BuildKit cache. CLAUDE.md section 9: "Registry-backed BuildKit cache
# keeps that affordable" -- without it every CI build starts cold, because a GitHub
# runner is a fresh machine with an empty local BuildKit cache.
#
# CI exports these (see .github/workflows/ci.yml, the `meta` job):
#   BUILDX_CACHE_FROM=type=registry,ref=ghcr.io/<owner>/<project>/buildcache
#   BUILDX_CACHE_TO=<the same>,mode=max,compression=zstd
# and deliberately leaves them EMPTY on a fork PR, which has no write access to GHCR
# and would otherwise fail on a 401.
#
# So they are consumed conditionally: unset or empty emits no flags at all, and a
# laptop build needs no registry, no login, and no network. This is why the value is
# `$(if $(strip ...))` rather than a bare `--cache-from $(VAR)` -- buildx rejects an
# empty flag argument outright.
BUILDX_CACHE_FROM ?=
BUILDX_CACHE_TO   ?=
BUILDX_CACHE_ARGS  = $(if $(strip $(BUILDX_CACHE_FROM)),--cache-from $(strip $(BUILDX_CACHE_FROM))) \
                     $(if $(strip $(BUILDX_CACHE_TO)),--cache-to $(strip $(BUILDX_CACHE_TO)))
SCAN_TARGET ?= $(IMAGE_PREFIX)/api:$(VERSION)
SCAN_SEVERITY ?= HIGH,CRITICAL

# Exported so `docker compose` interpolation in compose.tools.yaml can see them.
export TF_ENV ?= local

# Locust runs HEADLESS on purpose: CLAUDE.md section 12 says nothing binds to a
# host port except the proxy, so there is no Locust web UI published here.
LOCUST_WORKERS ?= 4
export LOCUST_USERS          ?= 1000
export LOCUST_SPAWN_RATE     ?= 100
export LOCUST_RUN_TIME       ?= 5m
export LOCUST_EXPECT_WORKERS := $(LOCUST_WORKERS)

# Words that must never be invoked by a recipe outside a docker invocation.
# Kept in a variable so the literal words do NOT appear inside any recipe line
# of this file -- otherwise the checker would flag itself.
#
# helm, kubectl, sqlc and oapi-codegen are on the list for the same reason as the
# rest: the entire Kubernetes deploy path (CLAUDE.md section 9, "Kubernetes -- Helm
# only") and the whole of codegen run inside the tools image, and a recipe that
# quietly shelled out to a host binary instead would pass CI on a developer laptop
# that happens to have one installed and fail on the bare runner.
#
# Note on the word boundary the checker uses: `-` and `/` do NOT terminate a match,
# so a PATH fragment like deploy/helm/Chart.yaml is not a hit -- only a bare command
# word is. That is deliberate; paths naming a tool are not invocations of it.
HOST_TOOLCHAIN_CMDS := go|npm|npx|node|psql|goose|terraform|golangci-lint|pnpm|yarn|helm|kubectl|sqlc|oapi-codegen

# Anchored on the `uses:` key so that YAML *comments* naming these actions
# (a CI file is allowed to document what it must not do) are not false hits.
CI_FORBIDDEN_ACTIONS := ^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*(actions/setup-(go|node|python|java|dotnet)|hashicorp/setup-terraform)

# =============================================================================
##@ Help
# =============================================================================

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { \
	    FS = ":.*##"; \
	    printf "\nSharpline -- every target below runs in a container.\n"; \
	    printf "Host requirement: Docker. Nothing else.\n\n"; \
	    printf "Usage: make <target> [VAR=value]\n"; \
	  } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next } \
	  /^[a-zA-Z0-9_%.-]+:.*##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 } \
	  END { printf "\n"; }' $(MAKEFILE_PATH)

.PHONY: doctor
doctor: ## Verify the one and only host dependency (the container runtime) is alive
	docker version
	docker compose version
	docker buildx version
	docker info --format 'server: {{.ServerVersion}}  os/arch: {{.OSType}}/{{.Architecture}}  cpus: {{.NCPU}}'
	@$(MAKE) --no-print-directory docker-socket

# Guard for every target that bind-mounts the Docker socket (test, test-race, cover,
# scan). See DOCKER_SOCKET above for why the path is discovered rather than assumed.
# Failing here, with the path printed, is the whole point: the alternative is Docker
# creating a directory at the missing bind source and testcontainers-go failing
# several minutes later with an error that names neither the socket nor this file.
.PHONY: docker-socket
docker-socket: ## Resolve and validate the Docker socket the test containers mount
	@sock='$(DOCKER_SOCKET)'; \
	 if [ -z "$$sock" ]; then \
	   printf 'FAIL  no Docker socket found.\n'; \
	   printf '      Looked at: the current docker context endpoint, then /var/run/docker.sock.\n'; \
	   printf '      Start the container runtime, or name the socket explicitly:\n'; \
	   printf '        make test DOCKER_SOCKET="$$HOME/.docker/run/docker.sock"\n'; \
	   exit 1; \
	 fi; \
	 if [ ! -S "$$sock" ]; then \
	   printf 'FAIL  DOCKER_SOCKET=%s is not a socket.\n' "$$sock"; \
	   printf '      A bind mount of a missing path silently becomes a DIRECTORY, which\n'; \
	   printf '      testcontainers-go cannot dial. Refusing to mount it.\n'; \
	   exit 1; \
	 fi; \
	 printf 'OK  docker socket: %s\n' "$$sock"

# =============================================================================
##@ Prime-directive enforcement
# =============================================================================

# How the check works: recipe lines (those beginning with a TAB) are joined
# across backslash continuations, then truncated at the first `docker` word or
# `$(COMPOSE*)` reference. Whatever remains BEFORE that point must not invoke a
# host toolchain. So `docker run img go test ./...` passes and
# `goose up && docker run ...` fails, which is the distinction that matters.
#
# Known limitation, stated rather than hidden: it scans recipe lines only. A
# command smuggled into a variable (`X := <toolchain cmd>` used as `\t$(X)`)
# is not detected. Closing that would require expanding every variable, which
# costs more false positives than it buys. CI is the real backstop: the runner
# has no toolchain, so such a recipe fails there regardless.
.PHONY: verify-no-host-toolchain
verify-no-host-toolchain: ## Fail if any recipe (or CI job) uses a host toolchain
	@awk -v forbidden="$(HOST_TOOLCHAIN_CMDS)" ' \
	  function flush(   pos, prefix) { \
	    if (line == "") return; \
	    sub(/^[@+-]+[ \t]*/, "", line); \
	    if (line == "" || line ~ /^#/) { line = ""; return } \
	    pos = 0; \
	    if (match(line, "(^|[^A-Za-z0-9_./-])docker([^A-Za-z0-9_./-]|$$)")) pos = RSTART; \
	    if (match(line, "[$$]\\(COMPOSE[A-Z0-9_]*\\)")) { if (pos == 0 || RSTART < pos) pos = RSTART } \
	    prefix = (pos > 0) ? substr(line, 1, pos - 1) : line; \
	    if (prefix ~ "(^|[^A-Za-z0-9_./-])(" forbidden ")([^A-Za-z0-9_./-]|$$)") { \
	      printf "  VIOLATION %s:%d: %s\n", FILENAME, start, line; \
	      bad++; \
	    } \
	    line = ""; \
	  } \
	  /^\t/ { \
	    seg = $$0; \
	    sub(/^[\t ]+/, "", seg); \
	    if (line == "") start = FNR; \
	    cont = (seg ~ /\\$$/); \
	    if (cont) sub(/\\$$/, " ", seg); \
	    line = line seg; \
	    if (!cont) flush(); \
	    next; \
	  } \
	  { flush() } \
	  END { \
	    flush(); \
	    if (bad > 0) { printf "\nFAIL: %d recipe line(s) invoke a host toolchain outside docker.\n", bad; exit 1 } \
	    printf "OK  Makefile: every recipe is a docker invocation.\n"; \
	  }' $(MAKEFILE_PATH)
	@if [ -d "$(ROOT_DIR)/.github/workflows" ]; then \
	  if grep -REn '$(CI_FORBIDDEN_ACTIONS)' "$(ROOT_DIR)/.github/workflows" ; then \
	    printf "\nFAIL: CI installs a host toolchain (CLAUDE.md section 9 forbids it).\n"; \
	    exit 1; \
	  fi; \
	  printf "OK  CI: no host-toolchain setup actions in .github/workflows.\n"; \
	else \
	  printf "SKIP CI: .github/workflows not present yet.\n"; \
	fi

# =============================================================================
##@ Stack lifecycle
# =============================================================================

.PHONY: env
env: ## Create .env from .env.example if it does not exist yet
	@if [ ! -f "$(ROOT_DIR)/.env" ] && [ -f "$(ROOT_DIR)/.env.example" ]; then \
	  cp "$(ROOT_DIR)/.env.example" "$(ROOT_DIR)/.env"; \
	  printf "created .env from .env.example -- review the secrets before use\n"; \
	fi

.PHONY: up
up: env ## Bring up the full stack (proxy is the only published port)
	$(COMPOSE) up --detach --build --remove-orphans
	$(COMPOSE) ps

.PHONY: up-dev
up-dev: env ## Bring up the stack with hot-reload overrides + kafka-ui (dev profile)
	$(COMPOSE_DEV) up --detach --build --remove-orphans
	$(COMPOSE_DEV) ps

.PHONY: up-obs
up-obs: env ## Bring up the stack plus observability (otel/prometheus/grafana/jaeger)
	$(COMPOSE_OBS) up --detach --build --remove-orphans
	$(COMPOSE_OBS) ps

.PHONY: down
down: ## Stop and remove containers (volumes survive)
	$(COMPOSE_ALL) down --remove-orphans

.PHONY: down-hard
down-hard: ## Stop and remove containers AND named volumes -- the reset button
	$(COMPOSE_ALL) down --volumes --remove-orphans

.PHONY: down-v
down-v: down-hard ## Alias for down-hard (docker compose down -v)

.PHONY: ps
ps: ## Show stack status
	$(COMPOSE_ALL) ps

.PHONY: logs
logs: ## Tail logs (all services, or SERVICE=api)
	$(COMPOSE_ALL) logs --follow --tail=$(TAIL) $(SERVICE)

.PHONY: restart
restart: ## Restart all services, or SERVICE=api
	$(COMPOSE_ALL) restart $(SERVICE)

.PHONY: sh
sh: ## Open a shell in a RUNNING container (SERVICE=postgres)
	$(COMPOSE_ALL) exec $(SERVICE) sh

# =============================================================================
##@ Build
# =============================================================================

.PHONY: build
build: $(addprefix build-,$(GO_SERVICES)) ## Build all six Go service images

# Deliberately NOT .PHONY: GNU Make skips implicit/pattern-rule search for any
# target named in .PHONY, so declaring build-api phony would make this pattern
# never match ("Nothing to be done for build-api"). No file named build-* is
# ever created, so the pattern rule always fires anyway.
build-%: ## Build ONE service image: build-api|ingest|pricer|stream|settle|migrate
	docker build \
	  --file $(GO_DOCKERFILE) \
	  --build-arg SERVICE=$* \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(REVISION) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  --tag $(IMAGE_PREFIX)/$*:$(VERSION) \
	  --tag $(LOCAL_IMAGE_PREFIX)/$*:$(LOCAL_TAG) \
	  $(ROOT_DIR)

.PHONY: build-web
build-web: ## Build the production Next.js image (standalone output, non-root)
	docker build \
	  --file $(WEB_DOCKERFILE) \
	  --build-arg VERSION=$(VERSION) \
	  --tag $(IMAGE_PREFIX)/web:$(VERSION) \
	  --tag $(LOCAL_IMAGE_PREFIX)/web:$(LOCAL_TAG) \
	  $(ROOT_DIR)/web

.PHONY: build-tools
build-tools: ## Build the tooling image (one build definition, shared by every tool target)
	$(COMPOSE_TOOLS) build lint

.PHONY: build-all
build-all: build build-web build-tools ## Build every image in the repo

.PHONY: compose-build
compose-build: ## Build every image the compose stack declares
	$(COMPOSE_ALL) build

.PHONY: buildx-setup
buildx-setup: ## Create the multi-arch buildx builder if it is missing
	docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
	  docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --bootstrap

.PHONY: buildx-multiarch
buildx-multiarch: buildx-setup ## Multi-arch build of all six services (arm64 Mac + amd64 server)
	for svc in $(GO_SERVICES); do \
	  docker buildx build \
	    --builder $(BUILDX_BUILDER) \
	    --platform $(PLATFORMS) \
	    --file $(GO_DOCKERFILE) \
	    --build-arg SERVICE=$$svc \
	    --build-arg VERSION=$(VERSION) \
	    --build-arg REVISION=$(REVISION) \
	    --build-arg BUILD_DATE=$(BUILD_DATE) \
	    --tag $(IMAGE_PREFIX)/$$svc:$(VERSION) \
	    $(BUILDX_CACHE_ARGS) \
	    $(BUILDX_OUTPUT) \
	    $(ROOT_DIR) || exit 1; \
	done

.PHONY: buildx-push
buildx-push: ## Multi-arch build AND push to the registry
	$(MAKE) buildx-multiarch BUILDX_OUTPUT=--push

.PHONY: buildx-web
buildx-web: buildx-setup ## Multi-arch build of the web image
	docker buildx build \
	  --builder $(BUILDX_BUILDER) \
	  --platform $(PLATFORMS) \
	  --file $(WEB_DOCKERFILE) \
	  --build-arg VERSION=$(VERSION) \
	  --tag $(IMAGE_PREFIX)/web:$(VERSION) \
	  $(BUILDX_CACHE_ARGS) \
	  $(BUILDX_OUTPUT) \
	  $(ROOT_DIR)/web

# =============================================================================
##@ Migrations
# =============================================================================

.PHONY: migrate
migrate: ## Run migrations to head via the real migrate container (never a host binary)
	$(COMPOSE) run --rm migrate

.PHONY: migrate-status
migrate-status: ## Show migration status (ad-hoc goose control)
	$(COMPOSE_TOOLS) run --rm goose goose status

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration (ad-hoc goose control)
	$(COMPOSE_TOOLS) run --rm goose goose down

.PHONY: migrate-create
migrate-create: ## Scaffold a new SQL migration (NAME=add_prices_hypertable)
	@if [ -z "$(NAME)" ]; then printf "usage: make migrate-create NAME=add_prices_hypertable\n"; exit 2; fi
	$(COMPOSE_TOOLS) run --rm --no-deps $(DOCKER_AS_USER) goose goose create $(NAME) sql

# CI runs this on every PR (.github/workflows/ci.yml, job `migrate-dry-run`).
#
# What it proves, against a database created seconds earlier and destroyed seconds
# later: every migration PARSES, every migration APPLIES, every migration is
# REVERSIBLE (up -> down-to 0 -> up, which is CLAUDE.md section 12's "forward-only,
# and every one is reversible in review"), and the schema version the database ends
# on matches the number of migration files on disk. That last check is read back out
# of goose_db_version with psql rather than scraped from goose's log output.
#
# ON AN EMPTY MIGRATION SET -- which is the state of this repo until phase 2 -- the
# run still happens for real. It is NOT skipped: the throwaway database is still
# started, goose still connects to it, still creates its goose_db_version bookkeeping
# table, and still reports the resulting schema version, which is then asserted to
# hold exactly zero applied migrations. Zero applied out of zero on disk is a pass.
# The one thing that cannot run is `goose up` itself: goose exits 1 with "no
# migration files found" on an empty directory, so an unconditional `up` would make
# a correct repository state look like a broken one.
#
# The repo is mounted READ-ONLY and the migrations are copied into the container's
# own /tmp before goose sees them. Two reasons: the dry run must not be able to
# mutate the working tree, and migrations/ does not exist yet -- Docker silently
# creates a DIRECTORY at a missing bind source, so mounting it directly would litter
# the repo with an empty directory on a fresh clone.
.PHONY: migrate-dry-run
migrate-dry-run: build-tools ## Apply + roll back every migration on a THROWAWAY database, then destroy it
	@printf '\n==> migration dry-run  (throwaway database; no real data plane is touched)\n'
	@docker rm --force --volumes $(MIGRATE_DRYRUN_PG) >/dev/null 2>&1 || true; \
	 docker network rm $(MIGRATE_DRYRUN_NET) >/dev/null 2>&1 || true; \
	 cleanup() { \
	   docker rm --force --volumes $(MIGRATE_DRYRUN_PG) >/dev/null 2>&1 || true; \
	   docker network rm $(MIGRATE_DRYRUN_NET) >/dev/null 2>&1 || true; \
	 }; \
	 trap cleanup EXIT HUP INT TERM; \
	 set -e; \
	 docker network create $(MIGRATE_DRYRUN_NET) >/dev/null; \
	 docker run --detach \
	   --name $(MIGRATE_DRYRUN_PG) \
	   --network $(MIGRATE_DRYRUN_NET) \
	   --env POSTGRES_USER=$(MIGRATE_DRYRUN_USER) \
	   --env POSTGRES_PASSWORD=$(MIGRATE_DRYRUN_PASSWORD) \
	   --env POSTGRES_DB=$(MIGRATE_DRYRUN_DB) \
	   $(POSTGRES_IMAGE) >/dev/null; \
	 printf '    waiting for the throwaway database to accept TCP connections\n'; \
	 waited=0; \
	 until docker exec $(MIGRATE_DRYRUN_PG) pg_isready --quiet --host=127.0.0.1 --username=$(MIGRATE_DRYRUN_USER) --dbname=$(MIGRATE_DRYRUN_DB); do \
	   waited=$$((waited + 1)); \
	   if [ $$waited -ge $(MIGRATE_DRYRUN_WAIT) ]; then \
	     printf 'FAIL  throwaway database never became ready in %ss. Last log lines:\n' '$(MIGRATE_DRYRUN_WAIT)'; \
	     docker logs --tail 40 $(MIGRATE_DRYRUN_PG) || true; \
	     exit 1; \
	   fi; \
	   sleep 1; \
	 done; \
	 printf '    ready after %ss\n' "$$waited"; \
	 docker run --rm \
	   --network $(MIGRATE_DRYRUN_NET) \
	   --volume $(ROOT_DIR):/workspace:ro \
	   --workdir /workspace \
	   --env GOOSE_DRIVER=postgres \
	   --env 'GOOSE_DBSTRING=postgres://$(MIGRATE_DRYRUN_USER):$(MIGRATE_DRYRUN_PASSWORD)@$(MIGRATE_DRYRUN_PG):5432/$(MIGRATE_DRYRUN_DB)?sslmode=disable' \
	   --env PGHOST=$(MIGRATE_DRYRUN_PG) \
	   --env PGUSER=$(MIGRATE_DRYRUN_USER) \
	   --env PGPASSWORD=$(MIGRATE_DRYRUN_PASSWORD) \
	   --env PGDATABASE=$(MIGRATE_DRYRUN_DB) \
	   $(TOOLS_IMAGE) \
	   sh -eu -c ' \
	     mkdir -p /tmp/dryrun; \
	     if [ -d /workspace/migrations ]; then \
	       cp /workspace/migrations/*.sql /tmp/dryrun/ 2>/dev/null || true; \
	     fi; \
	     found=$$(find /tmp/dryrun -maxdepth 1 -name "*.sql" | wc -l | tr -d " "); \
	     printf "    migration files on disk: %s\n" "$$found"; \
	     if [ "$$found" -gt 0 ]; then \
	       printf "    -- status (before)\n"; goose -dir /tmp/dryrun status; \
	       printf "    -- up\n";              goose -dir /tmp/dryrun up; \
	       printf "    -- down-to 0 (reversibility)\n"; goose -dir /tmp/dryrun down-to 0; \
	       printf "    -- up (again)\n";      goose -dir /tmp/dryrun up; \
	       printf "    -- status (after)\n";  goose -dir /tmp/dryrun status; \
	     else \
	       printf "    zero migrations on disk -- applying zero, which is the correct\n"; \
	       printf "    result until phase 2 introduces migrations/ (CLAUDE.md section 11)\n"; \
	     fi; \
	     goose -dir /tmp/dryrun version; \
	     applied=$$(psql --no-align --tuples-only --command "select count(*) from goose_db_version where version_id > 0"); \
	     printf "    applied according to the database: %s   expected: %s\n" "$$applied" "$$found"; \
	     if [ "$$applied" != "$$found" ]; then \
	       printf "FAIL  the database disagrees with the migration set on disk.\n"; \
	       exit 1; \
	     fi; \
	   '; \
	 printf 'OK  migration dry-run passed; throwaway database destroyed.\n'

# =============================================================================
##@ Test
# =============================================================================

.PHONY: test
test: cache-init docker-socket ## Run the Go suite in a container (docker socket mounted for testcontainers)
	docker run --rm \
	  $(DOCKER_GO_FLAGS) \
	  $(DOCKER_TESTCONTAINERS_FLAGS) \
	  $(GO_IMAGE) \
	  go test -count=1 $(PKG)

.PHONY: test-race
test-race: cache-init docker-socket ## Run the suite under the race detector (needs CGO + a C toolchain)
	docker run --rm \
	  $(DOCKER_GO_FLAGS) \
	  $(DOCKER_TESTCONTAINERS_FLAGS) \
	  -e CGO_ENABLED=1 \
	  $(GO_IMAGE) \
	  sh -c 'apk add --no-cache gcc musl-dev >/dev/null && go test -race -count=1 $(PKG)'

# Coverage thresholds, from CLAUDE.md section 10: "Coverage target 80% overall, and
# effectively 100% on internal/domain/odds." They are ENFORCED here, not merely
# printed -- a number nobody fails on is a number nobody reads. All three are
# overridable from the command line so a phase in progress can watch the figure move:
#
#   make cover COVER_MIN_TOTAL=0 COVER_MAX_ODDS_UNCOVERED=999
#
# `go tool cover -func` reports per FUNCTION and one grand TOTAL; it has no notion of
# a package, so the per-package rollup below is computed from the profile directly.
# Package = the directory part of each block's file path, and statement counts are
# summed exactly the way cmd/cover does it, so these figures agree with the
# `coverage: NN.N% of statements` line `go test` prints per package.
#
# ---------------------------------------------------------------------------
# Why the odds gate counts BLOCKS rather than demanding a percentage
# ---------------------------------------------------------------------------
#
# This gate used to read COVER_MIN_ODDS = 100, and it could not be met. Not because
# the package is under-tested -- every reachable statement in it is covered -- but
# because a literal 100% is unattainable while `if err != nil { return err }` remains
# the language's error idiom. Several calls inside this package are made behind a
# guard that has already excluded every input the callee can fail on; the error check
# after them is therefore dead, and it cannot be deleted, because deleting it means
# discarding an error, which is both worse code and a lint failure.
#
# An unmeetable gate is worse than no gate: it goes red on every run, everyone learns
# to ignore it, and a real regression arrives inside the noise. So the gate is stated
# in the unit that can actually be held constant -- the NUMBER of uncovered blocks.
# Adding covered code does not move it. Adding an uncovered block does, and fails the
# build. Covering one is expected to be accompanied by lowering the budget.
#
# The budget was 34 at the start of the phase-1 close and is 18 now. Sixteen of the
# sixteen removed were either covered by a new test or restructured away where the
# branch was genuinely redundant -- see convert.go's Decimal.American and its
# continued-fraction loop, the consolidation of the two ad-hoc "odds:" prefix
# helpers onto errors.go's `unprefixed`, and CorrelationMatrix.permute, which is the
# unchecked internal sibling of Submatrix for the call sites that generate their own
# index lists.
#
# Two blocks were ADDED to the budget, and the reason is worth stating because it
# reads backwards. They are the ErrOrthantNotConverged propagations in
# correlation.go's orthantByLattice and parlay.go's GaussianCopulaJoint. They used to
# be covered -- by a test asserting that a parlay on the edge of the positive
# semi-definite region CANNOT be priced. That refusal turned out to be a defect
# rather than a documented limitation: the property suite drew an ordinary three-leg
# same-game shape on that same edge, and the system declined to quote it. Raising
# orthantMaxBatches fixed it, and the fix removed the only input anyone has found
# that reaches those two propagations. The behaviour they guard -- refusing rather
# than returning an unconverged estimate -- is still pinned directly and
# unconditionally on latticeEstimate itself, against a tolerance no budget can meet.
# So the number rose because the system got better, which is the one reason a rise is
# not a warning sign.
#
# The 18 that remain are printed in full by this target on every run. In summary:
# 6 in devig.go (root-solver and bracket failures on objectives whose brackets are
# constructed to be valid, and a sum guard on values already bounded per-element),
# 10 in parlay.go and 1 in correlation.go (quantile, bivariate, quadrature and
# submatrix failures on arguments a caller-facing validator has already accepted),
# and 1 in vig.go, already annotated as unreachable-but-kept in the source.
#
# A rise for any OTHER reason is a design signal, not a routine edit.
COVER_MIN_TOTAL          ?= 80
COVER_MAX_ODDS_UNCOVERED ?= 18
COVER_ODDS_PKG           ?= github.com/anpl1623/sharpline/internal/domain/odds

.PHONY: cover
cover: cache-init docker-socket ## Coverage: per-package rollup + HTML, gated at 80% overall and on domain/odds' uncovered-block budget
	docker run --rm \
	  $(DOCKER_GO_FLAGS) \
	  $(DOCKER_TESTCONTAINERS_FLAGS) \
	  $(GO_IMAGE) \
	  go test -count=1 -covermode=atomic -coverprofile=coverage.out $(PKG)
	docker run --rm $(DOCKER_GO_FLAGS) $(GO_IMAGE) go tool cover -func=coverage.out
	docker run --rm $(DOCKER_GO_FLAGS) $(GO_IMAGE) go tool cover -html=coverage.out -o coverage.html
	@docker run --rm $(DOCKER_GO_FLAGS) $(GO_IMAGE) awk \
	  -v minTotal='$(COVER_MIN_TOTAL)' -v maxOddsGap='$(COVER_MAX_ODDS_UNCOVERED)' -v oddsPkg='$(COVER_ODDS_PKG)' ' \
	  /^mode:/ { next } \
	  { \
	    file = $$1; sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$$/, "", file); \
	    pkg = file; sub(/\/[^\/]*$$/, "", pkg); \
	    if (!(pkg in seen)) { seen[pkg] = 1; order[++n] = pkg } \
	    ns = $$2 + 0; hit = (($$3 + 0) > 0); \
	    stmt[pkg] += ns; total += ns; \
	    if (hit) { cov[pkg] += ns; covered += ns } \
	    else if (pkg == oddsPkg) { gap[++g] = $$1 } \
	  } \
	  END { \
	    printf "\ncoverage by package (statements)\n"; \
	    printf "  ------------------------------------------------------------------------\n"; \
	    for (i = 1; i <= n; i++) { \
	      p = order[i]; \
	      printf "  %7.2f%%  %6d/%-6d  %s\n", (stmt[p] ? 100 * cov[p] / stmt[p] : 0), cov[p], stmt[p], p; \
	    } \
	    printf "  ------------------------------------------------------------------------\n"; \
	    printf "  %7.2f%%  %6d/%-6d  TOTAL\n\n", (total ? 100 * covered / total : 0), covered, total; \
	    bad = 0; \
	    totalPct = (total ? 100 * covered / total : 0); \
	    if (totalPct < minTotal - 1e-9) { \
	      printf "FAIL  overall coverage %.2f%% is below the %s%% floor (CLAUDE.md section 10).\n", totalPct, minTotal; \
	      bad = 1; \
	    } else { \
	      printf "OK    overall coverage %.2f%% meets the %s%% floor.\n", totalPct, minTotal; \
	    } \
	    if (!(oddsPkg in seen)) { \
	      printf "FAIL  %s produced no coverage blocks -- it is missing from the profile.\n", oddsPkg; \
	      bad = 1; \
	    } else { \
	      oddsPct = (stmt[oddsPkg] ? 100 * cov[oddsPkg] / stmt[oddsPkg] : 0); \
	      budget = maxOddsGap + 0; \
	      if (g > budget) { \
	        printf "FAIL  %s is at %.2f%% with %d uncovered block(s), over its budget of %d:\n", oddsPkg, oddsPct, g, budget; \
	        for (i = 1; i <= g; i++) printf "        %s\n", gap[i]; \
	        printf "      The odds math is the one place where a wrong answer discredits the\n"; \
	        printf "      whole project (CLAUDE.md section 10). Cover the new block, restructure\n"; \
	        printf "      it away, or -- only with a written argument that no input reaches it --\n"; \
	        printf "      raise COVER_MAX_ODDS_UNCOVERED and record the reasoning beside it.\n"; \
	        bad = 1; \
	      } else { \
	        printf "OK    %s is at %.2f%% with %d uncovered block(s), within its budget of %d.\n", oddsPkg, oddsPct, g, budget; \
	        if (g < budget) { \
	          printf "      Below budget: lower COVER_MAX_ODDS_UNCOVERED to %d to keep the gate tight.\n", g; \
	        } \
	        if (g > 0) { \
	          printf "      The budgeted blocks, each a defensive error return behind a guard that\n"; \
	          printf "      already excludes the failure (see the note above the target):\n"; \
	          for (i = 1; i <= g; i++) printf "        %s\n", gap[i]; \
	        } \
	      } \
	    } \
	    printf "\n      profile: coverage.out   browsable: coverage.html\n"; \
	    if (bad) exit 1; \
	  }' coverage.out

.PHONY: e2e
e2e: ## Playwright critical-path E2E through the proxy (one-shot container)
	$(COMPOSE_TOOLS) run --rm e2e

.PHONY: load
load: ## Distributed Locust WebSocket-fanout load test, headless (LOCUST_WORKERS=4)
	$(COMPOSE_TOOLS) --profile load up --build \
	  --abort-on-container-exit --exit-code-from locust-master \
	  --scale locust-worker=$(LOCUST_WORKERS) \
	  locust-master locust-worker

# =============================================================================
##@ Quality
# =============================================================================

.PHONY: lint
lint: cache-init build-tools ## Run golangci-lint in the tools container
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_TOOLS_FLAGS) $(TOOLS_IMAGE) \
	  golangci-lint run $(PKG)

.PHONY: fmt
fmt: cache-init ## Format all Go source in a container (writes back through the mount)
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_GO_FLAGS) $(GO_IMAGE) \
	  gofmt -l -w .

.PHONY: fmt-check
fmt-check: cache-init ## Fail if any Go source is unformatted
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_GO_FLAGS) $(GO_IMAGE) \
	  sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then printf "unformatted:\n%s\n" "$$out"; exit 1; fi'

.PHONY: vet
vet: cache-init ## Run the Go vet analyzer in a container
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_GO_FLAGS) $(GO_IMAGE) \
	  go vet $(PKG)

.PHONY: vuln
vuln: cache-init ## Run govulncheck in a container (cached in a named GOBIN volume)
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_GO_FLAGS) $(GO_IMAGE) \
	  sh -c 'go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) && /go/bin/govulncheck $(PKG)'

.PHONY: scan
scan: docker-socket ## Trivy image scan (SCAN_TARGET=ghcr.io/anpl1623/sharpline/api:dev)
	docker run --rm \
	  -v $(DOCKER_SOCKET):/var/run/docker.sock \
	  -v $(PROJECT)-trivy-cache:/root/.cache \
	  $(TRIVY_IMAGE) image \
	    --severity $(SCAN_SEVERITY) \
	    --exit-code 1 \
	    --ignore-unfixed \
	    $(SCAN_TARGET)

.PHONY: scan-all
scan-all: ## Trivy scan every built service image
	for svc in $(GO_SERVICES) web; do \
	  $(MAKE) scan SCAN_TARGET=$(IMAGE_PREFIX)/$$svc:$(VERSION) || exit 1; \
	done

.PHONY: check
check: verify-no-host-toolchain fmt-check vet lint test ## The full local gate CI mirrors

# =============================================================================
##@ Codegen
# =============================================================================

.PHONY: codegen
codegen: codegen-sqlc codegen-openapi ## Run every code generator

.PHONY: codegen-sqlc
codegen-sqlc: cache-init build-tools ## Generate typed DB access from SQL (sqlc)
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_TOOLS_FLAGS) $(TOOLS_IMAGE) \
	  sqlc generate

.PHONY: codegen-openapi
codegen-openapi: cache-init build-tools ## Generate server + client stubs from the OpenAPI spec
	docker run --rm $(DOCKER_AS_USER) $(DOCKER_TOOLS_FLAGS) $(TOOLS_IMAGE) \
	  oapi-codegen -config internal/httpapi/oapi-codegen.yaml internal/httpapi/openapi.yaml

.PHONY: openapi
openapi: codegen-openapi ## Regenerate the OpenAPI server/client artifacts

# =============================================================================
##@ Data-plane shells (nothing is published to a host port)
# =============================================================================

.PHONY: psql
psql: ## Interactive SQL prompt against Postgres (brings it up healthy first)
	$(COMPOSE_TOOLS) run --rm psql

.PHONY: redis-cli
redis-cli: ## Interactive Redis prompt inside the running redis container
	$(COMPOSE) exec redis sh -lc 'exec redis-cli --no-auth-warning $${SHARPLINE_REDIS_PASSWORD:+-a "$$SHARPLINE_REDIS_PASSWORD"} $(ARGS)'

.PHONY: kafka-topics
kafka-topics: ## List topics, or ARGS="--describe --topic odds.normalized"
	$(COMPOSE_TOOLS) run --rm kafka-cli -lc \
	  'kafka-topics.sh --bootstrap-server $$KAFKA_BOOTSTRAP $(if $(ARGS),$(ARGS),--list)'

.PHONY: kafka-console
kafka-console: ## Tail a Kafka topic (KAFKA_TOPIC=price.computed)
	$(COMPOSE_TOOLS) run --rm kafka-cli -lc \
	  'kafka-console-consumer.sh --bootstrap-server $$KAFKA_BOOTSTRAP --topic $(KAFKA_TOPIC) --from-beginning --property print.key=true --property print.timestamp=true'

.PHONY: kafka-groups
kafka-groups: ## Show consumer groups and lag
	$(COMPOSE_TOOLS) run --rm kafka-cli -lc \
	  'kafka-consumer-groups.sh --bootstrap-server $$KAFKA_BOOTSTRAP --all-groups --describe'

.PHONY: kafka-shell
kafka-shell: ## Drop into a shell with the Kafka CLI on PATH
	$(COMPOSE_TOOLS) run --rm kafka-cli -l

# =============================================================================
##@ Frontend (never installs into web/node_modules from the host)
# =============================================================================

.PHONY: web-install
web-install: ## Install/refresh web deps INSIDE the container; lockfile round-trips via the mount
	$(COMPOSE_TOOLS) run --rm npm npm install $(ARGS)

.PHONY: web-ci
web-ci: ## Reproducible install from the committed lockfile
	$(COMPOSE_TOOLS) run --rm npm npm ci

.PHONY: web-lint
web-lint: ## Lint the Next.js app in a container
	$(COMPOSE_TOOLS) run --rm npm npm run lint

.PHONY: web-typecheck
web-typecheck: ## TypeScript strict typecheck in a container
	$(COMPOSE_TOOLS) run --rm npm npm run typecheck

.PHONY: web-build
web-build: ## Production Next.js build in a container (standalone output)
	$(COMPOSE_TOOLS) run --rm npm npm run build

# =============================================================================
##@ Terraform (runs from the tools container, CLAUDE.md section 9)
# =============================================================================

.PHONY: tf-init
tf-init: ## terraform init for TF_ENV (local|prod)
	$(COMPOSE_TOOLS) run --rm terraform terraform init $(ARGS)

.PHONY: tf-plan
tf-plan: ## terraform plan for TF_ENV
	$(COMPOSE_TOOLS) run --rm terraform terraform plan $(ARGS)

.PHONY: tf-apply
tf-apply: ## terraform apply for TF_ENV
	$(COMPOSE_TOOLS) run --rm terraform terraform apply $(ARGS)

.PHONY: tf-fmt
tf-fmt: ## terraform fmt across the whole deploy tree
	$(COMPOSE_TOOLS) run --rm $(DOCKER_AS_USER) terraform terraform fmt -recursive /workspace/deploy/terraform

# =============================================================================
##@ Deploy -- Kubernetes, Helm only (CLAUDE.md section 9)
# =============================================================================
#
# STATUS: the chart these targets drive is PHASE 10 (CLAUDE.md section 11). It does
# not exist yet, so all three fail at `deploy-preflight` with an explanation. That is
# the designed behaviour: .github/workflows/deploy.yml calls `make deploy-dry-run`,
# `make deploy` and `make deploy-status`, and a target that silently succeeded
# against a missing chart would report a green deploy that never happened.
#
# The release tool and the cluster client both live in the tools image, never on the
# host -- the prime directive has no Kubernetes exemption, and CLAUDE.md section 9 is
# explicit that Helm is the sole deploy path.
#
# CI (.github/workflows/deploy.yml) materialises a kubeconfig into $RUNNER_TEMP and
# exports KUBECONFIG, DEPLOY_ENV, REGISTRY, PROJECT and VERSION; every one of those is
# `?=` here, so the workflow's values win without a Makefile edit.

DEPLOY_ENV     ?= dev
HELM_CHART     ?= deploy/helm
HELM_VALUES    ?= deploy/helm/values-$(DEPLOY_ENV).yaml
HELM_RELEASE   ?= $(PROJECT)
HELM_NAMESPACE ?= $(PROJECT)-$(DEPLOY_ENV)
HELM_TIMEOUT   ?= 10m
KUBECONFIG     ?= $(HOME)/.kube/config

# Image coordinates handed to the chart. The value KEYS are the Makefile's guess
# until phase 10 authors deploy/helm/values.yaml -- kept in one variable precisely so
# that reconciling them is a one-line change rather than an edit to three recipes.
HELM_IMAGE_ARGS ?= --set image.registry=$(REGISTRY) \
                   --set image.repository=$(PROJECT) \
                   --set image.tag=$(VERSION)

# A kind cluster's API server is published on the host loopback, which a container on
# the default bridge cannot reach. Phase 10 decides the wiring (joining kind's own
# docker network is the usual answer); this knob exists so that decision does not
# require touching these recipes. Empty = the default bridge.
HELM_DOCKER_NETWORK ?=

DOCKER_KUBE_FLAGS = --rm \
	--volume $(ROOT_DIR):/workspace:ro \
	--workdir /workspace \
	--volume $(KUBECONFIG):/kube/config:ro \
	--env KUBECONFIG=/kube/config \
	--add-host host.docker.internal:host-gateway \
	$(if $(strip $(HELM_DOCKER_NETWORK)),--network $(strip $(HELM_DOCKER_NETWORK)))

# Everything below refuses to run until this passes. Each branch names the thing that
# is missing and what is supposed to create it -- the alternative is a tool stack
# trace about a path the reader has no reason to recognise.
.PHONY: deploy-preflight
deploy-preflight: ## Fail loudly (and early) if the chart, its values, or the kubeconfig are missing
	@if [ ! -f "$(ROOT_DIR)/$(HELM_CHART)/Chart.yaml" ]; then \
	   printf '\nFAIL  no chart at %s -- the Kubernetes deploy path is PHASE 10.\n\n' '$(HELM_CHART)'; \
	   printf '      CLAUDE.md section 11 sequences it deliberately: phase 0 ships the container\n'; \
	   printf '      substrate, and %s/Chart.yaml plus values-{dev,prod}.yaml arrive\n' '$(HELM_CHART)'; \
	   printf '      in phase 10 alongside the Terraform that provisions the cluster.\n\n'; \
	   printf '      This target fails on purpose rather than pretending to deploy. It starts\n'; \
	   printf '      working the moment phase 10 lands the chart -- no edit to the Makefile.\n\n'; \
	   printf '      Until then, the whole system runs locally:  make up\n\n'; \
	   exit 1; \
	 fi; \
	 if [ ! -f "$(ROOT_DIR)/$(HELM_VALUES)" ]; then \
	   printf '\nFAIL  DEPLOY_ENV=%s selects %s, which does not exist.\n\n' '$(DEPLOY_ENV)' '$(HELM_VALUES)'; \
	   printf '      CLAUDE.md section 9: one chart, with values-dev.yaml and values-prod.yaml.\n'; \
	   printf '      Either DEPLOY_ENV is wrong or that file has not been written yet.\n\n'; \
	   exit 1; \
	 fi; \
	 if [ -z "$(KUBECONFIG)" ] || [ ! -f "$(KUBECONFIG)" ]; then \
	   printf '\nFAIL  KUBECONFIG=%s is not a single readable file.\n\n' '$(KUBECONFIG)'; \
	   printf '      It is bind-mounted into the container, and a bind mount of a missing path\n'; \
	   printf '      silently becomes an empty DIRECTORY -- so the cluster credentials would be\n'; \
	   printf '      replaced by a folder and every command would fail on a connection error.\n\n'; \
	   exit 1; \
	 fi; \
	 printf 'OK  chart %s, values %s, kubeconfig %s\n' '$(HELM_CHART)' '$(HELM_VALUES)' '$(KUBECONFIG)'

.PHONY: deploy-dry-run
deploy-dry-run: deploy-preflight build-tools ## Lint + render the release without applying anything
	docker run $(DOCKER_KUBE_FLAGS) $(TOOLS_IMAGE) \
	  helm lint $(HELM_CHART) --values $(HELM_VALUES)
	docker run $(DOCKER_KUBE_FLAGS) $(TOOLS_IMAGE) \
	  helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
	    --namespace $(HELM_NAMESPACE) \
	    --create-namespace \
	    --values $(HELM_VALUES) \
	    $(HELM_IMAGE_ARGS) \
	    --dry-run

# --atomic --wait: a rollout that fails rolls itself back, so a red deploy job leaves
# the cluster on the previous release rather than half-migrated. The migrate Job runs
# inside this one command as a pre-install/pre-upgrade hook (CLAUDE.md section 9), so
# schema migration is never a binary anyone runs by hand.
.PHONY: deploy
deploy: deploy-preflight build-tools ## Roll the release out (DEPLOY_ENV=dev|prod, VERSION=<tag>)
	docker run $(DOCKER_KUBE_FLAGS) $(TOOLS_IMAGE) \
	  helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
	    --namespace $(HELM_NAMESPACE) \
	    --create-namespace \
	    --values $(HELM_VALUES) \
	    $(HELM_IMAGE_ARGS) \
	    --atomic \
	    --wait \
	    --timeout $(HELM_TIMEOUT)

.PHONY: deploy-status
deploy-status: deploy-preflight build-tools ## Report the release and the workloads it owns
	docker run $(DOCKER_KUBE_FLAGS) $(TOOLS_IMAGE) \
	  helm status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)
	docker run $(DOCKER_KUBE_FLAGS) $(TOOLS_IMAGE) \
	  kubectl --namespace $(HELM_NAMESPACE) get deployments,statefulsets,pods,services,ingress --output wide

# =============================================================================
##@ Caches / cleanup
# =============================================================================

# A fresh named volume mounted at a path that does NOT exist in the image is
# created root-owned 0755 (verified empirically). Containers here run as a
# non-root uid, so without this step every Go cache write fails with EACCES.
# Seeding the volumes once as 0777 makes them writable whichever uid a target
# picks -- the invoking user for targets that write source files, root for the
# test container which must open the Docker socket.
.PHONY: cache-init
cache-init: ## Create the named build-cache volumes and make them writable
	@docker run --rm \
	  -v $(GOMOD_VOLUME):/v/gopkg \
	  -v $(GOBUILD_VOLUME):/v/gobuild \
	  -v $(GOBIN_VOLUME):/v/gobin \
	  -v $(TOOLS_CACHE_VOLUME):/v/toolscache \
	  $(ALPINE_IMAGE) sh -c 'mkdir -p /v/gopkg/mod /v/gopkg/sumdb && chmod 0777 /v/gopkg /v/gopkg/mod /v/gopkg/sumdb /v/gobuild /v/gobin /v/toolscache'

.PHONY: cache-clean
cache-clean: ## Delete every named build-cache volume
	docker volume rm --force $(CACHE_VOLUMES)

.PHONY: clean
clean: down-hard cache-clean ## Full reset: stack down with volumes, caches dropped

.PHONY: clean-images
clean-images: ## Remove locally built Sharpline images
	for svc in $(GO_SERVICES) web; do \
	  docker image rm --force $(IMAGE_PREFIX)/$$svc:$(VERSION) >/dev/null 2>&1 || true; \
	  docker image rm --force $(LOCAL_IMAGE_PREFIX)/$$svc:$(LOCAL_TAG) >/dev/null 2>&1 || true; \
	done; \
	docker image rm --force $(TOOLS_IMAGE) >/dev/null 2>&1 || true
