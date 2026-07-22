# syntax=docker/dockerfile:1
#
# OpenRung relay hub — the publicly reachable component that terminates reverse
# tunnels from CGNAT volunteer-run relays and forwards client traffic to them.
# Unlike the relay runtime image it bundles no Xray (it only copies opaque
# bytes). Build from the repository ROOT so the Go sources are in the build
# context:
#
#   docker build -f deploy/relayhub/Dockerfile -t openrung-relayhub .
#
# Multi-arch (amd64 + arm64) via buildx:
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/relayhub/Dockerfile -t <registry>/openrung-relayhub .

ARG RELAYHUB_VERSION=source
ARG VCS_REVISION=unknown

# --- Stage 1: build the relayhub binary (cross-compiled, no cgo) ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# The root go.mod replaces the in-repo nested modules, so their go.mod files
# must exist before `go mod download`.
COPY punchcore/go.mod punchcore/
COPY wsscore/go.mod wsscore/
RUN go mod download
COPY . .
ARG TARGETARCH
ARG TARGETOS=linux
ARG RELAYHUB_VERSION
ARG VCS_REVISION
RUN set -eux; \
    ldflags="-s -w"; \
    if [ -n "$RELAYHUB_VERSION" ] && [ "$RELAYHUB_VERSION" != "source" ]; then \
      ldflags="$ldflags -X main.version=$RELAYHUB_VERSION"; \
    fi; \
    if [ -n "$VCS_REVISION" ] && [ "$VCS_REVISION" != "unknown" ]; then \
      ldflags="$ldflags -X main.revision=$VCS_REVISION"; \
    fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags="$ldflags" -o /out/relayhub ./cmd/relayhub

# --- Stage 2: minimal runtime ---
FROM alpine:3.21
ARG RELAYHUB_VERSION
ARG VCS_REVISION
# Image bundles GPL-2.0-only (busybox/apk-tools/baselayout from Alpine),
# MPL-2.0 (ca-certificates, yamux compiled into the binary), and MIT/BSD
# components. See /usr/local/share/openrung/THIRD_PARTY_NOTICES.md.
LABEL org.opencontainers.image.source="https://github.com/openrung/openrung" \
      org.opencontainers.image.licenses="GPL-3.0-or-later AND MPL-2.0 AND GPL-2.0-only AND MIT AND BSD-3-Clause" \
      org.opencontainers.image.title="openrung-relayhub" \
      org.opencontainers.image.version="$RELAYHUB_VERSION" \
      org.opencontainers.image.revision="$VCS_REVISION"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S openrung \
    && adduser -S -G openrung openrung
COPY --from=build /out/relayhub /usr/local/bin/relayhub
# Third-party attribution + GPL/MPL source offers travel with the distributed image.
COPY THIRD_PARTY_NOTICES.md /usr/local/share/openrung/THIRD_PARTY_NOTICES.md
USER openrung
# The hub is configured entirely through OPENRUNG_* environment variables, so the
# binary is the entrypoint directly (no shell wrapper). It receives SIGTERM as
# PID 1 for a clean shutdown.
ENTRYPOINT ["/usr/local/bin/relayhub"]
