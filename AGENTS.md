# OpenRung — agent guide

## Public repository — censorship opsec

`openrung/openrung` is public. **Document mechanisms, never circumvention
intelligence**, including in commit messages and PR titles/bodies:

- Reachability measurements, per-front results or latencies, vantage points and dates.
- Observed censor behaviour, explanations of why fronts survive, or health assessments.
- Probing methodology or plans for undeployed fronts.
- Known-unmitigated weaknesses, including abuse or rate-limit gaps.

Keep intelligence in the private operations notes (Obsidian vault). Publicly state
only the decision, without reasons or evidence. Before committing changes to broker
fronts, discovery order or blocking, review the diff and commit message as a censor would.

## Overview and code map

OpenRung routes clients through Foundation and volunteer relays using
VLESS + Reality + Vision. The broker handles discovery, never user traffic;
relay hubs support reverse tunnels for volunteers behind NAT.

Go multi-module repo; two Wails apps with React/TypeScript/Vite frontends.
Mobile apps live elsewhere and pin shared-module releases.

| Path | Owns |
| --- | --- |
| `cmd/` | Broker, relay, relayhub, client CLI and WSS sidecar entry points. |
| `internal/` | Server implementations, runtime and platform integration. |
| `connectcore/` | Shared client policy: discovery, connection lifecycle, failover and telemetry. |
| `brokerapi/` | Broker HTTP client, response verification and relay schema. |
| `punchcore/`, `wsscore/` | NAT punch and Reality-over-WebSocket mechanics. |
| `desktop/` | Client app: `vpnservice/` and `frontend/`. |
| `desktop-volunteer/` | Volunteer app: `directsetup/`, `volunteerservice/`, `frontend/`. |
| `deploy/`, `scripts/` | Container images, compose files, the volunteer bring-up script and release helpers. Foundation provisioning and front tooling live in the private `openrung/ops` repository. |
| `.github/workflows/` | Authoritative checks and release workflows. |

## Design and changes

- Prefer the simplest design that meets the requirements. If two approaches achieve
  the same result, choose the one with fewer abstractions, dependencies and moving
  parts. Reuse existing patterns; add complexity only for a concrete need.
- Keep shared client policy in `connectcore`, protocol mechanics in their owning
  modules, and platform integration in host adapters. Read the relevant module README.
- Preserve wire/API compatibility across independently released consumers.
- Shared-module changes need a fresh `VERSION`, except module `README.md`-only edits.
  Keep `connectcore/go.mod` sibling pins aligned with sibling versions. App changes
  do not require a bump every time; Wails bumps must also update `wails.json`.
  See [versioning](docs/versioning.md).
- Contract-vector edits/renames require a higher vector version and updated Go test
  pin; see [contracts](connectcore/contract/README.md).
- Follow [CONTRIBUTING.md](CONTRIBUTING.md): DCO sign-offs, SPDX source headers,
  and notices updates for shipped dependency changes.

## Validation

Use Go versions from `go.mod`; frontend CI uses Node 22 and npm lockfiles.
Local `replace` directives wire modules together; `go.work` is gitignored.
Format changed Go files with `gofmt`; check affected modules and consumers.

| Run from | Checks |
| --- | --- |
| Root | `go vet -tags with_utls,with_external_windivert ./...`; `go test -race -tags with_utls,with_external_windivert ./...` |
| Each affected shared module | `go vet ./...`; `go test -race ./...` |
| `desktop/` | `go vet ./vpnservice/...`; `go test -race ./vpnservice/...` |
| `desktop-volunteer/` | `go vet ./directsetup/... ./volunteerservice/...`; `go test ./directsetup/... ./volunteerservice/...`; `go test version.go version_test.go` |
| Either `frontend/` | `npm ci`; `npm test`; `npm run build` |

- Root tests exclude nested modules. `make test` also omits `connectcore` and both apps.
- Client builds need `-tags with_utls,with_external_windivert` for Reality support
  and exclusion of the embedded WinDivert driver.
- Full Wails builds need frontend assets and native dependencies; service tests alone
  do not validate the GUI. Relay integration tests need Xray pinned by `deploy/relay/Dockerfile`.
- Consult the relevant CI workflow for additional checks. Report what ran and any skips.

## User responses

Keep responses short, concise and direct. Lead with the result and include only
what the user needs to know: relevant changes, validation, and any limitations or
required action. Skip internal deliberation, process narration and repeated summaries.

## Further reading

[README](README.md) for local setup; [architecture](docs/architecture.md) for
component boundaries; [API](docs/api.md) for broker contracts;
[desktop](docs/desktop-client.md), [volunteer](desktop-volunteer/README.md),
and [WSS](docs/wss-fallback.md) for component details. Deployment instructions
live under `deploy/`. Keep this guide brief and update it when paths or workflows change.
