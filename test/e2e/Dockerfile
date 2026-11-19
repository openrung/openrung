# syntax=docker/dockerfile:1
# SPDX-License-Identifier: GPL-3.0-or-later
#
# OpenRung network lab — one image holding every binary under test plus the
# tools that build a simulated internet out of network namespaces. It is a test
# fixture, never shipped: run it with `make e2e` (see test/e2e/README.md).
#
# Build from the repository ROOT. XRAY_IMAGE is the `xray` stage of
# deploy/relay/Dockerfile, so the lab runs the same pinned, checksum-verified
# Xray as the relay image; `make e2e-image` builds that stage first.

ARG XRAY_IMAGE=openrung-e2e-xray:latest
FROM ${XRAY_IMAGE} AS xray

FROM golang:1.25-alpine AS deps
WORKDIR /src
COPY go.mod go.sum ./
# The root go.mod replaces the in-repo nested modules, so their go.mod files
# must exist before `go mod download`.
COPY brokerapi/go.mod brokerapi/
COPY connectcore/go.mod connectcore/
COPY punchcore/go.mod punchcore/
COPY wsscore/go.mod wsscore/
RUN --mount=type=cache,target=/go/pkg/mod go mod download

FROM deps AS build
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    export CGO_ENABLED=0; \
    go build -trimpath -tags with_utls,with_external_windivert -o /out/ \
      ./cmd/broker ./cmd/relay ./cmd/relayhub ./cmd/wsssidecar ./cmd/client; \
    go build -trimpath -o /out/lab ./test/e2e/cmd/lab; \
    go test -c -trimpath -tags e2e -o /out/e2e.test ./test/e2e

FROM alpine:3.21
RUN apk add --no-cache ca-certificates conntrack-tools curl dnsmasq iproute2 iptables tcpdump
COPY --from=xray /xray/ /usr/local/bin/
COPY --from=build /out/broker /out/relay /out/relayhub /out/lab /out/e2e.test /usr/local/bin/
COPY --from=build /out/wsssidecar /usr/local/bin/wss-sidecar
COPY --from=build /out/client /usr/local/bin/openrung-client
# Relays start through the shipped entrypoint so the lab covers its env mapping.
COPY deploy/relay/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV OPENRUNG_XRAY_PATH=/usr/local/bin/xray \
    XRAY_LOCATION_ASSET=/usr/local/bin
ENTRYPOINT ["/usr/local/bin/lab"]
CMD ["test"]
