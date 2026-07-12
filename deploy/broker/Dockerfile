# syntax=docker/dockerfile:1
#
# OpenRung broker — the control-plane HTTP API: it matches clients with healthy
# relays and records operational telemetry. It never carries user traffic and
# bundles no Xray. Build from the repository ROOT so the Go sources are in the
# build context:
#
#   docker build -f deploy/broker/Dockerfile -t openrung-broker .
#
# Multi-arch (amd64 + arm64) via buildx:
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/broker/Dockerfile -t <registry>/openrung-broker .

# --- Stage 1: build the broker binary (cross-compiled, no cgo) ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# The root go.mod replaces github.com/openrung/openrung/punchcore with the
# in-repo nested module, so its go.mod must exist before `go mod download`.
COPY punchcore/go.mod punchcore/
RUN go mod download
COPY . .
ARG TARGETARCH
ARG TARGETOS=linux
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/broker ./cmd/broker

# --- Stage 2: minimal runtime ---
FROM alpine:3.21
# Image bundles GPL-2.0-only (busybox/apk-tools/baselayout from Alpine),
# MPL-2.0 (ca-certificates, pgx compiled into the binary), and MIT/BSD
# components. See /usr/local/share/openrung/THIRD_PARTY_NOTICES.md.
LABEL org.opencontainers.image.source="https://github.com/openrung/openrung" \
      org.opencontainers.image.licenses="GPL-3.0-or-later AND MPL-2.0 AND GPL-2.0-only AND MIT AND BSD-3-Clause" \
      org.opencontainers.image.title="openrung-broker"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S openrung \
    && adduser -S -G openrung openrung \
    # State dir for the append-only telemetry JSONL. Owned by the runtime user so
    # a fresh named volume mounted here inherits writable ownership (Docker copies
    # the image dir's ownership onto an empty named volume). Bind mounts do NOT —
    # chown the host path to the container user for those.
    && mkdir -p /var/lib/openrung \
    && chown openrung:openrung /var/lib/openrung
COPY --from=build /out/broker /usr/local/bin/broker
# Third-party attribution + GPL/MPL source offers travel with the distributed image.
COPY THIRD_PARTY_NOTICES.md /usr/local/share/openrung/THIRD_PARTY_NOTICES.md
USER openrung
# The broker is configured entirely through OPENRUNG_* environment variables, so
# the binary is the entrypoint directly (no shell wrapper). It receives SIGTERM
# as PID 1 for a clean shutdown (it drains in-flight requests and flushes
# telemetry).
ENV OPENRUNG_TELEMETRY_FILE=/var/lib/openrung/telemetry.jsonl
ENTRYPOINT ["/usr/local/bin/broker"]
