#!/usr/bin/env bash
#
# Provision an OpenRung relay hub on AWS EC2 with TWO public IPs, so the NAT
# hole-punch reflector can classify a peer's NAT (RFC 5780) from two distinct
# vantage points.
#
#   deploy/relayhub/ec2-up.sh [name]
#
# Why EC2 and not Lightsail: Lightsail gives only one public IPv4 per instance,
# and its public IP is 1:1-NAT'd (not on the NIC). EC2 lets us put a secondary
# private IP on the ENI and associate a second Elastic IP, giving two public IPs
# that both map to on-NIC private addresses the reflector can bind. The reflector
# BINDS the private IPs and ADVERTISES the EIPs (see the bind/advertise split in
# internal/relayhub + deploy/relayhub/README.md).
#
# It launches an instance in the default VPC, assigns a secondary private IP,
# allocates + associates two EIPs, self-signs a control-channel TLS cert (SANs =
# both EIPs), and stages the hub with punch enabled. It starts immediately only
# after an explicit anonymous-mode opt-in. A boot-time systemd unit keeps the
# secondary private IP configured on the interface across reboots.
#
# Prerequisites: authenticated `aws` CLI with EC2 permissions, and the GHCR image
# published + PUBLIC.
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_EC2_SUBNET, OPENRUNG_EC2_TYPE,
# OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_HUB_CONTROL_PORT,
# OPENRUNG_HUB_HTTP_PORT, OPENRUNG_HUB_PORT_RANGE, OPENRUNG_HUB_REFLECTOR_PORT,
# OPENRUNG_KEY_NAME, OPENRUNG_KEY_FILE, OPENRUNG_SG_NAME,
# OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS.
#
# This helper never accepts a bearer token. EC2 retains user-data and exposes it
# through IMDS to local processes, including containers using host networking.
# By default it stages the host without starting the hub; explicitly set
# OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true to launch an open hub instead.
set -euo pipefail

if [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ]; then
  echo "error: OPENRUNG_VOLUNTEER_TOKEN is not accepted because EC2 user-data persists and is readable through IMDS; provision first, then install it post-boot over authenticated SSH" >&2
  exit 2
fi

case "${OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS:-}" in
  ''|false) ALLOW_ANONYMOUS=false ;;
  true) ALLOW_ANONYMOUS=true ;;
  *)
    echo "error: OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS must be exactly true or false" >&2
    exit 2
    ;;
esac

# Local temporary files can include newly-created SSH private-key material.
umask 077

REGION="${OPENRUNG_REGION:-ap-northeast-2}"          # Seoul
ITYPE="${OPENRUNG_EC2_TYPE:-t4g.micro}"              # ARM Graviton (cheapest)
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relayhub:main@sha256:a2b0a868146bd9285a935aeb10e7a8f39e4e4033d735220b3fc83679844de387}"
[[ "$IMAGE" =~ @sha256:[a-f0-9]{64}$ ]] || { echo "error: OPENRUNG_IMAGE must be pinned to an immutable sha256 digest" >&2; exit 2; }
# The hub registers every tunnelled relay on its behalf, so this is the fleet's
# highest-volume registration path. Point it at the broker's TLS origin — a
# DNS-only (grey-cloud) record straight to the broker box, so a datacenter IP
# reaches it without the Cloudflare front's Managed Challenge while the
# registrations stop crossing the public internet in cleartext.
BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
case "$BROKER_URL" in
  http://*|https://*) ;;
  *) echo "error: OPENRUNG_BROKER_URL must be an http(s) base URL" >&2; exit 2 ;;
esac
case "$BROKER_URL" in
  *[[:space:]]*|*'?'*|*'#'*)
    echo "error: OPENRUNG_BROKER_URL must not contain whitespace, a query, or a fragment; user-data is persistent" >&2
    exit 2
    ;;
esac
BROKER_AUTHORITY="${BROKER_URL#*://}"
BROKER_AUTHORITY="${BROKER_AUTHORITY%%/*}"
case "$BROKER_AUTHORITY" in
  ''|*@*)
    echo "error: OPENRUNG_BROKER_URL must have a host and no userinfo; user-data is persistent" >&2
    exit 2
    ;;
esac
CONTROL_PORT="${OPENRUNG_HUB_CONTROL_PORT:-9443}"
HTTP_PORT="${OPENRUNG_HUB_HTTP_PORT:-9444}"
PORT_RANGE="${OPENRUNG_HUB_PORT_RANGE:-20000-20100}"
REFLECTOR_PORT="${OPENRUNG_HUB_REFLECTOR_PORT:-19302}"
KEY_NAME="${OPENRUNG_KEY_NAME:-openrung}"
# Local private key used to SSH into the instance and (below) imported as the EC2
# key pair when it does not exist yet, so hosts share the fleet-standard key.
KEY_FILE="${OPENRUNG_KEY_FILE:-$HOME/.ssh/id_ed25519_openrung}"
SG_NAME="${OPENRUNG_SG_NAME:-openrung-relayhub}"

RANGE_START="${PORT_RANGE%%-*}"
RANGE_END="${PORT_RANGE##*-}"
NAME="${1:-hub-ec2-$RANDOM}"

# ARM types (t4g/c7g/m7g/...) need the arm64 AMI; everything else amd64.
case "$ITYPE" in
  *g.*|*gd.*) ARCH="arm64" ;;
  *) ARCH="amd64" ;;
esac
AMI="$(aws ssm get-parameter --region "$REGION" \
  --name "/aws/service/canonical/ubuntu/server/24.04/stable/current/${ARCH}/hvm/ebs-gp3/ami-id" \
  --query 'Parameter.Value' --output text)"

VPC="$(aws ec2 describe-vpcs --region "$REGION" --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)"
SUBNET="${OPENRUNG_EC2_SUBNET:-$(aws ec2 describe-subnets --region "$REGION" --filters Name=vpc-id,Values=$VPC --query 'Subnets[0].SubnetId' --output text)}"

echo "Provisioning EC2 relay hub '${NAME}' in ${REGION} (${ITYPE}/${ARCH}, ami ${AMI})"

# --- security group (idempotent) ---
SG_ID="$(aws ec2 describe-security-groups --region "$REGION" \
  --filters "Name=group-name,Values=${SG_NAME}" "Name=vpc-id,Values=${VPC}" \
  --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)"
if [ -z "$SG_ID" ] || [ "$SG_ID" = "None" ]; then
  SG_ID="$(aws ec2 create-security-group --region "$REGION" --group-name "$SG_NAME" \
    --description "OpenRung relay hub" --vpc-id "$VPC" --query GroupId --output text)"
  aws ec2 authorize-security-group-ingress --region "$REGION" --group-id "$SG_ID" --ip-permissions \
    "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${CONTROL_PORT},ToPort=${CONTROL_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${HTTP_PORT},ToPort=${HTTP_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${RANGE_START},ToPort=${RANGE_END},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=udp,FromPort=${REFLECTOR_PORT},ToPort=${REFLECTOR_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" >/dev/null
fi
echo "Security group: ${SG_ID}"

# --- key pair (idempotent) ---
# Prefer importing the fleet-standard public key (derived from $KEY_FILE) so every
# host is reachable with the same key. Only generate a fresh key pair as a last
# resort, and never overwrite an existing local private key. An existing EC2 key
# name must contain the public key derived from the configured private key: the
# authenticated post-boot token handoff depends on that key actually working.
if ! EC2_PUBLIC_KEY="$(aws ec2 describe-key-pairs --region "$REGION" \
  --key-names "$KEY_NAME" --include-public-key \
  --query 'KeyPairs[0].PublicKey' --output text 2>/dev/null)"; then
  if [ -f "$KEY_FILE" ] && PUBKEY="$(ssh-keygen -y -f "$KEY_FILE" 2>/dev/null)" && [ -n "$PUBKEY" ]; then
    aws ec2 import-key-pair --region "$REGION" --key-name "$KEY_NAME" \
      --public-key-material "$(printf '%s' "$PUBKEY" | base64)" >/dev/null
    echo "Imported key pair '${KEY_NAME}' from ${KEY_FILE}"
  elif [ -e "$KEY_FILE" ]; then
    echo "ERROR: ${KEY_FILE} exists but is not a usable SSH private key; set OPENRUNG_KEY_FILE" >&2
    exit 1
  else
    mkdir -p "$(dirname "$KEY_FILE")"
    KEY_TMP="$(mktemp "${KEY_FILE}.tmp.XXXXXX")"
    trap '[ -z "${KEY_TMP:-}" ] || rm -f -- "$KEY_TMP"' EXIT HUP INT TERM
    if ! aws ec2 create-key-pair --region "$REGION" --key-name "$KEY_NAME" \
      --query KeyMaterial --output text > "$KEY_TMP"; then
      echo "ERROR: failed to create EC2 key pair '${KEY_NAME}'; no local private key was installed" >&2
      exit 1
    fi
    chmod 0600 "$KEY_TMP"
    if ! ln "$KEY_TMP" "$KEY_FILE"; then
      echo "ERROR: ${KEY_FILE} appeared while creating '${KEY_NAME}'; refusing to overwrite it" >&2
      exit 1
    fi
    rm -f -- "$KEY_TMP"
    KEY_TMP=""
    trap - EXIT HUP INT TERM
    echo "Created key pair, private key at ${KEY_FILE}"
  fi
else
  if [ ! -f "$KEY_FILE" ] || ! LOCAL_PUBLIC_KEY="$(ssh-keygen -y -f "$KEY_FILE" 2>/dev/null)"; then
    echo "ERROR: EC2 key pair '${KEY_NAME}' already exists, but ${KEY_FILE} is missing or unusable; authenticated post-boot setup would be unreachable" >&2
    exit 1
  fi
  read -r EC2_KEY_TYPE EC2_KEY_BLOB _ <<< "$EC2_PUBLIC_KEY"
  read -r LOCAL_KEY_TYPE LOCAL_KEY_BLOB _ <<< "$LOCAL_PUBLIC_KEY"
  if [ -z "${EC2_KEY_BLOB:-}" ] || [ "$LOCAL_KEY_TYPE" != "$EC2_KEY_TYPE" ] || [ "$LOCAL_KEY_BLOB" != "$EC2_KEY_BLOB" ]; then
    echo "ERROR: ${KEY_FILE} does not match existing EC2 key pair '${KEY_NAME}'; authenticated post-boot setup would be unreachable" >&2
    exit 1
  fi
fi

# --- two Elastic IPs ---
read -r ALLOC1 EIP1 < <(aws ec2 allocate-address --region "$REGION" --domain vpc --query '[AllocationId,PublicIp]' --output text)
read -r ALLOC2 EIP2 < <(aws ec2 allocate-address --region "$REGION" --domain vpc --query '[AllocationId,PublicIp]' --output text)
echo "Elastic IPs: ${EIP1} (primary) + ${EIP2} (reflector-2)"

# --- cloud-init: configure secondary IP, TLS, and a staged/anonymous hub ---
UD="$(mktemp)"
RENDERED_UD="$(mktemp)"
trap 'rm -f "$UD" "$RENDERED_UD"' EXIT
cat > "$UD" <<'TMPL'
#!/bin/bash
set -eu
# Secrets are written later in this bootstrap. Keep newly-created files private
# by default, then explicitly relax only the public certificate and directories
# that the unprivileged relayhub container must traverse.
umask 077
exec > /var/log/openrung-init.log 2>&1
export DEBIAN_FRONTEND=noninteractive

# Keep every ENI private IP configured on the interface across reboots (AWS does
# not auto-configure secondary private IPs on the OS; the reflector binds them).
cat > /usr/local/bin/openrung-secondary-ips.sh <<'SCRIPT'
#!/bin/bash
set -eu
IMDS_TOKEN=$(curl -sS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 600")
imds() { curl -sS -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
MAC=$(imds network/interfaces/macs/ | head -1 | tr -d /)
PRIMARY=$(imds local-ipv4)
CIDR=$(imds "network/interfaces/macs/$MAC/subnet-ipv4-cidr-block")
PREFIX=${CIDR##*/}
IFACE=$(ip -o link show | awk -F': ' '/: (en|eth)/{print $2; exit}')
for ip in $(imds "network/interfaces/macs/$MAC/local-ipv4s"); do
  [ "$ip" = "$PRIMARY" ] && continue
  ip addr add "$ip/$PREFIX" dev "$IFACE" 2>/dev/null || true
done
SCRIPT
chmod +x /usr/local/bin/openrung-secondary-ips.sh
cat > /etc/systemd/system/openrung-secondary-ips.service <<'UNIT'
[Unit]
Description=Add EC2 secondary private IPs to the primary interface
After=network-online.target
Wants=network-online.target
Before=docker.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/openrung-secondary-ips.sh
[Install]
WantedBy=multi-user.target
UNIT

apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io openssl
systemctl daemon-reload
systemctl enable --now openrung-secondary-ips.service
systemctl enable --now docker

IMDS_TOKEN=$(curl -sS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 600")
imds() { curl -sS -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
MAC=$(imds network/interfaces/macs/ | head -1 | tr -d /)
PRIMARY=$(imds local-ipv4)
SECONDARY=$(imds "network/interfaces/macs/$MAC/local-ipv4s" | grep -v "^$PRIMARY$" | head -1)

install -d -m 0755 /etc/openrung/certs
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout /etc/openrung/certs/hub.key -out /etc/openrung/certs/hub.crt \
  -subj "/CN=__EIP1__" -addext "subjectAltName=IP:__EIP1__,IP:__EIP2__"
chmod 0600 /etc/openrung/certs/hub.key
chmod 0644 /etc/openrung/certs/hub.crt

cat > /etc/openrung/relayhub.env <<ENV
OPENRUNG_HUB_PUBLIC_HOST=__EIP1__
OPENRUNG_BROKER_URL=__BROKER_URL__
OPENRUNG_HUB_CONTROL_ADDR=:__CONTROL_PORT__
OPENRUNG_HUB_PORT_RANGE=__PORT_RANGE__
OPENRUNG_HUB_HTTP_ADDR=:__HTTP_PORT__
OPENRUNG_HUB_TLS_CERT=/etc/openrung/certs/hub.crt
OPENRUNG_HUB_TLS_KEY=/etc/openrung/certs/hub.key
OPENRUNG_HUB_REFLECTOR_ADDRS=$PRIMARY:__REFLECTOR_PORT__,$SECONDARY:__REFLECTOR_PORT__
OPENRUNG_HUB_REFLECTOR_ADVERTISE=__EIP1__:__REFLECTOR_PORT__,__EIP2__:__REFLECTOR_PORT__
__ANON_ENV__
ENV
chown root:root /etc/openrung/relayhub.env
chmod 0600 /etc/openrung/relayhub.env

docker pull __IMAGE__
# The image runs as its unprivileged openrung user. Preserve mode 0600 while
# making the key readable by that user through the read-only bind mount.
if ! HUB_UID=$(docker run --rm --entrypoint id __IMAGE__ -u); then
  echo "could not resolve relayhub image UID" >&2
  exit 1
fi
case "$HUB_UID" in
  ''|*[!0-9]*) echo "invalid relayhub image UID: $HUB_UID" >&2; exit 1 ;;
  0) echo "relayhub image must run as a non-root user" >&2; exit 1 ;;
esac
chown "$HUB_UID" /etc/openrung/certs/hub.key
chmod 0600 /etc/openrung/certs/hub.key

cat > /usr/local/sbin/openrung-relayhub-start <<'SCRIPT'
#!/bin/sh
set -eu
unset OPENRUNG_VOLUNTEER_TOKEN OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS
ENV_FILE=/etc/openrung/relayhub.env
if [ "$(id -u)" -ne 0 ]; then
  echo "openrung-relayhub-start must run as root" >&2
  exit 1
fi
if [ "$(stat -c '%u:%a' "$ENV_FILE" 2>/dev/null || true)" != "0:600" ]; then
  echo "$ENV_FILE must be owned by root with mode 0600" >&2
  exit 1
fi
AUTH_LINE_COUNT=$(grep -Ec '^OPENRUNG_(VOLUNTEER_TOKEN|ALLOW_ANONYMOUS_VOLUNTEERS)=' "$ENV_FILE" || true)
TOKEN_LINE_COUNT=$(grep -Ec '^OPENRUNG_VOLUNTEER_TOKEN=.+$' "$ENV_FILE" || true)
ANON_LINE_COUNT=$(grep -Ec '^OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true$' "$ENV_FILE" || true)
if [ "$AUTH_LINE_COUNT" -ne 1 ] || { [ "$TOKEN_LINE_COUNT" -ne 1 ] && [ "$ANON_LINE_COUNT" -ne 1 ]; }; then
  echo "$ENV_FILE must contain exactly one non-empty token or explicit anonymous opt-in" >&2
  exit 1
fi
docker run -d --name openrung-relayhub --restart unless-stopped \
  --network host --cap-drop ALL --read-only --tmpfs /tmp \
  -v /etc/openrung/certs:/etc/openrung/certs:ro \
  --env-file /etc/openrung/relayhub.env \
  __IMAGE__
SCRIPT
chmod 0700 /usr/local/sbin/openrung-relayhub-start

cat > /usr/local/sbin/openrung-relayhub-install-token <<'SCRIPT'
#!/bin/sh
set -eu
umask 077
ENV_FILE=/etc/openrung/relayhub.env
unset OPENRUNG_VOLUNTEER_TOKEN OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS VOLUNTEER_TOKEN EXTRA_INPUT
if [ "$(id -u)" -ne 0 ]; then
  echo "openrung-relayhub-install-token must run as root" >&2
  exit 1
fi
if [ ! -f "$ENV_FILE" ]; then
  echo "$ENV_FILE is missing; wait for cloud-init to finish" >&2
  exit 1
fi
if docker container inspect openrung-relayhub >/dev/null 2>&1; then
  echo "token installer is for a staged first start only; an openrung-relayhub container already exists" >&2
  exit 1
fi
ENV_TMP="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
trap 'unset VOLUNTEER_TOKEN EXTRA_INPUT; [ -z "${ENV_TMP:-}" ] || rm -f -- "$ENV_TMP"' EXIT HUP INT TERM
awk '!/^OPENRUNG_VOLUNTEER_TOKEN=/ && !/^OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=/' \
  "$ENV_FILE" > "$ENV_TMP"
chown root:root "$ENV_TMP"
chmod 0600 "$ENV_TMP"
if ! IFS= read -r VOLUNTEER_TOKEN || [ -z "$VOLUNTEER_TOKEN" ]; then
  unset VOLUNTEER_TOKEN
  echo "expected one non-empty volunteer bearer token on stdin" >&2
  exit 2
fi
case "$VOLUNTEER_TOKEN" in
  *[!A-Za-z0-9._~+/=-]*)
    unset VOLUNTEER_TOKEN
    echo "volunteer token contains characters that are not valid in an HTTP bearer token" >&2
    exit 2
    ;;
esac
EXTRA_INPUT=""
if IFS= read -r EXTRA_INPUT || [ -n "$EXTRA_INPUT" ]; then
  unset VOLUNTEER_TOKEN EXTRA_INPUT
  echo "expected exactly one token line on stdin" >&2
  exit 2
fi
unset EXTRA_INPUT
if ! printf 'OPENRUNG_VOLUNTEER_TOKEN=%s\n' "$VOLUNTEER_TOKEN" >> "$ENV_TMP"; then
  unset VOLUNTEER_TOKEN
  echo "could not write volunteer token to the private environment file" >&2
  exit 1
fi
unset VOLUNTEER_TOKEN
mv "$ENV_TMP" "$ENV_FILE"
ENV_TMP=""
trap - EXIT HUP INT TERM
exec /usr/local/sbin/openrung-relayhub-start
SCRIPT
chmod 0700 /usr/local/sbin/openrung-relayhub-install-token
__AUTO_START__
TMPL

ANON_ENV=""
AUTO_START='echo "relayhub staged without authentication; install a token post-boot before starting it"'
if [ "$ALLOW_ANONYMOUS" = true ]; then
  ANON_ENV="OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true"
  AUTO_START=/usr/local/sbin/openrung-relayhub-start
fi

escape_sed_replacement() {
  printf '%s' "$1" | sed 's/[\\&#]/\\&/g'
}
IMAGE_REPLACEMENT="$(escape_sed_replacement "$IMAGE")"
BROKER_URL_REPLACEMENT="$(escape_sed_replacement "$BROKER_URL")"
ANON_ENV_REPLACEMENT="$(escape_sed_replacement "$ANON_ENV")"
AUTO_START_REPLACEMENT="$(escape_sed_replacement "$AUTO_START")"

sed \
  -e "s#__IMAGE__#${IMAGE_REPLACEMENT}#g" \
  -e "s#__BROKER_URL__#${BROKER_URL_REPLACEMENT}#g" \
  -e "s/__EIP1__/${EIP1}/g" -e "s/__EIP2__/${EIP2}/g" \
  -e "s/__CONTROL_PORT__/${CONTROL_PORT}/g" -e "s/__HTTP_PORT__/${HTTP_PORT}/g" \
  -e "s/__PORT_RANGE__/${PORT_RANGE}/g" -e "s/__REFLECTOR_PORT__/${REFLECTOR_PORT}/g" \
  -e "s#__ANON_ENV__#${ANON_ENV_REPLACEMENT}#g" \
  -e "s#__AUTO_START__#${AUTO_START_REPLACEMENT}#g" \
  "$UD" > "$RENDERED_UD"
mv "$RENDERED_UD" "$UD"

# --- launch (primary private IP + one secondary; auto public IP for boot egress) ---
IID="$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI" --instance-type "$ITYPE" --key-name "$KEY_NAME" \
  --network-interfaces "DeviceIndex=0,SubnetId=${SUBNET},Groups=${SG_ID},AssociatePublicIpAddress=true,SecondaryPrivateIpAddressCount=1" \
  --user-data "file://${UD}" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NAME}}]" \
  --metadata-options 'HttpTokens=required,HttpEndpoint=enabled' \
  --query 'Instances[0].InstanceId' --output text)"
echo "Instance: ${IID}, waiting for running..."
aws ec2 wait instance-running --instance-ids "$IID" --region "$REGION"

ENI="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].NetworkInterfaceId' --output text)"
PRIMARY_IP="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].PrivateIpAddresses[?Primary==`true`].PrivateIpAddress | [0]' --output text)"
SECONDARY_IP="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].PrivateIpAddresses[?Primary==`false`].PrivateIpAddress | [0]' --output text)"

# Associate EIP1 -> primary, EIP2 -> secondary (order matches the env's
# REFLECTOR_ADDRS[primary,secondary] / REFLECTOR_ADVERTISE[EIP1,EIP2]).
aws ec2 associate-address --region "$REGION" --allocation-id "$ALLOC1" --network-interface-id "$ENI" --private-ip-address "$PRIMARY_IP" >/dev/null
aws ec2 associate-address --region "$REGION" --allocation-id "$ALLOC2" --network-interface-id "$ENI" --private-ip-address "$SECONDARY_IP" >/dev/null

echo "Provisioned hub host '${NAME}' (${IID}):"
echo "  control  ${EIP1}:${CONTROL_PORT} (TLS, self-signed)"
echo "  http api ${EIP1}:${HTTP_PORT} (reachability prober + punch coordinator)"
echo "  reflector UDP ${EIP1}:${REFLECTOR_PORT} + ${EIP2}:${REFLECTOR_PORT}"
echo "  tunnels  ${EIP1}:${PORT_RANGE}"
if [ "$ALLOW_ANONYMOUS" = true ]; then
  echo "  authentication: OPEN / anonymous (explicit opt-in)"
  echo "It registers tunneled relays with ${BROKER_URL} after boot (~2-3 min)."
  echo "Verify: curl -k https://${EIP1}:${HTTP_PORT}/api/v1/punch/config"
else
  echo "  status: STAGED; relayhub is not running"
  echo "Verify and pin the SSH host key out of band, then send exactly one token line"
  echo "over SSH stdin after cloud-init completes:"
  echo "  ssh -o StrictHostKeyChecking=yes -i ${KEY_FILE} ubuntu@${EIP1} \\"
  echo "    'sudo cloud-init status --wait >/dev/null && sudo /usr/local/sbin/openrung-relayhub-install-token'"
fi
echo "OPENRUNG_HUB name=${NAME} eip1=${EIP1} eip2=${EIP2} instance=${IID}"
