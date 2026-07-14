# punchcore

`github.com/openrung/openrung/punchcore` is the shared, dependency-free (stdlib
only) core of OpenRung's NAT hole-punch protocol. It is a nested Go module of
the OpenRung repository and the **single source of truth** for the punch wire
format and mechanics, consumed by:

- the relay hub and relay runtimes (this repository, via `internal/punch`
  and `internal/tunnel`),
- the desktop CLI and GUI clients (this repository),
- the Android app's gomobile binding (`android/punchbridge` in
  `openrung-mobile-app`), which pins a tagged version of this module.

## What belongs here

- **Wire format**: probe/ack framing, reflector request/reply framing, the JSON
  coordination structs (`PunchConfig`, `PunchRequest`, `PunchResponse`,
  `PunchDirective`, `PunchAck`, `PunchResult`), and token
  derivation/encoding (`ComputeToken`, `EncodeToken`, `DecodeToken`).
- **Symmetric punch mechanics**: `Policy.Gather`, `Policy.Attempt`,
  `GenerateNonce`/`NonceKey`, `LocalCandidates`.
- **The UDP `Reflector` server** the hub runs.
- **The hub HTTP client** (`HubClient`) and `HardenedHTTPClient`.
- **The `Policy` presets**: `DesktopPolicy()` (historical
  `internal/punch` behavior; also what the hub and relay runtimes run) and
  `MobilePolicy()` (the hardened Android profile). Zero-value `Policy` is not
  valid; always start from a preset.

## What never belongs here

- QUIC transports, sessions, or bridges — the QUIC stack differs per consumer
  (mainline quic-go on desktop/servers, the sagernet fork on Android), so each
  consumer keeps its own session layer on top of this module.
- Hub secret storage or rotation policy.
- The hub's HTTP coordination *server* (`internal/tunnel`).
- Any UI.

## Wire-format compatibility

`testdata/golden.json` + `golden_test.go` pin every wire artifact
(probe/ack/reflect framing, JSON encodings, token derivation, sanitize outputs
under both presets) byte-for-byte. A change that fails the golden test is a
protocol change and must be treated as one (see `ProtoVersion`).

## Pin/upgrade procedure (wire changes)

1. Edit punchcore in an OpenRung PR — the hub, relay runtimes, and desktop clients
   consume it via the in-repo `replace`, so servers and desktop stay atomically
   consistent.
2. Bump `punchcore/VERSION` in the same PR — a `go-checks` job fails any PR
   that changes punchcore without a fresh, untagged version.
3. Merge. `punchcore-tag.yml` tags `punchcore/v$(VERSION)` on the merge commit
   automatically (the nested-module tag is what makes the module fetchable via
   the Go proxy). No manual tagging.
4. Dependabot in the mobile repo (scoped to this module) opens the
   `android/punchbridge/go.mod` (+`go.sum`) bump PR when it sees the new tag;
   the bump automatically busts the AAR CI caches. Manual fallback:
   `go get github.com/openrung/openrung/punchcore@vX.Y.Z` in
   `android/punchbridge`.
5. Rebuild the AAR via `android/build-libbox-release.sh` and ship.

Local cross-repo development: use
`PUNCHCORE_SRC=/path/to/openrung/punchcore android/build-libbox-release.sh`
and/or an uncommitted `go.work`; never in releases (GPL §6 pins the module
version).

## License

GPL-3.0-or-later, same as the parent repository (see `LICENSE` in this
directory).
