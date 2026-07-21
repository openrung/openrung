# syntax=docker/dockerfile:1
#
# OpenRung relay runtime — a self-contained image bundling the `relay` binary
# and a pinned Xray-core. Build from the repository ROOT so the Go sources are
# in the build context:
#
#   docker build -f deploy/relay/Dockerfile -t openrung-relay .
#
# Multi-arch (amd64 + arm64) via buildx:
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/relay/Dockerfile -t <registry>/openrung-relay .

ARG XRAY_VERSION=v26.3.27
ARG RELAY_VERSION=source
ARG VCS_REVISION=unknown

# --- Stage 1: fetch + verify a pinned Xray-core release for the target arch ---
FROM --platform=$BUILDPLATFORM alpine:3.21 AS xray
ARG XRAY_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl unzip
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) asset="Xray-linux-64.zip" ;; \
      arm64) asset="Xray-linux-arm64-v8a.zip" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}"; \
    curl -fsSL -o /tmp/xray.zip "${base}/${asset}"; \
    curl -fsSL -o /tmp/xray.dgst "${base}/${asset}.dgst"; \
    # The .dgst file lists hashes as "SHA2-256= <hex>"; verify the download.
    expected="$(awk '/^SHA2-256=/ {print $2}' /tmp/xray.dgst)"; \
    actual="$(sha256sum /tmp/xray.zip | awk '{print $1}')"; \
    echo "xray ${XRAY_VERSION} ${asset} expected=${expected} actual=${actual}"; \
    test -n "$expected"; \
    test "$expected" = "$actual"; \
    mkdir -p /xray /xray-licenses; \
    unzip -o /tmp/xray.zip xray geoip.dat geosite.dat -d /xray; \
    # Keep Xray-core's own license/readme so the redistributed binary carries it
    # (MPL-2.0 §3.1). The release zip ships these alongside the binary.
    unzip -o /tmp/xray.zip LICENSE README.md -d /xray-licenses; \
    chmod +x /xray/xray

# --- Stage 2: build the relay and its colocated WSS sidecar (no cgo) ---
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
ARG RELAY_VERSION
ARG VCS_REVISION
RUN set -eux; \
    ldflags="-s -w"; \
    if [ -n "$RELAY_VERSION" ] && [ "$RELAY_VERSION" != "source" ]; then \
      ldflags="$ldflags -X main.version=$RELAY_VERSION"; \
    fi; \
    if [ -n "$VCS_REVISION" ] && [ "$VCS_REVISION" != "unknown" ]; then \
      ldflags="$ldflags -X main.revision=$VCS_REVISION"; \
    fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags="$ldflags" -o /out/relay ./cmd/relay; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags="$ldflags" -o /out/wss-sidecar ./cmd/wsssidecar

# --- Stage 3: minimal runtime ---
FROM alpine:3.21
ARG RELAY_VERSION
ARG VCS_REVISION
# Image bundles GPL-3.0-or-later (none here), MPL-2.0 (Xray-core, ca-certificates),
# GPL-2.0-only (busybox/apk-tools/baselayout from Alpine), CC-BY-SA-4.0 (geoip.dat),
# and MIT/BSD components. See /usr/local/share/openrung/THIRD_PARTY_NOTICES.md.
LABEL org.opencontainers.image.source="https://github.com/openrung/openrung" \
      org.opencontainers.image.licenses="GPL-3.0-or-later AND MPL-2.0 AND GPL-2.0-only AND CC-BY-SA-4.0 AND MIT AND BSD-3-Clause" \
      org.opencontainers.image.title="openrung-relay" \
      org.opencontainers.image.version="$RELAY_VERSION" \
      org.opencontainers.image.revision="$VCS_REVISION"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S openrung \
    && adduser -S -G openrung openrung \
    && mkdir -p /var/lib/openrung \
    && chown openrung:openrung /var/lib/openrung \
    && chmod 0700 /var/lib/openrung
COPY --from=xray /xray/ /usr/local/bin/
COPY --from=xray /xray-licenses/ /usr/local/share/openrung/xray/
COPY --from=build /out/relay /usr/local/bin/relay
COPY --from=build /out/wss-sidecar /usr/local/bin/wss-sidecar
COPY deploy/relay/entrypoint.sh /usr/local/bin/entrypoint.sh
# Third-party attribution + GPL/MPL source offers travel with the distributed image.
COPY THIRD_PARTY_NOTICES.md /usr/local/share/openrung/THIRD_PARTY_NOTICES.md
# Let the non-root relay process bind the privileged public port (443). Pairs with
# `cap_add: NET_BIND_SERVICE` at runtime. NOTE: file capabilities are ignored
# under no-new-privileges, so do not enable that flag for this image (or switch
# to a public port >= 1024 and run with zero capabilities instead).
RUN apk add --no-cache --virtual .setcap libcap \
    && setcap 'cap_net_bind_service=+ep' /usr/local/bin/relay \
    && getcap /usr/local/bin/relay | grep -q 'cap_net_bind_service=ep' \
    && apk del .setcap \
    && chmod +x /usr/local/bin/entrypoint.sh
USER openrung
ENV OPENRUNG_XRAY_PATH=/usr/local/bin/xray \
    XRAY_LOCATION_ASSET=/usr/local/bin
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
