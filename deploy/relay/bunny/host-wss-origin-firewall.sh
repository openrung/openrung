#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Restrict one relay host's WSS origin port to bunny.net's current edge address
# list with a host nftables table.
#
#   host-wss-origin-firewall.sh check|apply RELAY HOST
#
# For providers whose own firewall cannot hold bunny's list, such as Lightsail
# (60 source ranges per port).  The provider firewall opens the origin port and
# this table drops every source that is not a bunny edge, one layer down.
#
# `check` prints the change without touching the host.  `apply` loads the new
# table with the host's own nft as one transaction, saves it, and installs a
# boot unit that reloads it.  Any fetch, parse, or plausibility failure leaves
# the host exactly as it was.
#
# The allowlist proves only that a connection came from some bunny edge; the
# sidecar's per-front origin token is what authenticates the front.
set -euo pipefail
{ set +x; } 2>/dev/null

MODE="${1:-}"
RELAY="${2:-}"
HOST="${3:-}"
ORIGIN_PORT="${OPENRUNG_WSS_ORIGIN_PORT:-8443}"
EDGE_LIST_URL="${OPENRUNG_BUNNY_EDGE_LIST_URL:-https://api.bunny.net/system/edgeserverlist}"
# A unique IPv4 count outside this range means the feed changed shape or was
# truncated.
MIN_EDGE_ADDRESSES="${OPENRUNG_MIN_EDGE_ADDRESSES:-100}"
MAX_EDGE_ADDRESSES="${OPENRUNG_MAX_EDGE_ADDRESSES:-5000}"
SSH_KEY="${OPENRUNG_SSH_KEY:-$HOME/.ssh/id_ed25519_openrung}"
SSH_USER="${OPENRUNG_SSH_USER:-ubuntu}"
KNOWN_HOSTS="${OPENRUNG_KNOWN_HOSTS:-$HOME/.ssh/known_hosts}"
SSH_OPTS=(-i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=${KNOWN_HOSTS}")

readonly RULES=/etc/openrung/wss-origin-firewall.nft
readonly UNIT=/etc/systemd/system/openrung-wss-origin-firewall.service
readonly TABLE=openrung_wss_origin

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$MODE" == check || "$MODE" == apply ]] || die "usage: $0 check|apply RELAY HOST"
[[ "$RELAY" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "relay name is invalid"
[[ "$HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.:-]{0,254}$ ]] || die "host is invalid"
[[ "$ORIGIN_PORT" =~ ^[1-9][0-9]{0,4}$ ]] && ((10#$ORIGIN_PORT <= 65535)) || die "origin port is invalid"
command -v python3 >/dev/null || die "python3 is required"
[[ -f "$SSH_KEY" && -f "$KNOWN_HOSTS" ]] || die "SSH key or known_hosts is missing"
ssh-keygen -F "$HOST" -f "$KNOWN_HOSTS" >/dev/null || die "host key is not pinned: ${HOST}"

ssh_run() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${HOST}" "$@"; }

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
umask 077

# Certificate verification is the only thing standing between this feed and an
# attacker-chosen allowlist, so never relax it.  --disable has to be the first
# argument or curl may read an insecure option from a user or system curlrc.
curl --disable --fail --silent --show-error --proto '=https' --tlsv1.2 \
  --max-time 30 --retry 2 --retry-delay 2 \
  -o "${TMP_DIR}/edges.json" "$EDGE_LIST_URL" \
  || die "could not fetch the bunny edge list; host left unchanged"

python3 - "${TMP_DIR}/edges.json" "${TMP_DIR}/ruleset.nft" "${TMP_DIR}/ranges.txt" \
  "$ORIGIN_PORT" "$MIN_EDGE_ADDRESSES" "$MAX_EDGE_ADDRESSES" "$RELAY" "$TABLE" <<'PY'
import ipaddress, json, sys

edges_path, ruleset_path, ranges_path, port, minimum, maximum, relay, table = sys.argv[1:]
minimum, maximum = int(minimum), int(maximum)
if not 1 <= minimum <= maximum:
    raise SystemExit("edge address bounds are invalid")
try:
    with open(edges_path, encoding="utf-8") as handle:
        edges = json.load(handle)
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit(f"edge list is not valid JSON: {exc}") from None
if not isinstance(edges, list) or not all(isinstance(entry, str) for entry in edges):
    raise SystemExit("edge list is not an array of address strings")

addresses = set()
for entry in edges:
    try:
        address = ipaddress.ip_address(entry.strip())
    except ValueError:
        raise SystemExit(f"edge list contains an invalid IP address: {entry!r}") from None
    # The origin hostname publishes only an A record, so the edge reaches the
    # relay over IPv4; IPv6 to the origin port is dropped outright.
    if address.version == 4:
        addresses.add(ipaddress.ip_network(address))
# Bound the unique addresses that can enter the set, not the raw feed length,
# which duplicates or ignored IPv6 entries could inflate.
if not minimum <= len(addresses) <= maximum:
    raise SystemExit(f"edge list has an implausible unique IPv4 size: {len(addresses)}")

ranges = [str(net) for net in ipaddress.collapse_addresses(addresses)]
with open(ranges_path, "w", encoding="utf-8") as handle:
    handle.write("".join(f"{r}\n" for r in ranges))

elements = ",\n".join(f"      {r}" for r in ranges)
# Declaring and then deleting the table first makes one `nft -f` both create
# and fully replace it in a single transaction.  The drop is not family
# qualified, so IPv6 to the origin port is refused as well.
with open(ruleset_path, "w", encoding="utf-8") as handle:
    handle.write(f"""# OpenRung WSS origin allowlist for {relay}: bunny.net edges only.
# Managed by deploy/relay/bunny/host-wss-origin-firewall.sh; do not edit.
table inet {table}
delete table inet {table}
table inet {table} {{
  set bunny_v4 {{
    type ipv4_addr
    flags interval
    elements = {{
{elements}
    }}
  }}
  chain input {{
    type filter hook input priority filter - 1; policy accept;
    tcp dport {port} ip saddr @bunny_v4 accept
    tcp dport {port} drop
  }}
}}
""")
PY

# The boot unit only reloads the saved table.  It has no ExecStop: stopping it
# must never reopen the origin port.
cat >"${TMP_DIR}/unit" <<UNIT
[Unit]
Description=OpenRung WSS origin allowlist (nftables)
After=network-pre.target
Before=network-online.target caddy.service
ConditionPathExists=${RULES}

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f ${RULES}
ExecReload=/usr/sbin/nft -f ${RULES}

[Install]
WantedBy=multi-user.target
UNIT

ssh_run "sudo cat ${RULES} 2>/dev/null || true" </dev/null >"${TMP_DIR}/current.nft"
grep -Eo '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+' "${TMP_DIR}/current.nft" | sort -u >"${TMP_DIR}/current-ranges.txt" || true
sort -u "${TMP_DIR}/ranges.txt" >"${TMP_DIR}/new-ranges.txt"
comm -13 "${TMP_DIR}/current-ranges.txt" "${TMP_DIR}/new-ranges.txt" | sed 's/^/+ /'
comm -23 "${TMP_DIR}/current-ranges.txt" "${TMP_DIR}/new-ranges.txt" | sed 's/^/- /'
COUNT="$(wc -l <"${TMP_DIR}/ranges.txt" | tr -d ' ')"

if [[ "$MODE" == check ]]; then
  if cmp -s "${TMP_DIR}/current.nft" "${TMP_DIR}/ruleset.nft"; then saved=current; else saved=drift; fi
  state="$(ssh_run "sudo nft list table inet ${TABLE} >/dev/null 2>&1 && echo loaded=yes || echo loaded=no
    echo unit=\$(systemctl is-enabled openrung-wss-origin-firewall.service 2>/dev/null || echo missing)" </dev/null)"
  printf 'relay=%s ranges=%s rules=%s %s\n' "$RELAY" "$COUNT" "$saved" "$(tr '\n' ' ' <<<"$state" | sed 's/ $//')"
  exit 0
fi

ssh_run "set -e
  sudo install -d -m 0700 -o root -g root /etc/openrung
  sudo rm -f ${RULES}.new && sudo install -m 0600 -o root -g root /dev/stdin ${RULES}.new" <"${TMP_DIR}/ruleset.nft"
ssh_run "sudo rm -f ${UNIT}.new && sudo install -m 0644 -o root -g root /dev/stdin ${UNIT}.new" <"${TMP_DIR}/unit"
# Load before saving: a failed load leaves the running table and the saved
# copy untouched.
ssh_run "set -e
  trap 'sudo rm -f ${RULES}.new ${UNIT}.new' EXIT
  sudo nft -c -f ${RULES}.new
  sudo nft -f ${RULES}.new
  sudo mv ${RULES}.new ${RULES}
  sudo mv ${UNIT}.new ${UNIT}
  sudo systemctl daemon-reload
  sudo systemctl enable openrung-wss-origin-firewall.service >/dev/null 2>&1
  sudo nft list chain inet ${TABLE} input | grep -q 'tcp dport ${ORIGIN_PORT} drop'"
printf 'relay=%s ranges=%s rules=applied\n' "$RELAY" "$COUNT"
