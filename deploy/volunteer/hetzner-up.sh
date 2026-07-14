#!/usr/bin/env bash
#
# Provision an OpenRung volunteer-run relay on Hetzner Cloud.
#
#   deploy/volunteer/hetzner-up.sh [name]
#
# If no name is given, a random "adjective-noun" name is generated and used as
# BOTH the Hetzner server name and the relay label (OPENRUNG_LABEL), so the relay
# shows up in the broker dashboard under the same friendly name as the box.
#
# Requires the `hcloud` CLI with an active context (`hcloud context list`); the
# API token is read from that context. The SSH public key at
# ~/.ssh/id_ed25519_openrung.pub is uploaded so the fleet's standard key can
# reach the box.
#
# Overridable via env: OPENRUNG_LOCATION, OPENRUNG_SERVER_TYPE, OPENRUNG_OS_IMAGE,
# OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_SSH_KEY_NAME,
# OPENRUNG_FIREWALL_NAME.
#
# This helper provisions anonymous volunteer-class relays only. It cannot safely
# accept any registration bearer: Hetzner retains cloud-init user-data. Provision
# an authenticated host first, then install its credential post-boot through an
# authenticated channel instead of passing it to this helper.
set -euo pipefail

LOCATION="${OPENRUNG_LOCATION:-hel1}"          # Helsinki (EU: 20TB included traffic)
SERVER_TYPE="${OPENRUNG_SERVER_TYPE:-cax11}"   # ARM Ampere, 2 vCPU / 4GB / 40GB
OS_IMAGE="${OPENRUNG_OS_IMAGE:-ubuntu-24.04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-volunteer:main}"  # multi-arch: pulls arm64 on CAX
# Register against the broker ORIGIN, not the Cloudflare front (broker.openrung.org).
# That hostname is a Worker front for *client* discovery; its edge serves a Managed
# Challenge to datacenter IP ranges (incl. Hetzner), which a relay's HTTP client
# cannot solve (403). The origin is plaintext HTTP and takes registrations directly,
# exactly like the Lightsail fleet (see lightsail-up.sh).
BROKER_URL="${OPENRUNG_BROKER_URL:-http://54.238.185.205:8080}"
SSH_KEY_NAME="${OPENRUNG_SSH_KEY_NAME:-openrung}"
FIREWALL_NAME="${OPENRUNG_FIREWALL_NAME:-openrung-volunteer}"

if [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_FOUNDATION_TOKEN+x}" = x ]; then
  echo "error: this helper provisions anonymous volunteer-class relays only; OPENRUNG_VOLUNTEER_TOKEN / OPENRUNG_FOUNDATION_TOKEN must be unset because Hetzner retains cloud-init user-data. A Foundation relay also needs a TLS broker, which this plaintext-origin helper does not use — install its credential post-boot over an authenticated channel instead." >&2
  exit 2
fi

if [ "${OPENRUNG_NODE_CLASS:-volunteer}" != "volunteer" ]; then
  echo "error: this helper provisions volunteer-class relays only; configure Foundation credentials post-boot instead of placing them in cloud-init user-data" >&2
  exit 2
fi

adjectives=(happy grumpy glorious sleepy brave clever gentle jolly mighty nimble plucky quiet rapid shiny snappy spry sturdy sunny swift witty zesty breezy cosmic dapper eager fuzzy golden hardy lucky merry noble proud quirky rustic silly valiant)
nouns=(hippo walrus castle otter falcon badger lantern comet maple harbor meadow beacon pebble willow cactus cobra ferret gecko heron ibex jaguar koala lemur marmot narwhal ocelot panther quokka raven salmon tapir urchin viper wombat yak zebra)

NAME="${1:-${adjectives[RANDOM % ${#adjectives[@]}]}-${nouns[RANDOM % ${#nouns[@]}]}}"

echo "Provisioning Hetzner relay '${NAME}' in ${LOCATION} (${SERVER_TYPE}, ${OS_IMAGE})"

# SSH key: upload the fleet's standard public key so the box is reachable with it.
# Idempotent — ignore "key already exists / name in use".
SSH_PUBKEY_FILE="${OPENRUNG_SSH_PUBKEY_FILE:-$HOME/.ssh/id_ed25519_openrung.pub}"
if [ -f "$SSH_PUBKEY_FILE" ]; then
  hcloud ssh-key create --name "$SSH_KEY_NAME" --public-key-from-file "$SSH_PUBKEY_FILE" >/dev/null 2>&1 || true
else
  echo "warning: $SSH_PUBKEY_FILE not found — creating server without an SSH key" >&2
  SSH_KEY_NAME=""
fi

# Firewall: default-deny inbound, allow SSH (22), the relay's public port (443),
# and ICMP, over both IPv4 and IPv6. Create once, then reuse. Rule adds are
# idempotent (a duplicate rule is a no-op error we swallow).
if ! hcloud firewall describe "$FIREWALL_NAME" >/dev/null 2>&1; then
  hcloud firewall create --name "$FIREWALL_NAME" >/dev/null
fi
hcloud firewall add-rule "$FIREWALL_NAME" --direction in --protocol tcp  --port 22  --source-ips 0.0.0.0/0 --source-ips ::/0 >/dev/null 2>&1 || true
hcloud firewall add-rule "$FIREWALL_NAME" --direction in --protocol tcp  --port 443 --source-ips 0.0.0.0/0 --source-ips ::/0 >/dev/null 2>&1 || true
hcloud firewall add-rule "$FIREWALL_NAME" --direction in --protocol icmp             --source-ips 0.0.0.0/0 --source-ips ::/0 >/dev/null 2>&1 || true

# Cloud-init user-data: install Docker, pull the public image, run the relay. The
# public IP is not known until the server exists, so the box self-discovers it
# from Hetzner's metadata service at boot and bakes it into OPENRUNG_PUBLIC_HOST.
#
# Container hardening — identical posture to the Lightsail relay helper, and
# the exact posture the production fleet runs (confirmed read-only via
# `docker inspect` on a live Hetzner relay 2026-07-13: ReadonlyRootfs=true,
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
# DPkg::Lock::Timeout waits for cloud-init's own apt activity to release the lock.
apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io curl
systemctl enable --now docker
# Public IPv4 from the Hetzner metadata service; clients reach the relay here.
PUBLIC_IP="\$(curl -fsS http://169.254.169.254/hetzner/v1/metadata/public-ipv4)"
docker pull ${IMAGE}
docker rm -f openrung-volunteer 2>/dev/null || true
docker run -d --name openrung-volunteer --restart unless-stopped \\
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \\
  -e OPENRUNG_BROKER_URL=${BROKER_URL} \\
  -e OPENRUNG_PUBLIC_HOST="\$PUBLIC_IP" \\
  -e OPENRUNG_LISTEN_HOST=0.0.0.0 \\
  -e OPENRUNG_LABEL=${NAME} \\
  ${IMAGE}
EOF

SSH_ARG=()
if [ -n "$SSH_KEY_NAME" ]; then SSH_ARG=(--ssh-key "$SSH_KEY_NAME"); fi

hcloud server create \
  --name "$NAME" \
  --type "$SERVER_TYPE" \
  --image "$OS_IMAGE" \
  --location "$LOCATION" \
  --firewall "$FIREWALL_NAME" \
  --label openrung=volunteer \
  "${SSH_ARG[@]}" \
  --user-data-from-file "$USERDATA_FILE" >/dev/null

PUBLIC_IP="$(hcloud server ip "$NAME")"

echo "Done. '${NAME}' is at ${PUBLIC_IP}:443 and registers with ${BROKER_URL} after boot (~2-3 min)."
echo "  This helper launches an anonymous volunteer-class relay only; it rejects all registration tokens."
echo "  logs:  ssh -i ~/.ssh/id_ed25519_openrung root@${PUBLIC_IP} 'tail -f /var/log/openrung-init.log'"
echo "OPENRUNG_RELAY name=${NAME} ip=${PUBLIC_IP}"
