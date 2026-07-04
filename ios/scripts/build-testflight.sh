#!/usr/bin/env bash
#
# Build, export, and upload a TestFlight build of the OpenRung iOS app.
#
# Usage:
#   ios/scripts/build-testflight.sh [BUILD_NUMBER]
#
# BUILD_NUMBER must be unique and increasing for every upload (defaults to a UTC timestamp).
#   SKIP_UPLOAD=1                build + export only, don't upload
#   ASC_API_KEY_ID / ASC_API_ISSUER_ID   override the App Store Connect API key identifiers
#
# The marketing version (e.g. 0.1.1) comes from MARKETING_VERSION in ios/project.yml.
# The App Store Connect key AuthKey_<ASC_API_KEY_ID>.p8 must live in ~/.appstoreconnect/private_keys/.
# The first signed run registers the App IDs / App Group / Network Extension capability via
# -allowProvisioningUpdates, and needs the team's Apple ID signed into Xcode (Settings > Accounts).
set -euo pipefail

cd "$(dirname "$0")/.."          # -> ios/

TEAM_ID="9VLV9A7KS9"
SCHEME="OpenRungClient"
BUILD_NUMBER="${1:-$(date -u +%Y%m%d%H%M)}"
ARCHIVE_PATH="build/OpenRung.xcarchive"
EXPORT_DIR="build/export"
ASC_API_KEY_ID="${ASC_API_KEY_ID:-48595Z8V62}"
ASC_API_ISSUER_ID="${ASC_API_ISSUER_ID:-46f70df5-a159-48b5-8780-b4b59d922a17}"

echo "==> Regenerating Xcode project"
xcodegen generate

echo "==> Archiving OpenRung (build ${BUILD_NUMBER})"
xcodebuild \
  -project OpenRung.xcodeproj \
  -scheme "${SCHEME}" \
  -configuration Release \
  -destination 'generic/platform=iOS' \
  -archivePath "${ARCHIVE_PATH}" \
  CURRENT_PROJECT_VERSION="${BUILD_NUMBER}" \
  DEVELOPMENT_TEAM="${TEAM_ID}" \
  -allowProvisioningUpdates \
  archive

echo "==> Exporting .ipa"
rm -rf "${EXPORT_DIR}"
xcodebuild -exportArchive \
  -archivePath "${ARCHIVE_PATH}" \
  -exportOptionsPlist ExportOptions.plist \
  -exportPath "${EXPORT_DIR}" \
  -allowProvisioningUpdates

IPA="$(ls "${EXPORT_DIR}"/*.ipa | head -1)"
echo "==> Built ${IPA}"

if [ "${SKIP_UPLOAD:-0}" = "1" ]; then
  echo "==> SKIP_UPLOAD=1 set; not uploading."
  exit 0
fi

echo "==> Uploading to App Store Connect / TestFlight"
xcrun altool --upload-app -t ios \
  -f "${IPA}" \
  --apiKey "${ASC_API_KEY_ID}" \
  --apiIssuer "${ASC_API_ISSUER_ID}"

echo ""
echo "==> Uploaded. The build appears in the TestFlight tab after processing (~5-15 min)."
