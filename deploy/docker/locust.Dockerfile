# ---------------------------------------------------------------------------
# sharpline `locust` image  (CLAUDE.md §9 inventory rows `locust-master` /
# `locust-worker`, §10 "Load testing via Locust")
# ---------------------------------------------------------------------------
# Drives WebSocket fanout against the `stream` gateway through the proxy.
# Target: 10k concurrent subscribers on one node, distributed master/worker,
# workers scaled as replicas (compose) / a Deployment (k8s). CLAUDE.md §10 is
# explicit that each Locust user is a gevent greenlet rather than a goroutine,
# so connection count is bought with worker count.
#
# The base image already ships gevent + a full Locust; this layer exists to:
#   1. turn `websocket-client` from a *transitive* dependency of the base image
#      (it arrives today via python-socketio) into a *declared, pinned, direct*
#      one, so a future locust release cannot quietly take the WS client away;
#   2. drop the base image's `ENTRYPOINT ["locust"]`, which would otherwise make
#      `docker run --rm <img> <cmd>` impossible;
#   3. give the scenario a predictable, writable working directory.
# ---------------------------------------------------------------------------

# digest-pinned base, verbatim from the ledger "Resolved base images"
ARG LOCUST_IMAGE=locustio/locust:latest@sha256:0d7e43081c5f9437aed6513dc5827ec6984f024d00141a201a5b6be6e9a63e5d

FROM ${LOCUST_IMAGE} AS locust

# `websocket-client` is a sync, socket-based client — exactly what gevent's
# monkey-patching turns into a cooperative greenlet, which is why it is the
# right client for a Locust WS scenario.
ARG WEBSOCKET_CLIENT_VERSION=1.9.0

LABEL org.opencontainers.image.title="sharpline-locust" \
      org.opencontainers.image.description="Distributed Locust load generator for sharpline WebSocket fanout" \
      org.opencontainers.image.source="https://github.com/anpl1623/sharpline" \
      org.opencontainers.image.licenses="Apache-2.0"

USER root

RUN pip install --no-cache-dir "websocket-client==${WEBSOCKET_CLIENT_VERSION}" \
 && python -c "import websocket; assert websocket.__version__ == '${WEBSOCKET_CLIENT_VERSION}', websocket.__version__; print('websocket-client', websocket.__version__)"

# The scenario is bind-mounted here from the repo's `load/` directory
# (CLAUDE.md §8). 0777 so the compose/k8s caller may override the uid without
# losing the ability to write .coverage / report files.
RUN mkdir -p /mnt/locust \
 && chown 1000:1000 /mnt/locust \
 && chmod 0777 /mnt/locust

# PYTHONDONTWRITEBYTECODE: never drop root-owned __pycache__ into the mounted
# repo. PYTHONUNBUFFERED is already set by the base but is restated because the
# master/worker logs are the only visibility during a 10k-connection run.
#
# Deliberately NOT set: LOCUST_SKIP_MONKEY_PATCH. Locust tests it with
# `if not os.getenv("LOCUST_SKIP_MONKEY_PATCH", None)`, so *any* value —
# including "0" or "false" — disables gevent monkey-patching and destroys the
# concurrency this image exists to produce. It must stay unset.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /mnt/locust

# 8089 = web UI, 5557 = master<->worker. EXPOSE is metadata only; per CLAUDE.md
# §9 nothing but the proxy binds a host port.
EXPOSE 8089 5557

USER 1000:1000

# The base image sets ENTRYPOINT ["locust"], which would swallow the command in
# `docker run --rm <img> <cmd>`. Clear it and put locust in CMD instead, so the
# image is both a sane default and a usable shell.
ENTRYPOINT []
CMD ["locust"]
