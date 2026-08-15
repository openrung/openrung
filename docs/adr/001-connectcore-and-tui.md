# ADR-001: Shared client engine (`connectcore`) and TUI client rewrite

**Status:** Accepted (2026-08-15 — TUI stack and headless-subcommand retention confirmed by maintainer)
**Date:** 2026-08-15
**Deciders:** OpenRung maintainer
**Scope:** `openrung` repo (client + relay apps) and `openrung-mobile-app` repo (convergence track)

---

## Context

OpenRung ships four end-user clients (CLI, desktop GUI, Android, iOS) and two relay
apps (relay CLI, desktop-volunteer GUI). The protocol layer is genuinely unified:
the nested Go modules `brokerapi` (signed directory, front racing, no-SNI TLS),
`wsscore` (WSS transport), and `punchcore` (NAT punch) are single-sourced and
consumed everywhere — in-tree via `replace` on desktop/CLI, and on mobile through
pinned tags compiled into the libbox gomobile binding.

Everything **above** that layer — connection lifecycle, failover policy, WSS
fallback policy, relay ranking, failure classification, telemetry outbox,
sing-box config generation — is written repeatedly:

| Concern | Copies today |
|---|---|
| Connect state machine (ladder, failover, health) | Kotlin (~1.8k loc), Swift (~2k), desktop Go `desktop/vpnservice` (~2.6k); CLI has a different, simpler linear flow |
| Failure classification token ladder | Go `internal/clienttelemetry/classify.go`, `FailureClassifier.kt`, `FailureClassifier.swift` — self-declared mirrors, convention-only |
| Telemetry outbox/manager | Kotlin and Swift, independently implemented |
| sing-box config generation | Go `internal/client/singbox_config.go`, `SingBoxConfiguration.kt`, `SingBoxConfiguration.swift` — mobile diverged ahead (split tunneling, IR/CN DoH) |
| Relay orchestration | `cmd/relay/main.go` and `internal/relayruntime/engine` mirror each other with drift in both directions |
| Relay descriptor model | 4 copies (Go, Kotlin, Swift, TS) |

Drift has already caused production incidents: the Winsock errno gap fixed in
wsscore is still live in `internal/clienttelemetry/classify.go`; the mobile 0.3.5
telemetry silence came from rewriting one of two independent outbox
implementations; the WSS idle cliff had two duplicated guards. The current
reference implementation of the connect contract is the **mobile Kotlin** code —
desktop's Go state machine describes itself as a port of it — so canonical logic
lives in a platform language and is hand-ported outward.

Separately, **the CLI client (`cmd/client`) is badly behind** and will be totally
rewritten: it has no front racing, no WSS fallback, no failover ladder, no health
monitoring, no persistence, and a plain linear UX. The decision is to rebuild it
as an interactive TUI that mirrors the desktop GUI / mobile UX and logic.

**Constraints:** solo maintainer with agent-implemented PRs (review flow: agent →
maintainer review → merge); GPL-3.0-or-later; mobile apps release independently
against pinned module tags; nested modules cannot import root-module `internal/`
packages; existing VERSION-file → auto-tag → Dependabot pipeline must be reused,
not reinvented.

## Decision

1. **Extract a shared client engine, `connectcore`**, from `desktop/vpnservice`
   (the newest and richest Go embodiment of the mobile contract). It owns the
   connection state machine, candidate ranking, WSS fallback ladder, punch
   attempt, health monitoring/failover, directory cache, and probes — behind
   narrow platform interfaces (event sink, persistence, OS-proxy hook,
   elevation hook). It lives in `internal/connectcore` first; it is promoted to
   a nested module only when mobile adopts it (Track D), because nested modules
   cannot import root internals and early promotion would force moving
   `internal/client`, `internal/clienttelemetry`, and `internal/punch` wholesale.

2. **Rewrite `cmd/client` as an interactive TUI** (Bubble Tea + Bubbles +
   Lip Gloss; MIT-licensed, GPL-compatible) that is a pure view over
   `connectcore` — the same engine the desktop GUI uses. Proxy mode becomes the
   default (no privileges, mirrors desktop); TUN mode is retained behind a flag
   with an explicit sudo/elevation path. Headless subcommands (`check`,
   `config`, `connect --headless`) are preserved as thin engine drivers so
   scripts and docs keep working.

3. **Unify the two Go relay orchestrators**: `cmd/relay` becomes a CLI frontend
   over `internal/relayruntime/engine` after the engine absorbs the
   cmd/relay-only features.

4. **Converge mobile onto shared Go policy code** in stages: first
   machine-checked golden contract vectors (classification tokens, descriptor
   decode) shared across repos; then upstream mobile's sing-box config features
   into the Go builder; then promote the policy layer to a nested `connectcore/`
   module and expose classifier, config builder, and telemetry outbox through
   the existing punchbridge/libbox binding channel. Full mobile state-machine
   adoption is future work under a follow-up ADR.

## Options considered

### Option A — Status quo + discipline (hand-synced ports, more review checklists)
**Pros:** zero migration cost; no release coupling.
**Cons:** drift is already causing incidents; the reference implementation stays in Kotlin; the CLI rewrite would create a *fourth* orchestration copy.

### Option B — Shared Go engine, staged extraction, TUI as first new consumer *(chosen)*
**Pros:** extends the already-proven nested-module/gomobile pattern one layer up; CLI rewrite lands on shared logic instead of adding a copy; incremental and reversible per phase; desktop behavior preserved by construction (refactor-first).
**Cons:** engine changes eventually require mobile pin bumps + app releases (friction already carried for brokerapi/wsscore); gomobile binding surface grows.

### Option C — Big-bang: nested `connectcore` module + mobile adoption immediately
**Pros:** one migration.
**Cons:** forces untangling root-internal imports, binding design, and mobile release coordination in a single high-risk change; blocks the CLI rewrite on cross-repo work.

## Consequences

- **Easier:** the CLI inherits front racing, WSS fallback, failover, ranking for
  free; bug fixes to connect policy land once; the Go engine becomes the
  reference implementation; mobile-first features (ranker, split tunneling) get
  upstreamed instead of ported by hand.
- **Harder:** desktop refactor risk during Track A (mitigated: behavior-preserving
  PRs with tests moved along); eventual engine/mobile release coupling.
- **Revisit later:** full mobile engine adoption (own ADR); TS relay-model
  generation from schema; unifying the two no-SNI TLS implementations
  (`brokerapi/nosni_tls.go` vs `wsscore/nosni_tls.go`).

---

## Phasing and dependency graph

```mermaid
flowchart LR
    subgraph Track A — engine extraction
        A1[A1 extract connectcore] --> A2[A2 dedupe cmd/client helpers]
        A2 --> A3[A3 share proxy/persist primitives]
    end
    subgraph Track B — TUI rewrite
        B1[B1 TUI skeleton] --> B2[B2 desktop parity] --> B3[B3 TUN + docs]
    end
    subgraph Track C — relay unification
        C1[C1 engine absorbs cmd/relay features] --> C2[C2 cmd/relay = frontend]
    end
    subgraph Track D — cross-platform convergence
        D1[D1 golden contract vectors] --> D3[D3 nested connectcore module]
        D2[D2 sing-box config superset] --> D3
        D3 --> D4[D4 mobile leaf-policy bindings]
    end
    A3 --> B1
    A1 --> D3
```

Tracks C and D1/D2 are independent of A/B and can run in parallel. Track B is
strictly serial after A. D4 requires a tagged connectcore module and lands in
the mobile repo.

---

## Common preamble (prepend to every agent prompt)

> You are working in the OpenRung repo at `/opt/projects/openrung`
> (GPL-3.0-or-later; Go 1.25 root module `openrung` with nested modules
> `brokerapi/`, `wsscore/`, `punchcore/` consumed via local `replace`). Branch
> off `main`; one PR per prompt. Conventions:
> - Do not modify `brokerapi/`, `wsscore/`, or `punchcore/` unless the prompt
>   says so; any change to a nested module requires bumping its `VERSION` file
>   in the same PR (CI enforces this).
> - Run `go build ./...` and `go test ./...` in the root module **and** in
>   `desktop/` and `desktop-volunteer/` before declaring done; `gofmt` clean.
> - Where the prompt says "behavior-preserving", produce no observable behavior
>   change; move existing tests along with moved code and keep them passing.
> - Match surrounding code style and comment density; do not add dependencies
>   beyond those the prompt names; update `THIRD_PARTY_NOTICES.md` when adding
>   any.
> - Registration, heartbeat, directory, and telemetry wire behavior must not
>   change unless the prompt says so.
> - Describe any deliberate behavior difference you had to resolve in the PR
>   description under a "Resolved divergences" heading.

---

# Track A — engine extraction (openrung repo)

## PR A1 — Extract `internal/connectcore` from `desktop/vpnservice`

> **Goal (behavior-preserving refactor):** Extract the UI-agnostic client
> connection engine from `desktop/vpnservice` into a new root-module package
> `internal/connectcore`, leaving `desktop/vpnservice` as a thin Wails adapter.
>
> Today `desktop/vpnservice` (~2,600 non-test lines) mixes the engine with the
> Wails shell: `service.go` (state machine: statuses, connect ladder, candidate
> filtering, promote/teardown ordering, crash recovery), `monitor.go` (mid-session
> health probes, network-alive gate, automatic failover re-ladder), `ranker.go`
> (client-side latency ranking), `wss.go` (WSS front fallback ladder + ticket
> retry policy), `directory.go` (rate-limited directory cache), `punch.go`,
> `connect_helpers.go`, `reachability.go`, `internetprobe.go`.
>
> **Do:**
> 1. Create `internal/connectcore` containing the state machine, ladder,
>    ranking, WSS fallback, punch attempt, monitor/failover, directory cache,
>    and probes. The sing-box mode stays a parameter
>    (`internal/client.ModeProxy` / `ModeTUN`).
> 2. Define narrow interfaces owned by `connectcore` for everything
>    platform-specific: a status/log event sink (typed events, not strings), a
>    persistence interface (or config-dir path), an optional OS-proxy
>    apply/restore hook, and an elevation hook stub (used by TUN later).
> 3. `desktop/vpnservice` keeps: Wails event emission, the ringbuffer/log
>    coalescing, `desktop/proxymode` + `desktop/persist` wiring — implemented as
>    adapters over the new interfaces. It must not retain any copy of engine
>    logic.
> 4. `internal/connectcore` must not import Wails or anything under `desktop/`.
>    `cmd/client` is untouched in this PR.
>
> **Accept when:** desktop app builds and all moved tests pass unchanged;
> `desktop/vpnservice` contains no state-machine/ladder/WSS/monitor logic;
> `grep -r wails internal/connectcore` is empty.

## PR A2 — Deduplicate the drifted `cmd/client` helpers

> **Goal:** Collapse the known copy-paste pairs between `cmd/client` and the
> engine (now `internal/connectcore`, after PR A1) into single implementations,
> resolving each divergence explicitly.
>
> Known pairs and their drift:
> - Punch attempt: `cmd/client/main.go` `maybePunch`/`punchBaseURL`/`punchHTTPClient`
>   (~lines 343–409) vs the engine's `punch.go` (marked "Ported from
>   cmd/client/main.go maybePunch"). Drift: the CLI supports `-punch-url`
>   (engine doesn't); desktop constructed the engine with `PunchInsecure: true`
>   hardcoded while the CLI flag defaults to false.
> - Relay TCP reachability: `tcpReachMs` in `cmd/client/telemetry.go` (10s
>   timeout) vs the engine's `reachability.go` (5s via config, strips IPv6
>   brackets, wraps the error).
> - `flushOnShutdown`: duplicated in `cmd/client/main.go` and the engine's
>   `connect_helpers.go` (the engine copy adds a nil guard).
> - Relay-id/label targeting: inline filter in `fetchSelectedRelay`
>   (`cmd/client/main.go` ~305–316) vs the engine's `filterCandidates` (which
>   adds country targeting) — keep the engine's superset.
>
> **Do:** one implementation of each in `internal/connectcore` (or
> `internal/client` where it fits better), with options covering both callers.
> Resolutions: keep `-punch-url` as an engine option; make punch-insecure an
> explicit option defaulting to **false** — preserve desktop's current effective
> behavior by passing it explicitly, and flag the hardcoded `true` prominently
> in the PR description for maintainer review (do not silently change either
> app); adopt the engine's 5s reachability timeout as a configurable default and
> keep the CLI on its current 10s via the option. Delete every duplicate body.
>
> **Accept when:** no duplicated helper bodies remain (verify by search); both
> apps compile and pass tests; every behavior difference kept or removed is
> listed under "Resolved divergences".

## PR A3 — Promote desktop proxy/persistence primitives to shared packages

> **Goal (behavior-preserving move):** The upcoming TUI client needs the shell
> proxy helper, per-install persistence, and (optionally) OS proxy control that
> currently live under `desktop/`.
>
> **Do:** move `desktop/proxyconfig` (port-qualified `proxy-env-<port>.sh`
> generation), `desktop/proxymode` (per-OS system proxy controllers), and the
> reusable parts of `desktop/persist` (recents, persisted proxy port, proxy
> snapshot store) into root packages: `internal/proxyconfig`,
> `internal/proxymode`, `internal/clientstate`. Update desktop imports. Keep the
> config directory name `openrung` and all file formats byte-identical
> (existing user state must load). Do **not** touch `desktop-volunteer/persist`
> — it is deliberately a separate product with its own `openrung-volunteer`
> config dir.
>
> **Accept when:** desktop builds and passes tests; the moved packages import
> nothing under `desktop/`; a fresh build reads a config dir written by the
> previous build (add a compatibility test that loads a fixture of the current
> on-disk JSON).

---

# Track B — TUI client rewrite (openrung repo, after Track A)

## PR B1 — Replace `cmd/client` with an interactive TUI skeleton

> **Goal:** Total rewrite of `cmd/client` as an interactive terminal app that
> mirrors the desktop GUI's UX, built strictly as a view over
> `internal/connectcore` (see docs/adr/001). The old 475-line linear `main.go`
> flow is deleted, not preserved.
>
> **Stack:** `github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss`
> (MIT; add to root `go.mod`, update `THIRD_PARTY_NOTICES.md`).
>
> **Do:**
> 1. Views: **Status** (connection state, selected relay label/country/
>    node_class, transport path — direct / WSS front / punched, session
>    duration, local proxy endpoint); **Relays** (ranked candidate list from the
>    engine's directory cache + ranker: country, latency, foundation/volunteer
>    badge; manual relay selection like desktop's targeting); **Logs** (engine
>    log events in a scrollable ring buffer); **Settings** (broker URL override,
>    proxy/TUN mode, relay targeting by id/label/country).
> 2. Keys: connect/disconnect, refresh directory, view switching, quit with
>    graceful teardown (engine stop, telemetry flush, terminal restore — also on
>    panic).
> 3. Default mode is **proxy** (loopback mixed proxy, no privileges), matching
>    desktop. Persist the chosen port via `internal/clientstate` exactly as
>    desktop does (`OPENRUNG_PROXY_PORT` override honored, not persisted).
> 4. Preserve headless subcommands as thin engine drivers with their existing
>    flags: `check` (fetch + print selected relay), `config` (write sing-box
>    config), and add `connect --headless` (old non-interactive behavior, now
>    engine-backed). Plain `connect` (or bare invocation) launches the TUI.
> 5. The TUI layer must contain **zero** connection logic: no direct brokerapi,
>    sing-box, wss, or punch calls — engine events in, engine commands out.
>
> **Accept when:** builds on darwin/linux/windows (`GOOS` cross-compile
> suffices); a manual smoke test against a local broker + relay connects,
> shows status transitions, disconnects cleanly and restores the terminal;
> `grep` shows no brokerapi/wsscore imports under `cmd/client` outside the
> engine wiring; headless subcommands behave as documented.

## PR B2 — Desktop feature parity in the TUI

> **Goal:** Bring the TUI to functional parity with the desktop GUI's
> connection behavior — all of it via `internal/connectcore` configuration, none
> of it reimplemented in the view.
>
> **Do:**
> 1. Multi-front broker discovery: use the engine's brokerapi
>    `FirstReachable`/`BrokerCandidates` race (replacing the old single
>    `-broker` URL; keep `-broker` as an override that narrows candidates to
>    one).
> 2. Surface engine events in the UI: failover re-ladder (why + to which
>    relay), WSS fallback engagement (which front, ticket retries), punch
>    attempt outcome, mid-session health probe state.
> 3. Shell proxy helper: a Settings action that shows the two copyable
>    commands (enable/restore) from `internal/proxyconfig`, enabled only while
>    connected — mirroring the desktop Settings screen.
> 4. Recents: persist recently used relays via `internal/clientstate`, show
>    them in the Relays view.
> 5. Telemetry: construct the shared `internal/clienttelemetry` manager the way
>    desktop does, but introduce a distinct platform label (e.g. `PlatformCLI`).
>    Confirm the broker/dashboard tolerate an unknown platform string; call the
>    new label out in the PR description since it will appear in dashboards.
>
> **Accept when:** a forced direct failure (unreachable relay IP) visibly
> engages WSS fallback in the UI against a WSS-capable test relay, and a
> mid-session relay kill triggers a visible failover; telemetry events arrive
> at a local broker with the new platform label.

## PR B3 — TUN mode and documentation

> **Goal:** Retain full-device TUN mode in the TUI and update the docs for the
> rewritten client.
>
> **Do:**
> 1. `--tun` flag / Settings toggle: uses the engine with
>    `internal/client.ModeTUN` (existing `BuildSingBoxConfig` TUN shape:
>    `auto_route`, `strict_route`, DNS hijack, `route_exclude_address` for
>    relay IPs — unchanged). If not running with sufficient privileges, refuse
>    with a clear message telling the user to rerun under `sudo`; wire this
>    through the engine's elevation-hook stub from PR A1 rather than ad-hoc
>    checks.
> 2. Update `docs/desktop-client.md`: split the "Desktop CLI Client" section
>    into the new TUI client docs (views, keys, proxy vs TUN, headless
>    subcommands) — the old command examples (`go run ./cmd/client connect ...`)
>    must be replaced with working equivalents. Update the README quick-start
>    line that mentions the "desktop TUN CLI".
>
> **Accept when:** on macOS, `sudo go run ./cmd/client connect --tun` routes
> device traffic through a local test relay (document the manual test in the PR);
> non-sudo TUN attempts fail with the guidance message; docs contain no stale
> commands.

---

# Track C — relay orchestrator unification (openrung repo, parallel to A/B)

## PR C1 — `internal/relayruntime/engine` absorbs the `cmd/relay`-only features

> **Goal:** Make `internal/relayruntime/engine` (used by desktop-volunteer) the
> single relay orchestrator by porting in the features that today exist only in
> `cmd/relay/main.go`. `cmd/relay` itself is not rewritten in this PR.
>
> Context: `engine/engine.go` (~1,150 lines) documents itself as mirroring
> `cmd/relay/main.go` (~962 lines). Engine-only today: hub leaf-cert pinning
> (`HubCertFingerprint`), 429 Retry-After registration backoff, 5-min public-
> IPv6 rotation recheck, multi-port auto-mode probe. cmd/relay-only today:
> foundation token / node_class posture, WSS-fronts registration + capability
> signing, connection-log console output, `-print-config-only`.
>
> **Do:** port the four cmd/relay-only capabilities into the engine behind
> options, preserving these invariants exactly: registration/heartbeat use only
> the canonical routes and refuse redirects; foundation-token relays are forced
> into direct mode (the credential must never enter the hub path); WSS fronts
> remain gated to foundation class — keep the existing behavior where a
> non-foundation relay configured with WSS fronts fails fast rather than
> registering. Add engine tests for each ported feature (mirror the existing
> cmd/relay tests where they exist).
>
> **Accept when:** engine supports all four features under test;
> `cmd/relay` and desktop-volunteer still build and pass tests unchanged;
> invariants above each have an explicit test.

## PR C2 — Rewrite `cmd/relay` as a frontend over the engine

> **Goal:** Delete `cmd/relay`'s duplicated orchestration and make `main.go` a
> flag-parsing frontend over `internal/relayruntime/engine` (post-C1).
>
> **Do:** map every existing `cmd/relay` flag and env var onto engine options —
> the CLI surface is a compatibility contract (deploy scripts under `deploy/`
> and the volunteer one-liner depend on it; grep them and list each consumed
> flag in the PR). Delete the now-dead duplicates, including the byte-identical
> `stopProcess` and the unpinned `hubTLSConfig` (CLI relays thereby **gain**
> leaf-cert pinning support, Retry-After backoff, and IPv6 rotation rechecks —
> call these out as deliberate upgrades, off by default where they change
> behavior, e.g. pinning only activates when a fingerprint is supplied).
>
> **Accept when:** `cmd/relay -h` output covers the same flags as before;
> direct-mode and tunnel-mode registration against a local broker + hub work
> end-to-end; no orchestration logic remains in `cmd/relay` beyond flag mapping
> and console output; `deploy/` scripts run unmodified.

---

# Track D — cross-platform convergence (both repos, parallel start)

## PR D1 — Golden contract vectors + fix the Go classifier's Winsock gap

> **Goal:** The failure-classification token set and the relay-descriptor
> decode are load-bearing cross-repo contracts currently enforced only by
> convention across `internal/clienttelemetry/classify.go`,
> `FailureClassifier.kt`, `FailureClassifier.swift`, and the TS relay model.
> Make them machine-checked, and fix the known Go gap.
>
> **Do (openrung repo):**
> 1. Create `contract/vectors/` with versioned JSON files: (a)
>    `classification.json` — cases of {input error text / errno / HTTP status,
>    expected token}, generated to cover every branch of `classify.go`'s ladder
>    (cancellation → selection sentinels → HTTP status → errno → DNS → TLS →
>    permission → timeout → unknown), explicitly including Windows Winsock
>    codes (10060/10061/10065 etc.); (b) `relay_decode.json` — sample signed
>    directory JSON → expected parsed descriptor fields; (c)
>    `broker_fronts.json` — the advertised front URL list and phase ordering as
>    a snapshot.
> 2. Fix `classify.go` to classify Winsock error text/codes the same way
>    wsscore's classifier does (see the wsscore taxonomy work) — Go's
>    `syscall.ECONNREFUSED` does not match Winsock 10061 on Windows.
> 3. Add Go tests that run the vectors against `classify.go` and brokerapi's
>    decode; add a small doc header in the vector files stating they are
>    consumed by the mobile repo's Kotlin/Swift/TS suites and must only change
>    with a version bump.
>
> **Follow-up PR in `/opt/projects/openrung-mobile-app`:** vendor the vector
> files (with a check script that compares the vendored copy against the pinned
> openrung ref), and add JUnit/XCTest/Jest cases running them against
> `FailureClassifier.kt`, `FailureClassifier.swift`, and `src/model/relay.ts`.
>
> **Accept when:** all four language suites consume the same vectors; the
> Winsock cases fail against the old `classify.go` and pass against the fix.

## PR D2 — Make the Go sing-box config builder the superset

> **Goal:** Mobile's `SingBoxConfiguration.kt` (~485 lines) and
> `SingBoxConfiguration.swift` (~407) have diverged ahead of Go's
> `internal/client/singbox_config.go` (~243): split tunneling via `.srs` rule
> sets, IR/CN in-country DoH direct resolvers, punch outbound wiring, and
> platform DNS shapes. Upstream those capabilities into the Go builder so it
> becomes the single superset, in preparation for binding it into mobile.
>
> **Do (openrung repo, read-only reference to
> `/opt/projects/openrung-mobile-app/android/.../net/SingBoxConfiguration.kt`
> and `ios/Shared/SingBoxConfiguration.swift`):** extend `BuildSingBoxConfig`
> with an options struct covering the mobile features; add golden-file tests
> that reproduce, byte-for-byte where possible, the configs mobile's own tests
> expect for representative inputs (proxy, TUN, punched, WSS, split-tunnel IR,
> split-tunnel CN). Do not change the existing CLI/desktop output for existing
> option combinations — add a golden test freezing today's output first.
>
> **Accept when:** golden tests cover every mode; existing desktop/CLI configs
> are byte-identical to before; a table in the PR maps each mobile feature to
> its new Go option.

## PR D3 — Promote the policy layer to a nested `connectcore/` module

> **Goal:** Mobile can only consume fetchable modules. Promote the client
> policy layer out of root `internal/` into a nested module
> `connectcore/` (`github.com/openrung/openrung/connectcore`), following the
> exact conventions of `brokerapi`/`wsscore`/`punchcore`.
>
> **Do:**
> 1. Move into `connectcore/`: the engine (`internal/connectcore`), the failure
>    classifier + telemetry event/outbox/manager (`internal/clienttelemetry`),
>    the sing-box config builder (`internal/client/singbox_config.go` + engine
>    runner if cleanly separable), and the contract vectors from D1.
> 2. Untangle root-internal imports: client-side code must depend on
>    `brokerapi`'s relay schema (`relay_schema.go`) rather than
>    `internal/relay`; whatever `internal/punch` surface the engine needs moves
>    behind a `connectcore` interface implemented in the root module or comes
>    from `punchcore` directly. No root-module `internal/` imports may remain.
> 3. Add `connectcore/VERSION` (start at `0.1.0`) and wire the same
>    tag-on-merge workflow and go-checks VERSION-bump enforcement used by the
>    other nested modules. Root and `desktop/` consume via local `replace`.
> 4. Update `docs/architecture.md` with a connectcore section and pin/upgrade
>    procedure mirroring the existing module sections.
>
> **Accept when:** root, desktop, desktop-volunteer build and pass tests via
> `replace`; `connectcore` builds standalone (`go build ./...` inside it);
> CI enforces VERSION bumps; docs updated.

## PR D4 — Mobile adopts the bound leaf policies (mobile repo)

> **Goal (in `/opt/projects/openrung-mobile-app`, after a `connectcore/vX.Y.Z`
> tag exists):** Replace the Kotlin/Swift copies of the three leaf policies
> with the shared Go implementations, exposed through the existing
> punchbridge/libbox binding channel (`android/punchbridge/`, pattern of
> `broker_binding.go` / `wss_binding.go`).
>
> **Do, one policy at a time (separate commits, ideally separate PRs):**
> 1. **Failure classifier:** add a binding (error text/errno/status in → token
>    out); route `FailureClassifier.kt` and `FailureClassifier.swift` call
>    sites through it; keep the native files as thin errno/exception-to-input
>    adapters; the D1 vectors now run against the binding.
> 2. **sing-box config builder:** bind the D2 superset builder; delete
>    `SingBoxConfiguration.kt`/`.swift` generation logic, keeping only the
>    platform input assembly. The existing platform golden tests must pass
>    against the bound output.
> 3. **Telemetry outbox/manager:** bind the connectcore outbox (inputs: a
>    directory path from the platform, events in, brokerapi posting already
>    shared); migrate the on-disk NDJSON outbox format or provide a one-time
>    migration; delete `TelemetryManager.kt` outbox logic and the independent
>    Swift outbox (`TelemetryOutbox.swift` + state).
>    This is the 0.3.5-regression surface — add an explicit e2e test that
>    events written before upgrade are uploaded after.
> 4. Pin `connectcore` in `android/punchbridge/go.mod`; both
>    `build-libbox-release.sh` scripts must pick it up via their existing
>    pin-extraction (add `CONNECTCORE_SRC` local-dev override matching the
>    existing `*_SRC` pattern).
>
> **Accept when:** both platform VPN test suites pass; the deleted Kotlin/Swift
> logic has no remaining callers; vectors run against the binding on both
> platforms; outbox migration test passes.

---

## Explicit non-goals

- Sharing UI code between Wails/React, React Native, and the TUI.
- Moving `VpnService` / `NetworkExtension` / elevation lifecycle into Go.
- Full mobile adoption of the connectcore state machine — future work,
  separate ADR, only after D4 has soaked in a release.
- Unifying `brokerapi/nosni_tls.go` with `wsscore/nosni_tls.go` (backlog).
- Generating the TS relay model from a schema (backlog; D1's decode vectors
  cover the drift risk meanwhile).

## Action items

1. [x] Maintainer accepts this ADR (TUI stack and headless subcommands confirmed 2026-08-15; `PlatformCLI` label and D3 module name to be confirmed at their respective PRs).
2. [ ] Hand out A1 → A2 → A3 serially; C1 and D1/D2 may start in parallel immediately.
3. [ ] After A3: B1 → B2 → B3.
4. [ ] After D1+D2+A1: D3, then tag and hand out D4 in the mobile repo.
