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
# Each line below is one the broker's loader would reject (or, for the last
# group, one this script refuses although the broker is laxer); appended to
# otherwise valid rows, it must fail the refresh and keep the previous files.
VALID_ROWS=$'192.0.2.0\t192.0.2.127\t64500\tZZ\tA\n2001:db8::\t2001:db8::ff\t64501\tZZ\tB\n'
while IFS='|' read -r name line; do
  printf '%s%b\n' "$VALID_ROWS" "$line" | gzip >"$TMP/bad.gz"
  TABLE=bad.gz run 2>"$TMP/err" && fail "accepted a table with $name"
  grep -q 'IP-to-ASN table line 3' "$TMP/err" || fail "$name: not refused by the line check: $(cat "$TMP/err")"
  keeps_previous "$name"
done <<'EOF'
no asn|192.0.2.0\t192.0.2.255
non-numeric asn|192.0.2.0\t192.0.2.255\tAS64500\tZZ\tX
asn above 32 bits|192.0.2.0\t192.0.2.255\t4294967296\tZZ\tX
negative asn|192.0.2.0\t192.0.2.255\t-1\tZZ\tX
octet above 255|192.0.2.0\t192.0.2.256\t64500\tZZ\tX
leading zero octet|192.0.2.0\t192.0.2.099\t64500\tZZ\tX
three octets|192.0.2\t192.0.2.255\t64500\tZZ\tX
not an address|example\t192.0.2.255\t64500\tZZ\tX
reversed IPv4 range|192.0.2.255\t192.0.2.0\t64500\tZZ\tX
reversed IPv6 range|2001:db8::ff\t2001:db8::1\t64500\tZZ\tX
mixed families|192.0.2.0\t2001:db8::ff\t64500\tZZ\tX
two ellipses|2001:db8::1::2\t2001:db8::ff\t64500\tZZ\tX
five-digit group|2001:db8::12345\t2001:db8::fffff\t64500\tZZ\tX
nine groups|1:2:3:4:5:6:7:8:9\t1:2:3:4:5:6:7:8:a\t64500\tZZ\tX
leading space|  192.0.2.0\t192.0.2.255\t64500\tZZ\tX
zoned IPv6|fe80::1%eth0\tfe80::2%eth0\t64500\tZZ\tX
IPv4-mapped IPv6|::ffff:192.0.2.0\t::ffff:192.0.2.255\t64500\tZZ\tX
EOF
printf '%s192.0.2.0\t192.0.2.255\t64500\tZZ\t%s\n' "$VALID_ROWS" "$(head -c 70000 /dev/zero | tr '\0' x)" | gzip >"$TMP/long.gz"
TABLE=long.gz run 2>"$TMP/err" && fail "accepted a line beyond the broker's line limit"
grep -q 'line 3: line too long' "$TMP/err" || fail "overlong line: $(cat "$TMP/err")"
keeps_previous "overlong line"
# Valid edge forms still load: the full IPv4 space, "::" and a compressed
# IPv6 range, uppercase hex, 32-bit ASN boundary values.
printf '0.0.0.0\t255.255.255.255\t4294967295\tZZ\tX\n::\t::ff\t1\tZZ\tX\n2001:DB8::\t2001:db8:0:0:0:0:0:FFFF\t64500\tZZ\tX\n' | gzip >"$TMP/edges.gz"
TABLE=edges.gz run >/dev/null || fail "rejected valid edge forms"
cp "$TMP/out/ip2asn.tsv" "$TMP/good.tsv"
cp "$TMP/out/datacenter-asns.txt" "$TMP/good.txt"
printf 'not gzip' >"$TMP/plain.gz"
TABLE=plain.gz run 2>/dev/null && fail "accepted a non-gzip table"
keeps_previous "non-gzip table"
TABLE=missing.gz run 2>/dev/null && fail "accepted a failed download"
keeps_previous "missing table"
printf 'ASN,Entity\n' >"$TMP/empty.csv"
rm "$TMP/out/datacenter-asns.local"
LIST=empty.csv run 2>/dev/null && fail "accepted an ASN list below the floor"
keeps_previous "empty list"
# Entries the broker skips (zero, above 32 bits, a space after "AS") and
# repeats of one ASN in another spelling do not count toward the floor.
printf '0\n4294967296\nAS 64500\n64500\nas064500\n' >"$TMP/skipped.csv"
LIST=skipped.csv run 2>/dev/null && fail "counted ASNs the broker would skip"
keeps_previous "skipped list entries"

echo "refresh-source-asn_test: ok"
