#!/usr/bin/env bash
#
# Cut an application release: tag the tip of origin/main with the component's
# release tag (<component>-vX.Y.Z from its VERSION file) and push the tag,
# after checking everything the release workflow would reject. Reads VERSION
# from origin/main itself, so it always tags exactly what is merged — never
# local, possibly-unmerged state.
#
# Usage:
#   scripts/release.sh <component>
#
# Components: broker | client | relay | relayhub | desktop | desktop-volunteer
#
# Shared Go modules (brokerapi, punchcore, wsscore) are not released here:
# merging their VERSION bump is the release, and the matching *-tag workflow
# creates the <module>/vX.Y.Z tag on the merge commit.
#
# See docs/versioning.md for the full scheme.
set -euo pipefail

component="${1-}"

case "$component" in
  broker) version_path="cmd/broker/VERSION" ;;
  client) version_path="cmd/client/VERSION" ;;
  relay) version_path="cmd/relay/VERSION" ;;
  relayhub) version_path="cmd/relayhub/VERSION" ;;
  desktop) version_path="desktop/VERSION" ;;
  desktop-volunteer) version_path="desktop-volunteer/VERSION" ;;
  brokerapi | punchcore | wsscore)
    echo "error: $component is a shared Go module, not an application: merging its VERSION bump is the release, and the ${component}-tag workflow creates the ${component}/vX.Y.Z tag." >&2
    exit 1
    ;;
  *)
    echo "usage: scripts/release.sh <component>" >&2
    echo "components: broker client relay relayhub desktop desktop-volunteer" >&2
    exit 1
    ;;
esac

echo "==> fetching origin/main"
git fetch --quiet origin main
sha="$(git rev-parse FETCH_HEAD)"
subject="$(git log -1 --format=%s "$sha")"

version="$(git show "$sha:$version_path" | tr -d '\r\n')"
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "error: invalid $version_path \"$version\" at origin/main (want X.Y.Z)" >&2
  exit 1
fi

# The Wails apps keep an info.productVersion packaging copy that CI requires
# to match VERSION; catch drift here before a tag triggers a failing release.
case "$component" in
  desktop | desktop-volunteer)
    if ! command -v jq >/dev/null 2>&1; then
      echo "error: jq is required to check $component/wails.json" >&2
      exit 1
    fi
    copy="$(git show "$sha:$component/wails.json" | jq -r '.info.productVersion')"
    if [ "$copy" != "$version" ]; then
      echo "error: $component/wails.json info.productVersion ($copy) disagrees with $version_path ($version) at origin/main; VERSION is canonical — merge a fix first" >&2
      exit 1
    fi
    ;;
esac

tag="${component}-v${version}"

if git rev-parse --quiet --verify "refs/tags/$tag" >/dev/null; then
  echo "error: tag $tag already exists locally; bump $version_path in a PR first (a version must never mean different code)" >&2
  exit 1
fi
if git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then
  echo "error: tag $tag already exists on origin; bump $version_path in a PR first (a version must never mean different code)" >&2
  exit 1
fi

echo
echo "component: $component"
echo "version:   $version  (from $version_path at origin/main)"
echo "tag:       $tag"
echo "commit:    ${sha}  ($subject)"
echo
printf 'Tag and push? Pushing the tag IS the release. [y/N] '
read -r reply
case "$reply" in
  y | Y | yes | YES) ;;
  *)
    echo "aborted; nothing was tagged"
    exit 1
    ;;
esac

# Lightweight, not annotated (same as the module *-tag workflows): for an
# annotated tag push, GITHUB_SHA is the tag object's SHA, not the commit's,
# and the release workflows would stamp that non-commit SHA into binaries and
# image labels as the revision.
git tag "$tag" "$sha"
git push origin "refs/tags/$tag"

echo "==> pushed $tag; the release workflow is now building"
echo "    https://github.com/openrung/openrung/actions"
