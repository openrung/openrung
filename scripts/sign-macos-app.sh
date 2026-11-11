#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Sign a packaged macOS .app with a Developer ID identity, notarize it and
# staple the ticket to it, so Gatekeeper opens a downloaded copy without
# sending the user to System Settings to allow it. The desktop release
# workflow runs this after desktop/scripts/package-macos.sh and before it
# zips the bundle; the packaging script on its own keeps producing the ad-hoc
# signed bundle, which is all a local or fork build needs.
#
# Usage:
#   scripts/sign-macos-app.sh path/to/Some.app
#
# Environment:
#   MACOS_SIGNING_P12           base64 of the Developer ID certificate and key
#                               as a .p12. It must be a legacy-format export
#                               (openssl pkcs12 -legacy): `security import`
#                               cannot read the OpenSSL 3 default encryption.
#   MACOS_SIGNING_P12_PASSWORD  password of that .p12
#   MACOS_SIGNING_IDENTITY      "Developer ID Application: Name (TEAMID)"
#   NOTARY_KEY_P8               App Store Connect API key (the .p8 text)
#   NOTARY_KEY_ID               its key ID
#   NOTARY_ISSUER_ID            its issuer ID
#   MACOS_SIGNING_REQUIRED      "true" makes a missing certificate an error
#
# Without MACOS_SIGNING_P12 the bundle is left exactly as it was and the
# script succeeds, unless MACOS_SIGNING_REQUIRED is "true". Once the
# certificate is present every later step must succeed.
set -euo pipefail

APP="${1:-}"
if [[ -z "${APP}" || ! -d "${APP}" ]]; then
  echo "usage: $0 path/to/Some.app" >&2
  exit 2
fi

if [[ -z "${MACOS_SIGNING_P12:-}" ]]; then
  if [[ "${MACOS_SIGNING_REQUIRED:-}" == "true" ]]; then
    echo "error: MACOS_SIGNING_P12 is not set, and this build must be signed and notarized" >&2
    exit 1
  fi
  echo "==> no signing certificate configured; leaving ${APP} ad-hoc signed"
  exit 0
fi

for name in MACOS_SIGNING_P12_PASSWORD MACOS_SIGNING_IDENTITY NOTARY_KEY_P8 NOTARY_KEY_ID NOTARY_ISSUER_ID; do
  if [[ -z "${!name:-}" ]]; then
    echo "error: ${name} is not set" >&2
    exit 1
  fi
done

# A secret uploaded from a file (gh secret set < file) keeps the file's
# trailing newline, and the p12 MAC check then fails on it as a wrong
# password; strip line endings rather than trusting how each was uploaded.
p12_password="$(printf '%s' "${MACOS_SIGNING_P12_PASSWORD}" | tr -d '\r\n')"
notary_key_id="$(printf '%s' "${NOTARY_KEY_ID}" | tr -d '\r\n')"
notary_issuer_id="$(printf '%s' "${NOTARY_ISSUER_ID}" | tr -d '\r\n')"

work="$(mktemp -d)"
keychain="${work}/signing.keychain-db"
keychain_password="$(uuidgen)"

# The user's keychain search list, restored on exit: the throwaway keychain is
# added in front of it rather than replacing it.
search_list=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line#\"}"
  search_list+=("${line%\"}")
done < <(security list-keychains -d user)

cleanup() {
  security list-keychains -d user -s ${search_list[@]+"${search_list[@]}"} > /dev/null 2>&1 || true
  security delete-keychain "${keychain}" > /dev/null 2>&1 || true
  rm -rf "${work}"
}
trap cleanup EXIT

# Throwaway keychain: the key never enters the login keychain, and nothing of
# it outlives this script.
security create-keychain -p "${keychain_password}" "${keychain}"
security set-keychain-settings -lut 21600 "${keychain}"
security unlock-keychain -p "${keychain_password}" "${keychain}"
printf '%s' "${MACOS_SIGNING_P12}" | base64 -d > "${work}/signing.p12"
security import "${work}/signing.p12" -k "${keychain}" -f pkcs12 -P "${p12_password}" -T /usr/bin/codesign
rm "${work}/signing.p12"
security set-key-partition-list -S "apple-tool:,apple:" -s -k "${keychain_password}" "${keychain}" > /dev/null
security list-keychains -d user -s "${keychain}" ${search_list[@]+"${search_list[@]}"}

sign() {
  # Hardened runtime and a secure timestamp are both notarization
  # requirements. No entitlements: see docs/desktop-client.md.
  codesign --force --sign "${MACOS_SIGNING_IDENTITY}" --options runtime --timestamp "$1"
}

# Inside-out, not --deep: every Mach-O file other than the bundle's own
# executable is signed by itself first, then the bundle, whose signature
# seals them and covers its main executable. The desktop app has no nested
# code today, so the loop signs nothing there. A bundle that nested a
# framework or a second bundle would need those signed here too, innermost
# first, and the verification below fails until they are.
main_executable="${APP}/Contents/MacOS/$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "${APP}/Contents/Info.plist")"
while IFS= read -r -d '' file; do
  [[ "${file}" == "${main_executable}" ]] && continue
  if file -b "${file}" | grep -q 'Mach-O'; then
    echo "==> signing nested code ${file}"
    sign "${file}"
  fi
done < <(find "${APP}/Contents" -type f -print0)
echo "==> signing ${APP}"
sign "${APP}"
codesign --verify --deep --strict --verbose=2 "${APP}"

echo "==> notarizing"
printf '%s' "${NOTARY_KEY_P8}" > "${work}/notary.p8"
notary() {
  xcrun notarytool "$@" --key "${work}/notary.p8" --key-id "${notary_key_id}" --issuer "${notary_issuer_id}"
}
ditto -c -k --keepParent "${APP}" "${work}/notarize.zip"
# notarytool's exit code does not reliably reflect a rejected submission, so
# it is ignored: gate on the reported status, and print the service's log
# when it is not accepted.
notary submit "${work}/notarize.zip" --wait --output-format json | tee "${work}/notary.json" || true
echo
if ! python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("status")=="Accepted" else 1)' "${work}/notary.json"; then
  submission_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("id",""))' "${work}/notary.json" || true)"
  [[ -n "${submission_id}" ]] && notary log "${submission_id}" || true
  echo "error: notarization was not accepted" >&2
  exit 1
fi

# Staple the ticket into the bundle so Gatekeeper accepts it offline, too.
# The ticket can lag "Accepted" by a few minutes; retry before failing.
stapled=0
for attempt in $(seq 1 10); do
  if xcrun stapler staple "${APP}"; then
    stapled=1
    break
  fi
  echo "no notarization ticket to staple yet (attempt ${attempt}/10); retrying in 30s"
  sleep 30
done
[[ "${stapled}" == 1 ]] || { echo "error: could not staple the notarization ticket to ${APP}" >&2; exit 1; }

# End-to-end gate: the ticket is attached, the sealed bundle still verifies,
# and Gatekeeper itself accepts it as a notarized Developer ID app.
xcrun stapler validate "${APP}"
codesign --verify --deep --strict --verbose=2 "${APP}"
assessment="$(spctl -a -t execute -vv "${APP}" 2>&1)" || { echo "${assessment}" >&2; echo "error: Gatekeeper rejected ${APP}" >&2; exit 1; }
echo "${assessment}"
grep -q 'source=Notarized Developer ID' <<< "${assessment}" || { echo "error: ${APP} is not assessed as a notarized Developer ID app" >&2; exit 1; }

echo "==> signed, notarized and stapled ${APP}"
