# connectcore

`github.com/openrung/openrung/connectcore` is OpenRung's UI-agnostic client
policy engine, promoted to a nested Go module (ADR-001 D3) so clients outside
this repository — the mobile apps foremost — can pin and consume the same
implementation the desktop app and terminal client run.

The module root is the connection engine: the connect state machine, candidate
ladder and client-side ranking, relay-local WSS front fallback, the punch
attempt policy, mid-session health monitoring and failover, the directory
cache, and the internet/readiness probes. Its subpackages carry the rest of
the client policy layer:

- `client` — the sing-box config builder (TUN and proxy inbounds, mobile's
  DoH-failover DNS shape, split tunneling), the sing-box process runner, relay
  selection and usability, and the thin broker-client adapters.
- `clienttelemetry` — the closed failure-reason classifier and the telemetry
  event model, outbox, and session manager shared with the mobile apps.
- `discovery` — staggered multi-front relay-directory fetching over
  `brokerapi`, decoded into the exported relay schema.
- `proxyconfig` — the stable local proxy endpoint policy and the opt-in shell
  integration.
- `contract` — the embedded cross-repo contract vectors (failure
  classification, relay decoding, broker fronts) and their Go loaders; the
  mobile repository vendors the JSON files under `contract/vectors/`.

Everything platform-specific reaches the engine through narrow interfaces
(`interfaces.go`): event sink, persistence, OS proxy control, elevation, and
the NAT punch transport (`PunchEstablisher` — implemented in the root module's
`internal/enginepunch` over the quic-go transport in `internal/punch`, which
deliberately stays out of this module). Relay wire types come from `brokerapi`'s exported relay schema, and
the WSS transport mechanics from `wsscore`; both are sibling nested modules
this module pins by version for external consumers, while in-repo builds use
local `replace` directives.

Versioning follows the repository's nested-module convention: bump `VERSION`
in the same PR as any change (CI enforces it), and the tag workflow publishes
`connectcore/vX.Y.Z` on merge. Because external consumers resolve the sibling
modules through this module's `go.mod` requirements rather than the local
replaces, a change that relies on new `brokerapi`/`wsscore`/`punchcore` API
must bump that sibling's VERSION and this module's `require` line together.
See `docs/architecture.md` for the pin/upgrade procedure.
