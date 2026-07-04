#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
keystore="${OPENRUNG_RELEASE_STORE_FILE:-$HOME/.openrung/openrung-release.p12}"
key_alias="${OPENRUNG_RELEASE_KEY_ALIAS:-openrung}"
keychain_service="org.openrung.android.release"
libbox_aar="$script_dir/app/libs/libbox.aar"

if [[ ! -f "$libbox_aar" ]]; then
  echo "Release libbox AAR not found. Run ./build-libbox-release.sh first." >&2
  exit 1
fi

if LC_ALL=C unzip -p "$libbox_aar" | strings -a | LC_ALL=C grep -E \
  '(/Users/[^/]+|/home/[^/]+)/(go/pkg/mod|Documents|\.gradle)/' >/dev/null; then
  echo "Release libbox AAR contains a local build path. Rebuild it with ./build-libbox-release.sh." >&2
  exit 1
fi

if [[ ! -f "$keystore" ]]; then
  echo "Release keystore not found: $keystore" >&2
  exit 1
fi

if [[ -z "${OPENRUNG_RELEASE_STORE_PASSWORD:-}" ]]; then
  if ! command -v security >/dev/null 2>&1; then
    echo "Set OPENRUNG_RELEASE_STORE_PASSWORD outside macOS." >&2
    exit 1
  fi
  OPENRUNG_RELEASE_STORE_PASSWORD="$(security find-generic-password -a "$USER" -s "$keychain_service" -w)"
fi

export OPENRUNG_RELEASE_STORE_FILE="$keystore"
export OPENRUNG_RELEASE_STORE_PASSWORD
export OPENRUNG_RELEASE_KEY_ALIAS="$key_alias"
export OPENRUNG_RELEASE_KEY_PASSWORD="${OPENRUNG_RELEASE_KEY_PASSWORD:-$OPENRUNG_RELEASE_STORE_PASSWORD}"
export JAVA_HOME="${JAVA_HOME:-/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home}"
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-/private/tmp/openrung-gradle}"

cd "$script_dir"
./gradlew --no-daemon clean testDebugUnitTest assembleRelease

release_dir="$script_dir/app/build/outputs/apk/release"
built_apk="$release_dir/app-release.apk"
version="$(grep -E '^[[:space:]]*versionName[[:space:]]*=' "$script_dir/app/build.gradle.kts" | sed -E 's/.*"([^"]+)".*/\1/')"

if [[ -n "$version" ]]; then
  named_apk="$release_dir/OpenRung-${version}-release.apk"
  cp -f "$built_apk" "$named_apk"
  echo "Release APK: $named_apk"
else
  echo "Could not parse versionName from build.gradle.kts; keeping default APK name." >&2
  echo "Release APK: $built_apk"
fi
