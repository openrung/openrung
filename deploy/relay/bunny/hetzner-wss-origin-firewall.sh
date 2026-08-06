#!/usr/bin/env bash
# Converge one Hetzner Cloud firewall's WSS origin port to bunny.net's current
# edge address list.
#
#   hetzner-wss-origin-firewall.sh check|apply FIREWALL_NAME
#
# `check` prints the change without touching the firewall.  `apply` replaces
# the rule set atomically.  Only inbound TCP rules on the exact origin port are
# managed.  Every other rule is preserved, and any broader inbound TCP rule
# that also exposes the origin port makes the operation fail closed.
#
# The origin-facing allowlist proves only that a connection came from some
# bunny edge server; it never identifies which pull zone.  The sidecar's
# per-front origin token is what authenticates the front.  This is one coarse
# layer, so it fails closed: any fetch, parse, or sanity failure leaves the
# firewall exactly as it was.
set -euo pipefail
{ set +x; } 2>/dev/null

MODE="${1:-}"
FIREWALL="${2:-}"
ORIGIN_PORT="${OPENRUNG_WSS_ORIGIN_PORT:-8443}"
EDGE_LIST_URL="${OPENRUNG_BUNNY_EDGE_LIST_URL:-https://api.bunny.net/system/edgeserverlist}"
# Hetzner allows up to 500 effective rules per firewall.  Staying under a lower
# ceiling leaves room for bunny to grow its fleet between runs; crossing it
# must fail rather than silently truncate the allowlist.
MAX_EFFECTIVE_RULES="${OPENRUNG_MAX_EFFECTIVE_RULES:-450}"
# A unique accepted IPv4 count outside this range means the feed changed shape
# or was truncated.
MIN_EDGE_ADDRESSES="${OPENRUNG_MIN_EDGE_ADDRESSES:-100}"
MAX_EDGE_ADDRESSES="${OPENRUNG_MAX_EDGE_ADDRESSES:-5000}"
# Empty means one range per edge address, which is the tightest allowlist and
# the default.  bunny's list already collapses to close to the provider
# ceiling, so if it outgrows that, set this to a prefix length (24 is the
# natural one) to supernet the addresses instead of failing closed forever.
# That widens the port to neighbours inside each block, which the per-front
# origin token still has to authenticate past.
AGGREGATE_PREFIX="${OPENRUNG_BUNNY_AGGREGATE_PREFIX:-}"
# Hetzner caps source ranges at 100 per individual rule, separately from the
# per-firewall total, so the allowlist is split across several rules on the
# same port.  Every chunk carries identical semantics; only the count differs.
SOURCES_PER_RULE="${OPENRUNG_SOURCES_PER_RULE:-100}"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$MODE" == check || "$MODE" == apply ]] || die "usage: $0 check|apply FIREWALL_NAME"
[[ "$FIREWALL" =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]] || die "firewall name is invalid"
[[ "$ORIGIN_PORT" =~ ^[1-9][0-9]{0,4}$ ]] || die "origin port is invalid"
((10#$ORIGIN_PORT >= 1 && 10#$ORIGIN_PORT <= 65535)) || die "origin port is invalid"
command -v hcloud >/dev/null || die "hcloud CLI is required"
command -v jq >/dev/null || die "jq is required"
command -v python3 >/dev/null || die "python3 is required"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
umask 077

# Certificate verification is the only thing standing between this feed and an
# attacker-chosen allowlist, so never relax it.  --disable has to be the first
# argument or curl may read an insecure option from a user or system curlrc.
curl --disable --fail --silent --show-error --proto '=https' --tlsv1.2 \
  --max-time 30 --retry 2 --retry-delay 2 \
  -o "${TMP_DIR}/edges.json" "$EDGE_LIST_URL" \
  || die "could not fetch the bunny edge list; firewall left unchanged"

hcloud firewall describe "$FIREWALL" -o json >"${TMP_DIR}/firewall.json" \
  || die "could not read firewall ${FIREWALL}"

python3 - "${TMP_DIR}/edges.json" "${TMP_DIR}/firewall.json" "${TMP_DIR}/rules.json" \
  "$ORIGIN_PORT" "$MIN_EDGE_ADDRESSES" "$MAX_EDGE_ADDRESSES" "$MAX_EFFECTIVE_RULES" "$AGGREGATE_PREFIX" \
  "$SOURCES_PER_RULE" <<'PY'
import ipaddress, json, re, sys

edges_path, firewall_path, out_path, port, minimum, maximum, max_rules, aggregate, per_rule = sys.argv[1:]
try:
    port_number = int(port)
    minimum, maximum, max_rules, per_rule = int(minimum), int(maximum), int(max_rules), int(per_rule)
except ValueError as exc:
    raise SystemExit(f"numeric firewall setting is invalid: {exc}") from None
if not 1 <= port_number <= 65535:
    raise SystemExit("origin port must be between 1 and 65535")
if not 1 <= minimum <= maximum:
    raise SystemExit("edge address bounds are invalid")
if max_rules < 1:
    raise SystemExit("effective rule ceiling must be positive")
if not 1 <= per_rule <= 100:
    raise SystemExit("sources per rule must be between 1 and 100")
if aggregate:
    try:
        aggregate = int(aggregate)
    except ValueError:
        raise SystemExit("aggregate prefix must be an integer") from None
    if not 16 <= aggregate <= 32:
        raise SystemExit("aggregate prefix must be between 16 and 32")

def load_json(path, label):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"{label} is not valid JSON: {exc}") from None

edges = load_json(edges_path, "edge list")
if not isinstance(edges, list) or not all(isinstance(entry, str) for entry in edges):
    raise SystemExit("edge list is not an array of address strings")

ipv4_addresses = set()
for entry in edges:
    try:
        address = ipaddress.ip_address(entry.strip())
    except ValueError:
        raise SystemExit(f"edge list contains an invalid IP address: {entry!r}") from None
    # An IPv4 A record is what the origin hostname publishes, so the edge
    # reaches the relay over IPv4 and an IPv6 entry would only widen the rule.
    if address.version == 4:
        ipv4_addresses.add(address)

# Apply plausibility bounds to the unique addresses that can actually enter
# the firewall.  Raw feed length can be inflated with duplicates or ignored
# IPv6 entries and must not make a dangerously small IPv4 allowlist look sane.
ipv4_addresses = sorted(ipv4_addresses, key=int)
if not minimum <= len(ipv4_addresses) <= maximum:
    raise SystemExit(
        f"edge list has an implausible unique IPv4 size: {len(ipv4_addresses)} "
        f"from {len(edges)} feed entries"
    )

addresses = [
    ipaddress.ip_network(f"{address}/{aggregate}", strict=False) if aggregate
    else ipaddress.ip_network(address)
    for address in ipv4_addresses
]

networks = sorted(ipaddress.collapse_addresses(addresses), key=lambda net: (int(net.network_address), net.prefixlen))
source_ips = [str(net) for net in networks]

firewall = load_json(firewall_path, "firewall description")
if not isinstance(firewall, dict) or not isinstance(firewall.get("rules"), list):
    raise SystemExit("firewall description does not contain a rules array")
existing = firewall["rules"]
if not all(isinstance(rule, dict) for rule in existing):
    raise SystemExit("firewall rules must be JSON objects")

port_pattern = re.compile(r"([1-9][0-9]{0,4})(?:-([1-9][0-9]{0,4}))?")

def parse_port(raw, rule_number):
    """Return (first, last, is_single); None means all ports."""
    if raw is None or raw == "":
        return None
    if not isinstance(raw, str):
        raise SystemExit(f"firewall rule {rule_number} has a non-string port")
    match = port_pattern.fullmatch(raw)
    if not match:
        raise SystemExit(f"firewall rule {rule_number} has an invalid port or range: {raw!r}")
    first = int(match.group(1))
    last = int(match.group(2) or match.group(1))
    if not 1 <= first <= 65535 or not 1 <= last <= 65535 or first > last:
        raise SystemExit(f"firewall rule {rule_number} has an invalid port or range: {raw!r}")
    return first, last, match.group(2) is None

def is_inbound_tcp(rule):
    direction = rule.get("direction")
    protocol = rule.get("protocol")
    return (isinstance(direction, str) and direction.lower() == "in" and
            isinstance(protocol, str) and protocol.lower() == "tcp")

kept = []
previous = []
for number, rule in enumerate(existing, start=1):
    if not is_inbound_tcp(rule):
        kept.append(rule)
        continue

    parsed = parse_port(rule.get("port"), number)
    if parsed is None:
        raise SystemExit(
            f"inbound TCP all-port rule {number} overlaps managed origin port {port}; "
            "split or remove it before running this tool"
        )
    first, last, is_single = parsed
    if first <= port_number <= last:
        if is_single and first == port_number:
            previous.append(rule)
            continue
        raise SystemExit(
            f"inbound TCP rule {number} range {rule.get('port')!r} overlaps managed origin port {port}; "
            "split or remove it before running this tool"
        )
    kept.append(rule)

def address_ranges(rule, rule_number):
    direction = rule.get("direction")
    key = "source_ips" if isinstance(direction, str) and direction.lower() == "in" else "destination_ips"
    values = rule.get(key) or []
    if not isinstance(values, list) or not all(isinstance(value, str) for value in values):
        raise SystemExit(f"firewall rule {rule_number} has an invalid {key} list")
    return values

previous_sources = sorted({
    source
    for number, rule in enumerate(previous, start=1)
    for source in address_ranges(rule, number)
})

# Hetzner counts one effective rule per source or destination range, so the
# expanded range count has to fit, not merely the number of rule objects.
effective = sum(len(address_ranges(rule, number)) for number, rule in enumerate(kept, start=1)) + len(source_ips)
if effective > max_rules:
    raise SystemExit(
        f"allowlist needs {effective} effective rules, above the {max_rules} ceiling; "
        "raise OPENRUNG_MAX_EFFECTIVE_RULES only after confirming the provider limit"
    )

chunks = [source_ips[index:index + per_rule] for index in range(0, len(source_ips), per_rule)]
rules = [{
    "direction": "in",
    "protocol": "tcp",
    "port": port,
    "source_ips": chunk,
    "description": f"bunny.net edge origin-facing only ({number}/{len(chunks)})",
} for number, chunk in enumerate(chunks, start=1)]

json.dump({
    "rules": kept + rules,
    "summary": {
        "edge_addresses": len(ipv4_addresses),
        "ipv4_ranges": len(source_ips),
        "rule_chunks": len(chunks),
        "effective_rules": effective,
        "previous_ranges": len(previous_sources),
        "added": sorted(set(source_ips) - set(previous_sources)),
        "removed": sorted(set(previous_sources) - set(source_ips)),
    },
}, open(out_path, "w", encoding="utf-8"))
PY

jq -r '.summary | "edge_addresses=\(.edge_addresses) ipv4_ranges=\(.ipv4_ranges) chunks=\(.rule_chunks) effective_rules=\(.effective_rules) previous_ranges=\(.previous_ranges) added=\(.added|length) removed=\(.removed|length)"' \
  "${TMP_DIR}/rules.json"

if [[ "$MODE" == check ]]; then
  jq '.summary | {added, removed}' "${TMP_DIR}/rules.json"
  exit 0
fi

# replace-rules takes a bare array of rules, not the object the describe/API
# read returns.
jq '.rules' "${TMP_DIR}/rules.json" >"${TMP_DIR}/apply.json"
hcloud firewall replace-rules "$FIREWALL" --rules-file "${TMP_DIR}/apply.json"

hcloud firewall describe "$FIREWALL" -o json >"${TMP_DIR}/after.json"
# Verify every intended rule, not just the union of origin-port sources.  The
# API may reorder rules and address arrays, and may materialize omitted optional
# fields, so compare a normalized semantic representation of the full array.
python3 - "${TMP_DIR}/rules.json" "${TMP_DIR}/after.json" <<'PY' \
  || die "firewall did not converge to the complete intended ruleset"
import json, sys

want_path, actual_path = sys.argv[1:]

def load_rules(path, label, wrapped):
    try:
        with open(path, encoding="utf-8") as handle:
            document = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"{label} is not valid JSON: {exc}") from None
    rules = document.get("rules") if wrapped and isinstance(document, dict) else None
    if not isinstance(rules, list) or not all(isinstance(rule, dict) for rule in rules):
        raise SystemExit(f"{label} does not contain a rules array")
    return rules

def normalize(rule):
    result = dict(rule)
    result["port"] = result.get("port") or None
    result["description"] = result.get("description") or None
    for key in ("source_ips", "destination_ips"):
        values = result.get(key) or []
        if not isinstance(values, list) or not all(isinstance(value, str) for value in values):
            raise SystemExit(f"rule contains an invalid {key} list")
        result[key] = sorted(values)
    return result

def normalized_rules(rules):
    encoded = [json.dumps(normalize(rule), sort_keys=True, separators=(",", ":")) for rule in rules]
    return sorted(encoded)

want = load_rules(want_path, "intended rules", True)
actual = load_rules(actual_path, "applied firewall", True)
if normalized_rules(want) != normalized_rules(actual):
    raise SystemExit("applied firewall rules differ from the intended rules")
PY
printf 'firewall=%s port=%s converged\n' "$FIREWALL" "$ORIGIN_PORT"
