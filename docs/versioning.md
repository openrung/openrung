# Component versioning

OpenRung versions deployable artifacts independently. A matching number never
implies that two components must be deployed together.

| Component | Version source | Release tag | Published identity |
| --- | --- | --- | --- |
| Standalone relay | `cmd/relay/VERSION` | `relay-vX.Y.Z` | `relay/X.Y.Z` |
| Relay hub | `cmd/relayhub/VERSION` | `relayhub-vX.Y.Z` | `relayhub/X.Y.Z` |
| Broker | `cmd/broker/VERSION` | `broker-vX.Y.Z` | `broker/X.Y.Z` |
| Volunteer desktop | `desktop-volunteer/VERSION` | `volunteer-vX.Y.Z` | `desktop-volunteer/X.Y.Z` |
| Desktop client | `desktop/wails.json` → `info.productVersion` | `vX.Y.Z` | `X.Y.Z` in the About UI, native metadata, and broker telemetry |

Shared Go modules are versioned separately from deployable applications:

| Module | Version source | Module tag | Consumers |
| --- | --- | --- | --- |
| `github.com/openrung/openrung/punchcore` | `punchcore/VERSION` | `punchcore/vX.Y.Z` | Relay/hub and desktop code in this repository; pinned mobile punch bindings |
| `github.com/openrung/openrung/wsscore` | `wsscore/VERSION` | `wsscore/vX.Y.Z` | Desktop client and relay sidecar in this repository; separately released mobile clients when they adopt WSS |

These nested-module versions identify reusable code, not a running service or
an application release. The root and desktop modules use local replacements so
the server and desktop builds in one commit consume the same source. An
external mobile repository instead pins an immutable module tag and updates it
through its own reviewed dependency change. A new `wsscore` tag therefore does
not make Android or iOS WSS-capable and does not publish either mobile app.

The module workflows follow the same release rule. Except for a module
`README.md`-only edit, a pull request that changes files in a module must also
advance that module's strict `X.Y.Z` `VERSION`. CI rejects a version whose
nested tag already exists. After merge, the matching tag workflow creates
`punchcore/vX.Y.Z` or `wsscore/vX.Y.Z` on the merge commit. Consumers pin that
tag rather than copying or hand-mirroring the implementation.

The desktop client uses Wails' `info.productVersion` as its single version
source. The frontend build and Go linker flags consume that value so the About
screen, native package metadata, HTTP headers, and broker telemetry all report
the same identity. The desktop release workflow validates it as strict `X.Y.Z`
before starting the platform build matrix. For a tag build, the tag must be
exactly `vX.Y.Z` for that configured version.

The desktop client keeps the unprefixed `vX.Y.Z` tag namespace it originally
released under, so its workflow matches `v[0-9]*` rather than `v*` — a bare
`v*` also matches `volunteer-v0.1.0` and would publish a desktop release onto
the volunteer app's tag. A new component tag must therefore not begin with `v`
followed by a digit.

Server image workflows reject release tags that do not exactly match their
component's `VERSION` file. Release builds publish the `X.Y.Z` image tag.
Builds from `main` publish `main` plus `sha-*` and embed a development identity
such as `0.1.0-dev+sha.c4b2c65`. Published images also carry the full Git
revision and component version in OCI labels.

`sha-*` is a dev-build identity, published only for non-release builds. A
release build of a commit `main` already built is not the same artifact — it
injects a different `-X main.version`, so it produces different bits — and
re-pushing `sha-<commit>` would repoint a tag the fleet pins to. Nothing in
GHCR enforces tag immutability, so pin a digest wherever an image must be
guaranteed not to move.

To release a server component:

1. Choose the next semantic version and update only that component's `VERSION`.
2. Merge and verify the ordinary `main` image.
3. Tag the intended commit with the matching component tag, such as
   `relay-v0.1.0`.
4. Deploy the exact semantic image tag or digest. Keep `main` for development
   and explicitly managed rolling deployments.

To release the desktop client:

1. Set `desktop/wails.json` → `info.productVersion` to the next strict semantic
   version, such as `0.1.3`, and merge the change after CI passes.
2. Tag that exact commit with `vX.Y.Z`, such as `v0.1.3`.
3. The desktop release workflow validates that the tag is exactly `v` plus the
   configured product version before building. A mismatch stops the release
   before any platform jobs start.

Application versions identify builds for operations and rollback. They do not
replace compatibility contracts:

- the broker HTTP contract remains under `/api/v1`;
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
