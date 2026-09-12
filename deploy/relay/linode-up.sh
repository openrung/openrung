#!/usr/bin/env bash
#
# Provision an OpenRung volunteer-run relay on Akamai/Linode.
#
#   deploy/relay/linode-up.sh [name]
#
# If no name is given, a random "adjective-noun" name is generated and used as
# BOTH the Linode label and the relay label (OPENRUNG_LABEL), so the relay shows
# up in the broker dashboard under the same friendly name as the box.
#
# Requires the `linode-cli` configured with an API token. The SSH public key at
# ~/.ssh/id_ed25519_openrung.pub is installed on root so the fleet's standard
# key can reach the box. The region must support the Metadata service (the boot
# script self-discovers its public IP from it) — all -3 generation and most
# older regions do; `linode-cli regions list` shows a Metadata capability.
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_TYPE, OPENRUNG_OS_IMAGE,
# OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_FIREWALL_NAME,
# OPENRUNG_SSH_PUBKEY_FILE, OPENRUNG_SHAPE_RATE.
#
# This helper provisions anonymous volunteer-class relays only. It cannot safely
# accept any registration bearer: Linode retains cloud-init user-data in the
# Metadata service for the instance's lifetime. Provision an authenticated host
# first, then install its credential post-boot through an authenticated channel
# (deploy/relay/foundation-up.sh convert) instead of passing it to this helper.
set -euo pipefail

REGION="${OPENRUNG_REGION:-jp-tyo-3}"          # Tokyo 3
TYPE="${OPENRUNG_TYPE:-g6-standard-1}"         # 1 vCPU / 2GB / 2TB transfer / 2 Gbps out
OS_IMAGE="${OPENRUNG_OS_IMAGE:-linode/ubuntu24.04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main@sha256:9e58bdc0218d726d424a2c83e91ef91431767cd880c5683d45989ee9b8d41c66}"
[[ "$IMAGE" =~ @sha256:[a-f0-9]{64}$ ]] || { echo "error: OPENRUNG_IMAGE must be pinned to an immutable sha256 digest" >&2; exit 2; }
# Register against the broker ORIGIN, not the Cloudflare front (broker.openrung.org).
# That hostname is a Worker front for *client* discovery; its edge serves a Managed
# Challenge to datacenter IP ranges (incl. Linode), which a relay's HTTP client
# cannot solve (403). The origin is DNS-only (grey-cloud), so a datacenter IP
# reaches it directly, and it terminates TLS itself (deploy/broker/origin-tls.md)
# — registration no longer crosses the public internet in cleartext. Same default
# as the Lightsail fleet (see lightsail-up.sh).
BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
FIREWALL_NAME="${OPENRUNG_FIREWALL_NAME:-openrung-relay}"
# Egress CAKE shaping rate ('off' disables). g6-standard-1 has a 2 Gbps line,
# but xray on its single shared vCPU tops out near 1 Gbps — the rate must sit
# at or below true deliverable egress so the queue forms on the box, where
# CAKE manages it, not in the provider's switch.
SHAPE_RATE="${OPENRUNG_SHAPE_RATE:-1000mbit}"

if [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_FOUNDATION_TOKEN+x}" = x ]; then
  echo "error: this helper provisions anonymous volunteer-class relays only; OPENRUNG_VOLUNTEER_TOKEN / OPENRUNG_FOUNDATION_TOKEN must be unset because Linode retains cloud-init user-data. A Foundation relay also needs a TLS broker, which this plaintext-origin helper does not use — install its credential post-boot over an authenticated channel instead." >&2
  exit 2
fi

if [ "${OPENRUNG_NODE_CLASS:-volunteer}" != "volunteer" ]; then
  echo "error: this helper provisions volunteer-class relays only; configure Foundation credentials post-boot instead of placing them in cloud-init user-data" >&2
  exit 2
fi

# Names come from the relay binary's own vocabulary, read from the canonical
# word lists rather than a copy kept here (see deploy/lib/relay-label.sh).
source "$(dirname -- "${BASH_SOURCE[0]}")/../lib/relay-label.sh"

NAME="${1:-$(openrung_random_label)}"

echo "Provisioning Linode relay '${NAME}' in ${REGION} (${TYPE}, ${OS_IMAGE})"

SSH_PUBKEY_FILE="${OPENRUNG_SSH_PUBKEY_FILE:-$HOME/.ssh/id_ed25519_openrung.pub}"
[ -f "$SSH_PUBKEY_FILE" ] || { echo "error: $SSH_PUBKEY_FILE not found; the box would be unreachable for the credential-install step" >&2; exit 1; }
SSH_PUBKEY="$(cat "$SSH_PUBKEY_FILE")"

# Firewall: default-deny inbound, allow SSH (22), the relay's public port (443),
# and ICMP, over both IPv4 and IPv6. Create once, then reuse by label.
FW_ID="$(linode-cli firewalls list --text --format 'id,label' 2>/dev/null | awk -F'\t' -v l="$FIREWALL_NAME" '$2==l{print $1; exit}')"
if [ -z "$FW_ID" ]; then
  FW_ID="$(linode-cli firewalls create \
    --label "$FIREWALL_NAME" \
    --rules.inbound_policy DROP \
    --rules.outbound_policy ACCEPT \
    --rules.inbound '[
      {"label":"ssh","action":"ACCEPT","protocol":"TCP","ports":"22","addresses":{"ipv4":["0.0.0.0/0"],"ipv6":["::/0"]}},
      {"label":"relay","action":"ACCEPT","protocol":"TCP","ports":"443","addresses":{"ipv4":["0.0.0.0/0"],"ipv6":["::/0"]}},
      {"label":"icmp","action":"ACCEPT","protocol":"ICMP","addresses":{"ipv4":["0.0.0.0/0"],"ipv6":["::/0"]}}
    ]' \
    --text --format id --no-headers)"
fi

# Cloud-init user-data: install Docker, pull the public image, run the relay. The
# public IP is not known until the instance exists, so the box self-discovers it
# from the Linode Metadata service at boot (token-authenticated, IMDSv2-style)
# and bakes it into OPENRUNG_PUBLIC_HOST, with the primary global address on the
# main interface as a fallback (Linode puts the public IPv4 directly on it).
#
# Container hardening — identical posture to the Lightsail and Hetzner relay
# helpers, and the exact posture the production fleet runs (ReadonlyRootfs=true,
# CapDrop=[ALL], CapAdd=[NET_BIND_SERVICE], while serving 443). The flags on the
# `docker run` line:
#   --cap-drop ALL --cap-add NET_BIND_SERVICE
#       Drop every capability, re-add only the one needed to bind the privileged
#       public port (443). The binary carries a cap_net_bind_service file
#       capability (setcap in the Dockerfile); NET_BIND_SERVICE keeps it in the
#       container's bounding set so the non-root `openrung` user can use it.
#   (deliberately NO --security-opt no-new-privileges)
#       no-new-privileges makes the kernel ignore file capabilities on exec,
#       which would break the 443 bind. Do NOT add it here. To harden with
#       no-new-privileges instead, serve a port >= 1024 and drop NET_BIND_SERVICE.
#   --read-only --tmpfs /tmp
#       Read-only rootfs; the only writable path the relay needs is the generated
#       xray config under /tmp. Xray logs to stdout (loglevel warning, no log
#       files), so nothing else is written.
USERDATA_FILE="$(mktemp)"
trap 'rm -f "$USERDATA_FILE"' EXIT
cat >"$USERDATA_FILE" <<EOF
#!/bin/bash
set -eu
exec > /var/log/openrung-init.log 2>&1
export DEBIAN_FRONTEND=noninteractive
# Linode images ship sshd with password auth enabled; the fleet is key-only.
# The drop-in must sort BEFORE any 50-cloud-init.conf a future image might
# write: sshd takes the FIRST value it encounters across lexically-ordered
# includes, so 01- wins where a 99- file would silently lose.
mkdir -p /etc/ssh/sshd_config.d
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' > /etc/ssh/sshd_config.d/01-openrung.conf
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
# DPkg::Lock::Timeout waits for cloud-init's own apt activity to release the lock.
apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io curl jq
systemctl enable --now docker
# Per-client fairness + bufferbloat control: shape egress with CAKE in
# dual-dsthost mode, so under contention every destination host (= client IP)
# gets an equal share of the link, while a lone client may still use the full
# rate (CAKE is work-conserving). besteffort collapses the DSCP tiers — tunnel
# traffic is all one class. Installed as a boot unit so the qdisc survives
# reboots. Best-effort: a shaping failure must never block relay bring-up.
cat > /usr/local/sbin/openrung-shape <<'SHAPESCRIPT'
#!/bin/sh
set -eu
RATE="${SHAPE_RATE}"
case "\$RATE" in ""|off|none) exit 0 ;; esac
DEV="\$(ip -o route get 1.1.1.1 | sed -n 's/.* dev \([^ ]*\).*/\1/p')"
[ -n "\$DEV" ] || { echo "openrung-shape: no default-route device found" >&2; exit 1; }
exec tc qdisc replace dev "\$DEV" root cake bandwidth "\$RATE" dual-dsthost besteffort
SHAPESCRIPT
chmod 0755 /usr/local/sbin/openrung-shape
cat > /etc/systemd/system/openrung-shape.service <<'SHAPEUNIT'
[Unit]
Description=OpenRung egress fairness shaping (CAKE dual-dsthost)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/openrung-shape

[Install]
WantedBy=multi-user.target
SHAPEUNIT
systemctl daemon-reload
systemctl enable --now openrung-shape.service || echo "warning: egress shaping unit failed; relay continues unshaped" >&2
# Public IPv4 from the Linode Metadata service; clients reach the relay here.
MDTOKEN="\$(curl -fsS -X PUT -H 'Metadata-Token-Expiry-Seconds: 300' http://169.254.169.254/v1/token || true)"
PUBLIC_IP=""
if [ -n "\$MDTOKEN" ]; then
  PUBLIC_IP="\$(curl -fsS -H "Metadata-Token: \$MDTOKEN" -H 'Accept: application/json' http://169.254.169.254/v1/network | jq -r '.ipv4.public[0] // empty' | cut -d/ -f1)"
fi
[ -n "\$PUBLIC_IP" ] || PUBLIC_IP="\$(ip -4 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | head -1)"
docker pull ${IMAGE}
docker rm -f openrung-relay 2>/dev/null || true
# Minted once per instance: the broker derives the relay ID from this seed
# (spec openrung-relay-identity-v1), so the relay keeps one identity across
# container restarts instead of fragmenting its dashboard/ranking history.
# The seed IS the relay's Ed25519 private key; disable xtrace before touching
# it so a future 'set -x' here can never trace it into the persisted
# /var/log/openrung-init.log (this block currently runs under 'set -eu').
set +x
IDENTITY_SEED="\$(head -c 32 /dev/urandom | base64)"
docker run -d --name openrung-relay --restart unless-stopped \\
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \\
  -e OPENRUNG_BROKER_URL=${BROKER_URL} \\
  -e OPENRUNG_PUBLIC_HOST="\$PUBLIC_IP" \\
  -e OPENRUNG_LISTEN_HOST=0.0.0.0 \\
  -e OPENRUNG_LABEL=${NAME} \\
  -e OPENRUNG_IDENTITY_SEED="\$IDENTITY_SEED" \\
  ${IMAGE}
EOF

# No root_pass: provisioning with authorized_keys alone leaves root's password
# LOCKED (verified on a probe instance: `passwd -S root` reports L, shadow
# field is `*`), strictly stronger than minting a throwaway password.
CREATED="$(linode-cli linodes create \
  --label "$NAME" \
  --region "$REGION" \
  --type "$TYPE" \
  --image "$OS_IMAGE" \
  --authorized_keys "$SSH_PUBKEY" \
  --firewall_id "$FW_ID" \
  --tags openrung \
  --metadata.user_data "$(base64 < "$USERDATA_FILE" | tr -d '\n')" \
  --text --format 'id,ipv4' --no-headers)"

LINODE_ID="$(printf '%s' "$CREATED" | awk -F'\t' '{print $1}')"
PUBLIC_IP="$(printf '%s' "$CREATED" | awk -F'\t' '{print $2}' | tr ',' '\n' | grep -v '^192\.168\.' | head -1)"
[ -n "$LINODE_ID" ] && [ -n "$PUBLIC_IP" ] || { echo "error: could not parse id/ip from linode-cli output: $CREATED" >&2; exit 1; }

echo "Waiting for instance ${LINODE_ID} to start..."
until [ "$(linode-cli linodes view "$LINODE_ID" --text --format status --no-headers 2>/dev/null)" = "running" ]; do
  sleep 5
done

echo "Done. '${NAME}' is at ${PUBLIC_IP}:443 and registers with ${BROKER_URL} after boot (~2-3 min)."
echo "  This helper launches an anonymous volunteer-class relay only; it rejects all registration tokens."
echo "  logs:  ssh -i ~/.ssh/id_ed25519_openrung root@${PUBLIC_IP} 'tail -f /var/log/openrung-init.log'"
echo "OPENRUNG_RELAY name=${NAME} ip=${PUBLIC_IP}"
