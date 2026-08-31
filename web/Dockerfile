# =============================================================================
# Sharpline frontend — PRODUCTION image.
#
# CLAUDE.md §7: "multi-stage build — deps -> build -> runtime. output:
# 'standalone' in next.config, runtime stage copies only .next/standalone,
# .next/static, and public onto node:24-alpine running as the non-root `node`
# user. Target image under 200MB."
#
# -----------------------------------------------------------------------------
# BUILD CONTEXT IS `web/`, NOT THE REPO ROOT. This is load-bearing.
#
#   docker build -f web/Dockerfile -t sharpline/web:dev ./web
#
# and in compose:
#
#   build:
#     context: ../../web          # <-- web/, relative to deploy/compose/
#     dockerfile: Dockerfile
#
# Every COPY below is written against that context (`package.json`, not
# `web/package.json`). Do not "fix" this to a repo-root context: web/.dockerignore
# only applies to a context rooted at web/, so a repo-root context would ship the
# entire Go tree — and the host's `web/node_modules` — into the build, which is
# both enormous and a prime-directive violation (host-arch binaries leaking in).
# -----------------------------------------------------------------------------
#
# NOTE — no `# syntax=` parser directive on purpose, matching
# deploy/docker/go.Dockerfile:26-31. A `# syntax=docker/dockerfile:1` directive
# makes BuildKit fetch and execute that image as the build frontend on every
# build: a floating tag whose digest moves, which CLAUDE.md §12 forbids. Every
# feature used here (ARG-before-FROM, ARG in FROM, RUN --mount=type=cache with
# sharing=locked, COPY --from --chown) is supported by the BuildKit frontend
# built into the daemon, so dropping the directive costs nothing and the build
# pulls zero unpinned images.
#
# Base image digest copied verbatim from the contract ledger's resolved table
# (multi-arch manifest list: linux/amd64 + linux/arm64, per CLAUDE.md §9 buildx).
# =============================================================================

ARG NODE_IMAGE=node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43

# ---------------------------------------------------------------------------
# Stage 1: deps — resolve node_modules from the lockfile, nothing else.
# Isolated so a source-only change never re-runs `npm ci`.
# ---------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS deps
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY package.json package-lock.json ./
# BuildKit cache mount on the npm cache (CLAUDE.md §9) — without it,
# containerized builds get slow enough that people start cheating with a host
# `npm install`, which breaks the prime directive.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    npm ci --no-audit --no-fund

# ---------------------------------------------------------------------------
# Stage 2: build — compile the Next standalone output.
# ---------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS build
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

# CLAUDE.md §7: the browser reaches the API through the reverse proxy, never a
# container hostname. These are proxy PATHS, and they are inlined at build time
# because that is how NEXT_PUBLIC_* works — a runtime override would not take.
ARG NEXT_PUBLIC_API_URL=/api
ARG NEXT_PUBLIC_WS_URL=/ws
ENV NEXT_PUBLIC_API_URL=${NEXT_PUBLIC_API_URL}
ENV NEXT_PUBLIC_WS_URL=${NEXT_PUBLIC_WS_URL}

COPY --from=deps /app/node_modules ./node_modules
COPY . .
# `public/` is optional in Next but the runtime COPY below is not conditional,
# so guarantee the directory exists in this stage.
RUN mkdir -p public

# Cache mount on Next's incremental build cache. It is a build accelerator
# only, and mounting it here also keeps it out of the image layer.
RUN --mount=type=cache,target=/app/.next/cache,sharing=locked \
    npm run build

# ---------------------------------------------------------------------------
# Stage 3: runtime — only the three artifacts §7 names, on the mandated base,
# as the non-root `node` user that image already ships.
# ---------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS runtime
WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
# Bind on all interfaces so the proxy can reach it over the compose network.
# Nothing publishes this port to the host — the proxy is the only entrypoint
# (CLAUDE.md §9, §12).
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

COPY --from=build --chown=node:node /app/public ./public
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static

# Patch the base image's OpenSSL.
#
# Trivy fails the scan on CVE-2026-14456 (HIGH, OpenSSL DoS): node:24-alpine
# ships libcrypto3/libssl3 3.5.7-r0 and the fix is 3.5.8-r0. Bumping NODE_IMAGE
# does NOT help -- the current node:24-alpine digest still carries 3.5.7-r0 --
# but 3.5.8-r0 IS in the Alpine v3.24 main repository the image points at, so
# the upgrade is available without changing branch or base.
#
# THE COST, STATED RATHER THAN HIDDEN. CLAUDE.md §12 pins base images by digest
# so a build is reproducible, and `apk upgrade` reaches out to a repository
# whose contents move, which weakens that for these two packages. It is the
# lesser evil: the alternative is shipping a known-vulnerable OpenSSL in the one
# image that terminates browser traffic, or suppressing the finding in a
# .trivyignore where the next reader has to re-derive whether it still applies.
# Two named packages, not a blanket `apk upgrade`, so the rest of the image
# stays exactly what the digest says it is.
#
# When the upstream image picks up 3.5.8-r0, bump NODE_IMAGE and delete this.
RUN apk upgrade --no-cache libcrypto3 libssl3

# Strip the package manager from the RUNTIME image.
#
# A Next standalone server is started with `node server.js` (see CMD) and never
# invokes npm; the healthcheck below uses busybox wget. But node:24-alpine ships
# npm, and npm vendors its OWN dependency tree at
# /usr/local/lib/node_modules/npm/node_modules -- which is where Trivy found
# tar 7.5.16 (CRITICAL, gzip-bomb DoS), undici 6.26.0, ip-address 10.2.0 and
# brace-expansion 5.0.6.
#
# Those are not this application's dependencies, so no change to package.json or
# package-lock.json can fix them; `npm audit` does not even see them, which is
# why it and Trivy disagreed. Deleting the package manager removes the findings
# and the runtime attack surface in one move, and a production image that cannot
# install packages is the better posture regardless (CLAUDE.md §9 hardening).
# yarn goes for the same reason. It scans clean today, but it is a second
# package manager with its own vendored tree sitting in a production image that
# has no use for either.
RUN rm -rf /usr/local/lib/node_modules/npm \
           /usr/local/lib/node_modules/corepack \
           /usr/local/bin/npm \
           /usr/local/bin/npx \
           /usr/local/bin/corepack \
           /opt/yarn-v* \
           /usr/local/bin/yarn \
           /usr/local/bin/yarnpkg

USER node
EXPOSE 3000

HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
  CMD wget --quiet --spider --tries=1 http://127.0.0.1:3000/ || exit 1

CMD ["node", "server.js"]
