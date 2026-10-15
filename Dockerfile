# syntax=docker/dockerfile:1.7
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Every FROM is pinned to a digest, not just a tag: a floating tag can be
# repointed (accidentally or via supply-chain compromise) without this file
# changing at all. The tag stays alongside the digest for readability --
# Docker ignores it once a digest is present, it never affects what's
# actually pulled. These go stale as upstream ships security patches under
# the same tag, so they need periodic, deliberate refreshing (not
# "whenever CI happens to rebuild"); re-resolve with e.g.
#   docker buildx imagetools inspect <image>:<tag> | grep Digest
FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS web
WORKDIR /src
COPY web/package.json web/package-lock.json web/tsconfig.json web/vite.config.ts web/index.html ./web/
COPY web/src ./web/src
RUN cd web && npm ci && npm run build

FROM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kairon-controller ./cmd/kairon-controller && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kairon-node ./cmd/kairon-node && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kairon-ui ./cmd/kairon-ui && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kairon-csi-node ./cmd/kairon-csi-node && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kairon-csi-controller ./cmd/kairon-csi-controller

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS controller
COPY --from=build /out/kairon-controller /kairon-controller
ENTRYPOINT ["/kairon-controller"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS node
COPY --from=build /out/kairon-node /kairon-node
ENTRYPOINT ["/kairon-node"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS ui
COPY --from=build /out/kairon-ui /kairon-ui
COPY --from=web /src/web/dist /web
ENV KAIRON_UI_WEB_DIR=/web
ENTRYPOINT ["/kairon-ui"]

# Deliberately NOT the distroless base every other Kairon image uses:
# internal/csinode shells out to a real iSCSI initiator (open-iscsi's
# iscsiadm) and filesystem tooling (util-linux's blkid, e2fsprogs' mkfs.*)
# it doesn't reimplement -- see internal/csinode/exec.go's own doc
# comment for why. A larger, real attack surface than the other three
# images, and a real tradeoff, not an oversight; it's also why this
# runs privileged (see charts/kairon/templates/all.yaml's kairon-csi-node
# DaemonSet) -- both are inherent to actually attaching/mounting network
# block devices from inside a container, not specific to this image.
FROM debian:12-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS csi-node
# apt-get upgrade, not just install: the base image is pinned to a fixed
# digest for reproducibility, but that freezes whatever package versions
# were current the day that digest was built -- Debian security patches
# published since then (e.g. libpcre2-8-0's CVE-2026-86145/89157/89161)
# never apply on their own. Explicitly upgrading pulls the latest patched
# packages from Debian's own apt repos at build time without re-pinning
# (or having to track) the base image digest itself.
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y --no-install-recommends open-iscsi util-linux e2fsprogs ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/kairon-csi-node /kairon-csi-node
COPY docker/kairon-csi-node-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]

# Also not distroless, for the same reason as csi-node above: dynamic
# provisioning (internal/csinode's ControllerServer) shells out to Linux
# LIO's targetcli to create/destroy real iSCSI backstores/targets/LUNs
# (see internal/csinode/lio.go) -- reimplementing raw configfs
# manipulation isn't something this project does either. targetcli-fb
# additionally needs the host's target_core_mod kernel module already
# loaded (an operator/host prerequisite this image deliberately doesn't
# try to modprobe itself -- see docs/guides/machine-storage-csi.md, the
# same "we don't reach into host-level setup for you" posture this
# project already takes for webhook/console TLS material).
FROM debian:12-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS csi-controller
# apt-get upgrade -- see csi-node's identical comment above for why this
# is needed alongside a digest-pinned base image.
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y --no-install-recommends targetcli-fb ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/kairon-csi-controller /kairon-csi-controller
ENTRYPOINT ["/kairon-csi-controller"]

# kairon-mcp: `kaironctl mcp serve` over stdio, packaged for the MCP Registry
# (server.json). Its own build stage so the kairon-* images above are
# untouched. The entrypoint deliberately does NOT carry --allow-write: the
# server offers read tools only unless the operator appends --allow-write
# to the container arguments themselves (and then also needs a writable
# --audit-log, see docs/guides/hermes-mcp.md).
FROM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build-mcp
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kaironctl ./cmd/kaironctl

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS mcp
# The MCP Registry verifies ownership of an OCI package by this label; it
# must equal "name" in server.json (scripts/validate.py gates that).
LABEL io.modelcontextprotocol.server.name="io.github.zyvorai/kairon"
LABEL org.opencontainers.image.source="https://github.com/zyvorai/zyvor-kairon"
COPY --from=build-mcp /out/kaironctl /kaironctl
ENTRYPOINT ["/kaironctl", "mcp", "serve"]
