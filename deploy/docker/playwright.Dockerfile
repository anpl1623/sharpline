# ---------------------------------------------------------------------------
# sharpline `playwright` image  (CLAUDE.md §9 inventory row `playwright`, §10)
# ---------------------------------------------------------------------------
# One-shot E2E runner against the compose stack. Critical path per CLAUDE.md
# §10: sign in -> browse board -> build parlay -> place -> observe settlement.
# It reaches the app through the proxy only (CLAUDE.md §9: the proxy is the one
# published entrypoint), never a container hostname:port of its own choosing.
#
# VERSION LOCKSTEP — this is the failure mode this file is most likely to hit:
# the browser binaries baked into mcr.microsoft.com/playwright:vX are only
# usable by @playwright/test at exactly version X. If `e2e/package.json` pins a
# different @playwright/test, Playwright refuses to run ("Executable doesn't
# exist at /ms-playwright/chromium-NNNN"). PLAYWRIGHT_VERSION below, the image
# tag, and the version in e2e/package.json move together or not at all.
# ---------------------------------------------------------------------------

# digest-pinned base, verbatim from the ledger "Resolved base images".
# Tag is v1.62.1-noble (Ubuntu 24.04 LTS) — see ledger deviation 1: MCR's
# `:latest` is an unstable pre-release pointer and must not be used here.
ARG PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.62.1-noble@sha256:dcc5531e97840b9b5e794f2814476b21571c5124a3fca2267d73041f56e7580e

FROM ${PLAYWRIGHT_IMAGE} AS playwright

# Must equal the image tag above.
ARG PLAYWRIGHT_VERSION=1.62.1

LABEL org.opencontainers.image.title="sharpline-playwright" \
      org.opencontainers.image.description="One-shot Playwright E2E runner for the sharpline compose stack" \
      org.opencontainers.image.source="https://github.com/anpl1623/sharpline" \
      org.opencontainers.image.licenses="Apache-2.0"

USER root

# A global @playwright/test makes the image self-sufficient: `playwright test`
# works even before `e2e/node_modules` has been populated, and CI never has a
# step that silently depends on a host npm. A project-local install in the
# bind-mounted e2e/ directory still wins over this one via normal node
# resolution — which is exactly why the versions must match.
#
# The install is then asserted against the browsers actually baked into the
# image, so a mismatch fails the BUILD rather than an E2E run at 2am.
RUN npm install -g --no-fund --no-audit "@playwright/test@${PLAYWRIGHT_VERSION}" \
 && npm cache clean --force \
 && playwright --version \
 && test "$(playwright --version | awk '{print $2}')" = "${PLAYWRIGHT_VERSION}" \
 && ls -d "${PLAYWRIGHT_BROWSERS_PATH}"/chromium-* >/dev/null

# Node only searches `node_modules` directories walking UP from the importing
# file, and never consults the global prefix. Without this, a spec in the
# bind-mounted /e2e that has no local node_modules yet cannot resolve
# '@playwright/test' at all — the CLI is global, the import is not. A
# /node_modules symlink puts the global tree on that upward search path, and
# unlike NODE_PATH it works for ESM/TypeScript imports as well as require().
# Anything installed in e2e/node_modules still shadows it, as it should.
RUN ln -s /usr/lib/node_modules /node_modules

# The repo's e2e/ directory (CLAUDE.md §8) is bind-mounted here. 0777 because
# Playwright writes test-results/, playwright-report/ and traces back through
# the mount, and the caller may override the uid.
RUN mkdir -p /e2e \
 && chown 1001:1001 /e2e \
 && chmod 0777 /e2e

# npm/node caches must live somewhere writable regardless of --user, otherwise
# a uid override turns every run into an EACCES.
ENV HOME=/home/pwuser \
    NPM_CONFIG_CACHE=/tmp/.npm \
    NPM_CONFIG_UPDATE_NOTIFIER=false \
    NPM_CONFIG_FUND=false \
    XDG_CACHE_HOME=/tmp/.cache \
    CI=1
RUN mkdir -p /home/pwuser /tmp/.npm /tmp/.cache \
 && chmod -R 0777 /home/pwuser /tmp/.npm /tmp/.cache

WORKDIR /e2e

# Chromium's sandbox needs either --no-sandbox or extra capabilities. Running as
# a non-root user is the half of that tradeoff worth keeping; the browser flags
# belong in playwright.config.ts, not baked in here.
USER 1001:1001

# Base image has no ENTRYPOINT and we add none, so `docker run --rm <img> <cmd>`
# behaves normally. CMD is only the one-shot default.
CMD ["playwright", "test"]
