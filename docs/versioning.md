# Component versioning

OpenRung versions deployable artifacts independently. A matching number never
implies that two components must be deployed together. What IS uniform is the
mechanics: every component owns a `VERSION` file, every release tag follows
one of exactly two grammars, and every component follows one of exactly two
release models.

## Components

| Component | Version source | Release tag | Ships as |
| --- | --- | --- | --- |
| Broker | `cmd/broker/VERSION` | `broker-vX.Y.Z` | container image `X.Y.Z` |
| Client CLI | `cmd/client/VERSION` | `client-vX.Y.Z` | cross-compiled binaries |
| Standalone relay | `cmd/relay/VERSION` | `relay-vX.Y.Z` | container image `X.Y.Z` |
| Relay hub | `cmd/relayhub/VERSION` | `relayhub-vX.Y.Z` | container image `X.Y.Z` |
| WSS sidecar | `cmd/wsssidecar/VERSION` | — (ships inside the relay image) | second binary in the relay image |
| Desktop client | `desktop/VERSION` | `desktop-vX.Y.Z` | GUI installers |
| Volunteer desktop | `desktop-volunteer/VERSION` | `desktop-volunteer-vX.Y.Z` | GUI installers |

Shared Go modules are versioned separately from deployable applications:

| Module | Version source | Module tag | Consumers |
| --- | --- | --- | --- |
| `github.com/openrung/openrung/brokerapi` | `brokerapi/VERSION` | `brokerapi/vX.Y.Z` | Root and desktop clients in this repository; pinned mobile broker bindings |
| `github.com/openrung/openrung/punchcore` | `punchcore/VERSION` | `punchcore/vX.Y.Z` | Relay/hub and desktop code in this repository; pinned mobile punch bindings |
| `github.com/openrung/openrung/wsscore` | `wsscore/VERSION` | `wsscore/vX.Y.Z` | Desktop client and relay sidecar in this repository; pinned Android and iOS WSS bindings |

These nested-module versions identify reusable code, not a running service or
an application release. In-repository Go modules use local replacements so
server and desktop builds in one commit consume the same source. An external
mobile repository instead pins an immutable module tag and updates it through
its own reviewed dependency change. A new `brokerapi` or `wsscore` tag
therefore does not change either mobile app until that repository reviews the
pin, rebuilds its native binding, tests it, and publishes an application
release.

## Two tag grammars

1. **Applications**: `<component>-vX.Y.Z`, where `<component>` is the name in
   the table above (matching its directory basename). Workflow triggers match
   `<component>-v[0-9]*`, requiring a digit after the `v`: with a bare `*`,
   `desktop-v*` would also match `desktop-volunteer-v0.1.1` and publish one
   app's release onto another's tag. For the same reason a new component name
   must never extend an existing one all the way through its `-v` (as in a
   hypothetical `relay-v2` component; `relay-vpn` is fine).
2. **Shared Go modules**: `<module-dir>/vX.Y.Z`. The slash form is what Go
   tooling requires for a nested module, and it is reserved for modules — app
   tags never use it, so an app release can never be mistaken for (or
   accidentally become) a fetchable module version.

Two historical namespaces are retired and frozen: bare `vX.Y.Z` tags
(`v0.1.0`–`v0.1.3`, early desktop client releases) and `volunteer-vX.Y.Z`
(`volunteer-v0.1.0`, the first volunteer desktop release). Existing tags and
the GitHub releases on them stay; nothing new is ever tagged in either
namespace, and no workflow triggers on them anymore.

## Two release models

**Modules: merge = release.** Except for a module `README.md`-only edit, a
pull request that changes files in `brokerapi/`, `punchcore/`, or `wsscore/`
must also advance that module's strict `X.Y.Z` `VERSION`; CI rejects a version
whose nested tag already exists. After merge, the matching `*-tag` workflow
creates `<module>/vX.Y.Z` on the merge commit. Consumers pin that tag rather
than copying the implementation.

**Applications: tag = release.** `VERSION` on `main` names the release being
prepared; it advances in an ordinary reviewed PR (no bump is required just
because the component's code changed). Pushing the matching
`<component>-vX.Y.Z` tag is the release action: the component's workflow
validates that the tag exactly matches `VERSION`, builds, and publishes. A
mismatched tag stops the release before any build starts. Use the helper,
which re-checks everything CI would reject and tags the tip of `origin/main`:

```bash
scripts/release.sh <component>
```

Because tag = release, cutting an application release is a two-step affair:
first merge a PR advancing `VERSION` (skip if it already names an unreleased
version), then run `scripts/release.sh`. A version, once tagged, never means
different code: to re-release, bump again.

## How a binary learns its version

Every server binary and the client CLI resolve their identity through
`internal/buildinfo`:

- Release builds inject `-X openrung/internal/buildinfo.version=X.Y.Z` and
  `-X openrung/internal/buildinfo.revision=<commit>` (see
  `deploy/*/Dockerfile` and `client-release.yml`).
- Without injection, `Version` falls back to the component's own embedded
  `VERSION` file and `Revision` to the VCS metadata the Go toolchain records,
  so a plain `go build` still reports something truthful.
- Every server binary and the client CLI answer `-version` with the same
  shape: `<component>/X.Y.Z revision=<commit>`.

The linker silently ignores `-X` for a symbol it cannot resolve, so
`internal/buildinfo/injection_test.go` links a probe with the release flags
and reads the values back, and rejects any Dockerfile or workflow that injects
a symbol outside the proven set.

The Wails apps do not link `internal/buildinfo`; they use two other
mechanisms, fed from the same `VERSION` files:

- `internal/client.appVersion` is the telemetry identity sent in the
  `X-OpenRung-App-Version` header. The desktop packaging scripts stamp it from
  `desktop/VERSION` (`desktop/scripts/versioned-wails-build.mjs`), and the
  client CLI release stamps it from `cmd/client/VERSION`. The symbol path is
  proven resolvable by `desktop/scripts/version-injection.test.mjs`.
- The two Wails apps keep an `info.productVersion` **copy** of their `VERSION`
  in `wails.json`, only because Wails stamps it into native package metadata
  (Info.plist, the Windows exe resource). `VERSION` is canonical, and drift is
  rejected by the `go-checks` PR gate and both release workflows; the desktop
  client's build script (`versioned-wails-build.mjs`) and the volunteer app's
  Go test suite (`version_test.go`) refuse it too. When bumping a Wails app,
  update both files in the same PR.

The volunteer desktop's frontend and Go runtime read `VERSION` directly
(Vite `define` and `go:embed`); the desktop client's frontend receives it the
same way through `versioned-wails-build.mjs`.

## Server images

Server image workflows reject release tags that do not exactly match their
component's `VERSION` file. Release builds publish the `X.Y.Z` image tag.
Builds from `main` publish `main` plus `sha-*` and embed a development
identity such as `0.1.0-dev+sha.c4b2c65`.

`sha-*` is a dev-build identity, published only for non-release builds. A
release build of a commit `main` already built is not the same artifact — it
injects a different build version, so it produces different bits — and
re-pushing `sha-<commit>` would repoint a tag the fleet pins to. Nothing in
GHCR enforces tag immutability, so pin a digest wherever an image must be
guaranteed not to move.

The WSS sidecar is not released on its own: it ships as the second binary in
the relay image, and a relay release build stamps both binaries with the relay
version. Its own `cmd/wsssidecar/VERSION` only feeds development builds.

## Versions are not compatibility contracts

Application versions identify builds for operations and rollback. They do not
replace compatibility contracts:

- the broker HTTP contract remains under `/api/v1`; `brokerapi` module versions
  identify a client-library release, not a new HTTP protocol version;
- the reverse tunnel uses `tunnel.ProtocolVersion` plus additive capability
  flags;
- NAT punching uses `punchcore.ProtoVersion` and its ALPN;
- Reality-over-WSS uses the protocol constants and interoperability contract
  owned by `wsscore`; its module version does not replace its on-wire protocol
  version;
- `relay_version` identifies the relay runtime and is not the relay hub,
  broker, or client version.

Breaking wire changes require a protocol/API migration even when application
versions also receive a major bump. Conversely, compatible application
releases do not require protocol-version changes.
