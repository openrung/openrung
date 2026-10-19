#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Offline tests for refresh-source-asn.sh: sources are file:// fixtures built
# from documentation prefixes and private-use ASNs.
#
#   bash deploy/broker/refresh-source-asn_test.sh
set -euo pipefail

SCRIPT="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/refresh-source-asn.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

printf '192.0.2.0\t192.0.2.255\t64500\tZZ\tEXAMPLE-DC\n2001:db8::\t2001:db8::ffff\t64501\tZZ\tEXAMPLE\n' | gzip >"$TMP/table.gz"
printf 'ASN,Entity\n64500,"Example DC"\nAS64502 # example\n' >"$TMP/list.csv"

run() {
  OPENRUNG_SOURCE_ASN_URL="file://$TMP/${TABLE:-table.gz}" \
    OPENRUNG_DATACENTER_ASN_URL="file://$TMP/${LIST:-list.csv}" \
    OPENRUNG_SOURCE_ASN_MIN_ROWS=2 OPENRUNG_DATACENTER_ASN_MIN="${MIN_ASNS:-2}" \
    bash "$SCRIPT" "$TMP/out"
}

# Happy path: both files written, decompressed, readable, local additions
# appended after the published list.
mkdir -p "$TMP/out"
printf 'AS64503 # operator addition\n' >"$TMP/out/datacenter-asns.local"
MIN_ASNS=3 run >/dev/null || fail "refresh with valid sources"
[ "$(wc -l <"$TMP/out/ip2asn.tsv" | tr -d ' ')" = 2 ] || fail "table rows"
grep -q '^64500,' "$TMP/out/datacenter-asns.txt" || fail "published list missing"
grep -q '^AS64503' "$TMP/out/datacenter-asns.txt" || fail "local additions missing"
[ -f "$TMP/out/datacenter-asns.local" ] || fail "local additions file removed"
case "$(ls -l "$TMP/out/ip2asn.tsv")" in -rw-r--r--*) ;; *) fail "table not world-readable" ;; esac
[ -z "$(find "$TMP/out" -name '.refresh.*')" ] || fail "work directory left behind"
cp "$TMP/out/ip2asn.tsv" "$TMP/good.tsv"
cp "$TMP/out/datacenter-asns.txt" "$TMP/good.txt"

# Every failure leaves the previous files untouched.
keeps_previous() {
  cmp -s "$TMP/out/ip2asn.tsv" "$TMP/good.tsv" || fail "$1: table replaced"
  cmp -s "$TMP/out/datacenter-asns.txt" "$TMP/good.txt" || fail "$1: list replaced"
}
printf '192.0.2.0\t192.0.2.255\n' | gzip >"$TMP/truncated.gz"
TABLE=truncated.gz run 2>/dev/null && fail "accepted a table line without an ASN"
keeps_previous "truncated table"
printf 'not gzip' >"$TMP/plain.gz"
TABLE=plain.gz run 2>/dev/null && fail "accepted a non-gzip table"
keeps_previous "non-gzip table"
TABLE=missing.gz run 2>/dev/null && fail "accepted a failed download"
keeps_previous "missing table"
printf 'ASN,Entity\n' >"$TMP/empty.csv"
rm "$TMP/out/datacenter-asns.local"
LIST=empty.csv run 2>/dev/null && fail "accepted an ASN list below the floor"
keeps_previous "empty list"

echo "refresh-source-asn_test: ok"
