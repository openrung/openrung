# Component versioning

OpenRung versions deployable artifacts independently. A matching number never
implies that two components must be deployed together.

| Component | Version source | Release tag | Published identity |
| --- | --- | --- | --- |
| Standalone relay | `cmd/relay/VERSION` | `relay-vX.Y.Z` | `relay/X.Y.Z` |
| Relay hub | `cmd/relayhub/VERSION` | `relayhub-vX.Y.Z` | `relayhub/X.Y.Z` |
| Broker | `cmd/broker/VERSION` | `broker-vX.Y.Z` | `broker/X.Y.Z` |
| Volunteer desktop | `desktop-volunteer/VERSION` | `volunteer-vX.Y.Z` | `desktop-volunteer/X.Y.Z` |

Server image workflows reject release tags that do not exactly match their
component's `VERSION` file. Release builds publish an immutable `X.Y.Z` image
tag plus a `sha-*` tag. Builds from `main` publish `main` plus `sha-*` and embed
a development identity such as `0.1.0-dev+sha.c4b2c65`. Published images also
carry the full Git revision and component version in OCI labels.

To release a server component:

1. Choose the next semantic version and update only that component's `VERSION`.
2. Merge and verify the ordinary `main` image.
3. Tag the intended commit with the matching component tag, such as
   `relay-v0.1.0`.
4. Deploy the exact semantic image tag or digest. Keep `main` for development
   and explicitly managed rolling deployments.

Application versions identify builds for operations and rollback. They do not
replace compatibility contracts:

- the broker HTTP contract remains under `/api/v1`;
- the reverse tunnel uses `tunnel.ProtocolVersion` plus additive capability
  flags;
- NAT punching uses `punchcore.ProtoVersion` and its ALPN;
- `relay_version` identifies the relay runtime and is not the relay hub,
  broker, or client version.

Breaking wire changes require a protocol/API migration even when application
versions also receive a major bump. Conversely, compatible application
releases do not require protocol-version changes.
