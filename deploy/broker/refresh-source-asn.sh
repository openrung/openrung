#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Refresh the data behind the broker's datacenter source classification
# (OPENRUNG_SOURCE_ASN_DB / OPENRUNG_DATACENTER_ASN_LIST, see README.md).
# Runs on the broker HOST, daily from cron or a systemd timer:
#
#   refresh-source-asn.sh [DIR]     (default /var/lib/openrung-source)
#
# It writes two files into DIR, each replaced atomically and only after it
# validates, so a failed or truncated download never replaces good data:
#
#   ip2asn.tsv          IP range -> ASN table (iptoasn.com, IPv4 + IPv6)
#   datacenter-asns.txt the published hosting/datacenter ASN list, followed by
#                       the operator's own additions from datacenter-asns.local
#                       in DIR when that file exists (it is never downloaded or
#                       overwritten)
#
# The broker re-reads the files within a minute of a change. Its staleness
# guard (OPENRUNG_SOURCE_ASN_MAX_AGE) dates the data by ip2asn.tsv's
# modification time, so a refresh that keeps failing eventually turns new
# enrollments into retryable 503s rather than classifying with old data.
#
# Sources (overridable for mirrors and tests):
#   OPENRUNG_SOURCE_ASN_URL      default https://iptoasn.com/data/ip2asn-combined.tsv.gz
#                                (gzip, decompressed here)
#   OPENRUNG_DATACENTER_ASN_URL  default the MIT-licensed bad-asn-list by Brian
#                                Hama (github.com/brianhama/bad-asn-list), a
#                                published list of cloud, managed hosting and
#                                colocation ASNs
# Sanity floors: OPENRUNG_SOURCE_ASN_MIN_ROWS (default 100000 table rows) and
# OPENRUNG_DATACENTER_ASN_MIN (default 100 ASNs).
set -euo pipefail

DIR="${1:-/var/lib/openrung-source}"
TABLE_URL="${OPENRUNG_SOURCE_ASN_URL:-https://iptoasn.com/data/ip2asn-combined.tsv.gz}"
LIST_URL="${OPENRUNG_DATACENTER_ASN_URL:-https://raw.githubusercontent.com/brianhama/bad-asn-list/master/bad-asn-list.csv}"
MIN_ROWS="${OPENRUNG_SOURCE_ASN_MIN_ROWS:-100000}"
MIN_ASNS="${OPENRUNG_DATACENTER_ASN_MIN:-100}"

die() { echo "refresh-source-asn: $*" >&2; exit 1; }

mkdir -p "$DIR"
WORK="$(mktemp -d "$DIR/.refresh.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

fetch() {
  curl --fail --silent --show-error --location --max-time 300 --retry 3 -o "$2" "$1" ||
    die "download failed: $1"
}

# Both files are validated before either replaces the current one. The checks
# accept a subset of what the broker's loader (internal/broker/source_class.go)
# accepts, never more, so data this script installs always loads; a refresh
# that fails here keeps the previous files. Where the broker is laxer (zoned,
# IPv4-mapped or IPv4-suffixed IPv6 spellings, which the upstream table does
# not use), this check refuses. LC_ALL=C makes length() count bytes, like the
# broker's 64 KiB line limit.

# IP -> ASN table: the broker rejects the whole file on any malformed line,
# so every line must parse: two addresses of one family in ascending order
# and a 32-bit ASN.
fetch "$TABLE_URL" "$WORK/table.gz"
gzip -dc "$WORK/table.gz" >"$WORK/ip2asn.tsv" || die "IP-to-ASN table is not valid gzip"
LC_ALL=C awk -F'\t' -v min="$MIN_ROWS" '
  # ipv4 returns a fixed-width sortable key for a dotted quad (decimal octets
  # 0-255, no leading zeros), or "".
  function ipv4(s,   o, n, i, v) {
    if (s !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) return ""
    split(s, o, ".")
    v = 0
    for (i = 1; i <= 4; i++) {
      if (length(o[i]) > 3 || (length(o[i]) > 1 && substr(o[i], 1, 1) == "0") || o[i] + 0 > 255) return ""
      v = v * 256 + o[i]
    }
    return sprintf("%010.0f", v)
  }
  # ipv6 returns the address expanded to eight 4-digit lowercase groups (a
  # sortable key), or "" for anything else, including the IPv4-mapped range.
  function ipv6(s,   c, at, head, tail, hn, tn, h, t, g, i, out) {
    if (s !~ /^[0-9A-Fa-f:]+$/ || s ~ /:::/) return ""
    c = gsub(/::/, "&", s)
    if (c > 1) return ""
    hn = tn = 0
    if (c == 1) {
      at = index(s, "::")
      head = substr(s, 1, at - 1); tail = substr(s, at + 2)
      if (head != "") hn = split(head, h, ":")
      if (tail != "") tn = split(tail, t, ":")
      if (hn + tn > 7) return ""
    } else {
      hn = split(s, h, ":")
      if (hn != 8) return ""
    }
    for (i = 1; i <= hn; i++) g[i] = h[i]
    for (i = hn + 1; i <= 8 - tn; i++) g[i] = "0"
    for (i = 1; i <= tn; i++) g[8 - tn + i] = t[i]
    out = ""
    for (i = 1; i <= 8; i++) {
      if (g[i] !~ /^[0-9A-Fa-f]+$/ || length(g[i]) > 4) return ""
      out = out (i > 1 ? ":" : "") substr("0000" tolower(g[i]), length(g[i]) + 1)
    }
    if (out ~ /^0000:0000:0000:0000:0000:ffff:/) return ""
    return out
  }
  function reject(why) { bad = NR ": " why; exit 1 }
  length($0) > 60000 { reject("line too long") }
  $0 ~ /^[[:space:]]*$/ { next }
  {
    if (NF < 3) reject("want range_start, range_end and asn")
    if ($3 !~ /^[0-9]+$/ || $3 + 0 > 4294967295) reject("malformed asn")
    lo = ipv4($1); hi = ipv4($2)
    if (lo == "" && hi == "") { lo = ipv6($1); hi = ipv6($2) }
    if (lo == "" || hi == "") reject("malformed or mixed-family range")
    if (hi < lo) reject("range ends before it starts")
    rows++
  }
  END {
    if (bad != "") { printf "IP-to-ASN table line %s\n", bad > "/dev/stderr"; exit 1 }
    if (rows < 1 || rows < min) { printf "IP-to-ASN table has %d rows, expected at least %d\n", rows, min > "/dev/stderr"; exit 1 }
  }' "$WORK/ip2asn.tsv" || die "IP-to-ASN table failed validation"

# Datacenter ASN list: the published list plus local additions. Count the
# distinct ASNs exactly as the broker reads them: optional leading space and
# "AS", then the leading digits, 1..2^32-1.
fetch "$LIST_URL" "$WORK/published.txt"
{
  echo "# Generated by refresh-source-asn.sh from $LIST_URL"
  cat "$WORK/published.txt"
  echo
  if [ -f "$DIR/datacenter-asns.local" ]; then
    echo "# Local additions ($DIR/datacenter-asns.local)"
    cat "$DIR/datacenter-asns.local"
    echo
  fi
} >"$WORK/datacenter-asns.txt"
asns="$(LC_ALL=C awk '
  length($0) > 60000 { toolong = NR; exit 1 }
  {
    line = $0
    sub(/^[[:space:]]+/, "", line)
    if (toupper(substr(line, 1, 2)) == "AS") line = substr(line, 3)
    if (match(line, /^[0-9]+/)) {
      n = substr(line, 1, RLENGTH) + 0
      if (n > 0 && n <= 4294967295) seen[n] = 1
    }
  }
  END {
    if (toolong) { printf "datacenter ASN list line %d too long\n", toolong > "/dev/stderr"; exit 1 }
    count = 0
    for (n in seen) count++
    print count
  }' "$WORK/datacenter-asns.txt")" ||
  die "datacenter ASN list failed validation"
[ "$asns" -ge 1 ] && [ "$asns" -ge "$MIN_ASNS" ] ||
  die "datacenter ASN list names $asns ASNs, expected at least $MIN_ASNS"

# World-readable: the broker container runs as an unprivileged user.
chmod 0644 "$WORK/ip2asn.tsv" "$WORK/datacenter-asns.txt"
mv -f "$WORK/datacenter-asns.txt" "$DIR/datacenter-asns.txt"
mv -f "$WORK/ip2asn.tsv" "$DIR/ip2asn.tsv"
echo "refresh-source-asn: $(wc -l <"$DIR/ip2asn.tsv" | tr -d ' ') table rows, $asns datacenter ASNs"
