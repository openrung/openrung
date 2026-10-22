# ADR-003 B2/B3 host prerequisites

The host APIs introduced in **connectcore/v0.6.0** support the B2/B3 mobile
cutovers. The recovery and location corrections against core main `9f141a3`
target **connectcore/v0.6.1** after merge. The `connectcore-tag` workflow creates
the tag; no release-build source rewriting or manual tag is needed. CLI/desktop
recent-node projections keep their existing wire/storage shapes.

The original prerequisite audit used core main `77708e6` and mobile main
`53e03d9` (including mobile #111 and preparation #112); its mapping follows.

## Source audit and requirement mapping

Paths in the source column are relative to mobile main `53e03d9`:
`android/app/src/main/java/com/openrung/` and `ios/` respectively. The mobile
`docs/ANDROID_ENGINE_CUTOVER.md` and `docs/ENGINE_BINDING.md`, plus ADR-003 in
the canonical Obsidian ADR collection, establish scope. Tests in
`../mobile_test.go` are independent shipping-source expectations; the seven
A4 sequence vectors and their versions are unchanged.

| Requirement / shipping source | Finding on core main; supported API | Independent tests (`TestMobile` prefix) |
| --- | --- | --- |
| `net/SingBoxConfiguration.kt`, `vpn/OpenRungVpnService.currentSplitTunnelRules`, `Shared/SingBoxConfiguration.swift` | Builder already has the mobile superset; Engine did not pass it. `MobileHost.Settings` supplies TUN addresses, MTU, resolvers, priority probe suffixes, split rules/packages, find-process, Clash accounting and log level. Engine fixes DoH shape and retains relay/bridge ownership. Settings refresh before each direct and WSS candidate, including recovery. | `CandidateConfigurationParityAndRefresh` (direct, punch, WSS; independent config/priority assertions), `StatusAcrossRecoverySwitchAndTeardown` |
| `ensureLocalTunnelPreconditions` | Core only built config after TCP reachability. `MobileRuntime.Preflight` validates direct **and** protected bridge graphs before TCP or tickets; `Settings` checks effective platform permission/availability. | `PreflightBlocksRemoteFailureAndTickets`, `MissingConfigurationFailsBeforeDiscovery` |
| `vpn/TunnelStartupGuard.kt`, both `StartupPathVerification` files | Desktop readiness was fixed/private. Each `MobileTunnelRun.WaitReady` and `VerifyPath` belongs to the fresh run; completion is guarded against cancellation and an already-observed engine exit. Unknown/local errors cannot authorize WSS. | `ReadinessMustPrecedeVerification`, `VerificationFailureClassificationAndStoppedTie`, `CancellationDuringReadinessAndVerification`, `ReadinessTimeoutMatchesDesktopAndPreservesCancellation`, `StopDuringLaunchAndRetry`, `FailedLaunchRetiresReporterAndAllowsCleanRetry` |
| `net/TunnelPathProbe.kt`, `DnsProbe.kt`, `InternetProbe.kt`, `ProbeTargets.kt`, `PacketTunnel/PacketTunnelDnsProbe.swift`, `PacketTunnelInternetProbe.swift` | Ordinary desktop TUN HTTP cannot prove an iOS provider path. `TunnelPathEvidence` requires the correct platform transport plus fresh DNS and pinned HTTPS; incomplete/physical responses are local failures. A typed `RemotePathError` retains DNS versus HTTPS staging. | `VerificationRejectsPhysicalAndIncompleteEvidence`, `VerificationFailureClassificationAndStoppedTie`, `HealthUsesCurrentRunAndLocalFailuresDoNotRecover` |
| Native `awaitTunnelHealthFailure` implementations | Mobile's traffic-aware cadence was absent. The shared loop now uses reported tunneled counters, 30 s ±1/6 ticks, healthy exponential allowance capped at 5 min, fast probes for uplink without reply or consecutive failure, and immediate network-epoch checks. Protected physical liveness gates remote recovery in mobile TUN mode. | `HealthCadenceMatchesShippingTrafficBudget`, `HealthUsesCurrentRunAndLocalFailuresDoNotRecover`, `HealthClassificationMatchesShippingThreshold`; existing network/lifecycle tests |
| `telemetry/ClientIdentity.kt`, `Shared/ClientIdentity.swift`, `OpenRungVpnModule.getIdentity` | Filesystem identity was hardwired. `MobileHost.InstallID` and `Engine.InstallID()` preserve the existing UUID verbatim without HOME/XDG changes. Discovery, WSS tickets, telemetry use the same identity, app version and platform version; mobile telemetry follows the verified discovery winner. | `WireIdentityForDiscoveryTicketsAndTelemetry`, `TelemetryFollowsWinningDiscoveryFront`, `IdentityTelemetryOwnershipAndRestart` |
| Both `TelemetryManager` files, `telemetry/NativeTelemetryOutbox.kt`, `android/punchbridge/telemetry_binding.go` | Existing `clienttelemetry.Outbox` already owns migration/fsync/repair/locking/batching. `MobileHost.Outbox` borrows that exact process-owned instance; Engine owns sessions/heartbeats/uploads. Existing `EnqueueBatch` is the supported durable migration acknowledgement. | `BorrowedOutboxKeepsSingleLockAndLegacyCopy`, `IdentityTelemetryOwnershipAndRestart`; existing `clienttelemetry/outbox*_test.go` durability/cancellation suites |
| `ApplicationConnectionAggregator.kt`, native libbox counters | `RunTelemetry` accepts reduced attributed counts and cumulative counters, retires stale runs and captures final Stop samples. Counts are chunked and use shared per-app batching even in the unavailable-store memory fallback. | `ReducedCountsSurviveUnavailableStoreFallback`, `IdentityTelemetryOwnershipAndRestart`, `HeartbeatMetadataAndTraffic`, `StatusAcrossRecoverySwitchAndTeardown` |
| Native platform/device metadata and ADR Track C | `MobileHost.Attributes` supplies current native metadata; protected `app_version`, `platform`, `engine=connectcore`, and `engine_version` identify releases. Other host metadata yields to event attributes on conflict. Metadata is sampled outside engine/manager locks. No global app-version mutation is needed. | `HeartbeatMetadataAndTraffic`, `IdentityTelemetryOwnershipAndRestart`, `WireIdentityForDiscoveryTicketsAndTelemetry`; `clienttelemetry.TestRecordEventAttributesWinExceptReleaseIdentity` |
| `state/OpenRungStatusStore.kt`, `model/RecentNode.kt`, RN contract | `ActiveConnectionInfo` already exists but cannot be called in a synchronous sink. `State.Details` atomically carries session and credential-free relay ID/name, geographic `LocationLabel`, effective class/transport/front; mobile `RelayLabel` is geographic or null. `RecentNode` adds relay ID/name and mobile deduplicates by relay (including legacy country entries). | `StatusAcrossRecoverySwitchAndTeardown`, `StateRemainsAtomicDuringSlowTeardown`, `PromoteKeepsAllLocationSurfacesGeographic` |

## Binding handoff

Configure one `Engine` with `Platform`, `SetMode(ModeTUN)`, `Elevation`, existing
`PunchEstablisher`, network/protection hooks, `Sink`, `Persistence`, and
`Mobile: &MobileHost{...}` before `Start` or `Connect`. `Mobile` is a host
contract, not a runtime engine selector; B2/B3 still remove native orchestration.
Do not set `Engine.TunnelRuntime` or `TelemetryOutboxDirectory` with `Mobile`.
The mobile host requires a non-nil borrowed outbox, valid native UUID, app
version, settings supplier, and mobile runtime. Android also requires the
current `SocketProtector`. iOS construction must still enforce the provider
process/platform as the existing B1 constructor does. Supply `PlatformVersion`
(Android API / iOS version) and platform attributes from native APIs.

`MobileHost.Settings(ctx)` replaces the B1 fixed candidate shape. Return an
owned effective snapshot: MTU 1400 and release `warn` logs, Android find-process,
Clash accounting, current probe suffixes, staged country rules and installed
excluded packages. Preserve the shipping **fail-open preference resolution**:
re-derive automatic countries from current timezone, drop missing rule sets and
uninstalled packages toward full-tunnel. Malformed *effective* config or denied
VPN permission is a local error. Do not pass raw invalid preferences through as
fatal config. `MobileRuntime.Preflight` wraps libbox CheckConfig without opening
TUNs/services/network sockets; the engine supplies both graph variants.

Adapt B1's per-run libbox service wrapper to `MobileRuntime.Run(ctx, configJSON,
reporter)`. It passes the generated bytes unchanged, retains the reporter with
that run, and returns a `MobileTunnelRun`. Preserve B1's launch cleanup and
incomplete-teardown latch: never launch a successor over a service that has not
closed. If Run fails, unwind partial services and drain final telemetry before
returning. If cancellation races a returned handle, Engine stops that handle.
A Stop error remains an OS-owner responsibility, as in B1: keep the provider/TUN
owner until actual cleanup completes, or use the platform's process lifecycle.
Go cannot forcibly kill an in-process native call safely.

`WaitReady(ctx)` must identify this run's new Android VPN Network or applied iOS
provider TUN settings. It must not select an arbitrary old VPN Network and must
not issue an HTTP request as readiness proof. Engine applies its 15 s TUN ready
limit. `VerifyPath(ctx, VerificationStartup)` calls the shipping fresh-DNS then
HTTPS **leaf probes**, preserving their retries; `VerificationHealth` runs one
sweep. Keep the DNS query nonce/transaction validation, in-TUN DNS successor
address, uncached priority rules, chain-sized attempt budget (6 s for the default
2 s + 3 s chain plus margin), startup retry budget (12.25 s), HTTPS retry budget
(12 s), request budget (3 s), authenticated TLS, no redirects/caches, and 2xx
acceptance. Probe names and configured suffix pins must agree. Bound adapters
must finish when the context ends and cancel the underlying native operations.

Android uses this run's VPN-bound DNS datagram/HTTP transports and attests
`PathAndroidVPN`. iOS uses `createUDPSessionThroughTunnel` and
`createTCPConnectionThroughTunnel(enableTLS: true)` and attests
`PathIOSProviderTunnel`. Ordinary extension URLSession/Go HTTP requests bypass
the provider TUN and **cannot** produce this evidence. Return both evidence flags
only after fresh DNS and pinned HTTPS pass. Provider/API state, permission,
missing VPN Network and engine-stop errors remain local. Only the shipping
allow-listed remote failures become `RemotePathError{Stage: "dns_probe" |
"internet_probe", Err: ...}`; preserve native cancellation as context cancellation.
Keep the per-service stopped/expected-stop guard from the shipping startup
wrapper: an observed local stop wins over a simultaneous remote-looking result.
Engine also rechecks Done before accepting the completed mobile probe result.
Do not retain the native ladder, health scheduler or ticket/recovery policy.

Health classification is deliberately strict, matching Android
`OpenRungVpnService.awaitTunnelHealthFailure` and iOS
`PacketTunnelProvider.awaitTunnelHealthFailure`: both immediately throw a local
error when `isGenuineRemoteDataPathFailure` rejects the native error. One unknown
exception, bare inner timeout, misspelled `RemotePathError.Stage`, or incomplete
path attestation therefore terminates the session as FAILED without recovery.
Only recognized remote DNS/HTTPS errors get three consecutive failures, reset by
a successful sweep, followed by the physical-network liveness gate. B2/B3 must
translate native allow-listed inner probe timeouts into `RemotePathError` with the
actual stage, rather than passing exception strings. Supplied-context cancellation
remains cancellation. This differs deliberately from desktop's generic health
error gate; it prevents lost native classification from authorizing recovery.
The engine's own readiness budget uses desktop's “tunnel did not become ready in
time” error and classification; parent cancellation/deadlines retain their
context error identity.

`RunTelemetry.UpdateTraffic` receives cumulative tunnel bytes; it keeps the
shipping session high-water semantics through counter resets, and Engine uses
run-local samples for the shared adaptive health cadence. It does **not** sum
reset counters (the shipping native policy also uses high-water marks). Keep the
15-minute `ApplicationConnectionAggregator` as an attribution/reduction leaf:
exclude DNS/own-package flows, send its chunks through
`RecordApplicationConnections`, and drain suppressed tail counts before this
run's Stop returns. Do not attach geo/destinations to application rows. Isolate
reducer state by run and drain before replacement so counts cannot be attributed
to the successor's relay. Reporter methods return false after retirement. A
failed launch also retires its reporter. Report final counters during Stop,
not by querying a later global libbox owner. Unflushed reducer memory can still
be lost on process death, and the bounded outbox can evict old events, matching
the shipping dashboard-grade approximation.

Lifecycle calls run on the host IO queue. `Sink` callbacks remain synchronous
and must enqueue/copy and return without reentering Engine. Translate the supplied
`State.Details` and `Recents` on the native queue; no synchronous
`ActiveConnectionInfo`/`SessionID` query is needed. Non-connected events clear
relay details; recovery preserves its session; terminal events clear details.
`State.Details.LocationLabel` contains only city/country (including city-only
geo). Empty means native UI should use its localized unknown-location string.
Sanitize location and operator names before display. Mobile `State.RelayLabel`
is null when geo is absent, so null-coalescing consumers retain a fallback.
`Details.LocationLabel` and recent location labels use empty strings for absent
geo; all three otherwise carry the same geographic-only value. Operator-name
and relay-ID fallbacks remain desktop-only. Use
`Details.LocationLabel` in mobile bindings to keep the purpose explicit, and
keep the separately supplied `RelayName` distinct from location.
Polling returns the same complete snapshot as the last state event while
resource teardown is in progress. Continue B1/B2's event sequence ordering and
retired-OS-owner filtering. Join Shutdown before replacing the native event
owner. Use `SetSocketProtector`, `SetDNSServers`, `UpdateNetworkState`,
`Pause`/`Resume` for existing lifecycle integration; never mutate public options
while running. Mobile teardown cancels/joins each run’s health worker; Shutdown also joins the heartbeat before final session
records, then honors the existing bounded terminal-flush contract. Pause does
not pause the data plane; that remains OS/libbox lifecycle work. Android screen
state is not suspension: do not pause recovery on SCREEN_OFF.

Mobile physical liveness accepts either a neutral HTTPS response or a successful
broker-front TCP connection. It first sends protected HEAD requests to the
gstatic/Cloudflare generate_204 endpoints, then falls through to the existing
protected front dials if neither responds. Neither set is required to be
reachable: blocked fronts cannot veto a neutral response, and blocked neutral
hosts cannot veto a reachable front. The dedicated OpenRung tunnel-probe hostname
is not used. This gate permits recovery; it does not prove tunnel health.

Each neutral request has a five-second total budget (`RelayTCPTimeout`), covering
DNS, TCP, TLS and response headers. This is not exact Android timeout parity:
the former Kotlin implementation had separate three-second connect/read budgets
with DNS outside them. Neutral requests verify TLS, do not follow redirects,
and send no application identity headers. Any TLS-verified response permits
recovery. All shared physical HTTP transports (geo, punch coordination and
liveness) bypass process proxies. Socket-protection refusal permits the ladder
to surface its terminal local failure instead of waiting for connectivity.

Hosts should also report IPv6 availability on every observation:
`NetworkState.IPv6Known = true` with `HasIPv6` set from the physical (non-VPN)
network. On Android, that means its `LinkProperties` carry a global IPv6
address and an IPv6 default route; on iOS, `NWPath.supportsIPv6`. When
`HasIPv6` is false, relays that an access answer names by IPv6 are dialled at
their public (IPv4) endpoint with the same per-user credential, and IPv6-only
relays move to the end of the ladder. Without a report, a mobile engine changes
nothing up front, because the VPN may own the route table. On every platform, a
failed TCP check to an IPv6 relay endpoint is retried once on that relay's
public endpoint.

An observed mobile `NetworkState.Up == false` skips physical probes; before the
first observation, probing remains enabled. Post-teardown outage polls double from
5 seconds toward a 60-second ceiling, always clipped to the remaining budget.
With immediate probe failures, no resume, and the default two-minute budget, waits are
5, 10, 20, 40, then at most 45 seconds; the 60-second plateau is not reached.
A network epoch wakes the wait immediately and resets backoff, but does not
renew the budget.

One two-minute budget covers the whole mobile recovery episode, starting when
health first holds recovery or when a transport death triggers teardown. The
health hold transfers its budget to the supervisor; punch classification,
post-teardown waits, and subsequent failed ladders reuse it. All mobile
transports can retry when the physical network goes down during a ladder, but
a failed ladder after budget expiry ends with `failover_exhausted`. Expiry
permits a final ladder attempt; it never grants a new outage hold. The health
loop releases at its next failed sweep after expiry; waits clip probes and
timers to the same deadline. This deliberately trades indefinite automatic
waiting for a terminal error and manual reconnect on a sustained outage.

An actual `Resume` renews the budget so suspended time cannot force immediate
expiry. The next eligible check evaluates physical connectivity with the renewed
budget before deciding recovery. Idempotent Resume calls while already running
do not renew it. Stale down signals or blocked reference hosts alone cannot hold
CONNECTED/CONNECTING forever. Desktop retains its existing broker-front
TCP gate and fixed recovery polling without these mobile hold budgets.

Mobile recents retain two distinct relays in the same country. At the audited
mobile main `53e03d9`, `src/components/RecentsSection.tsx` already keys pills by
`relayId`, selects the exact relay, and hides unpinned legacy entries; the native
mock also deduplicates pinned entries by relay. Core replaces an old unpinned
country entry when adding a pin, while desktop keeps country-only deduplication.
B2 must confirm that the target RN/map integration preserves both same-country
pins and selects the intended relay; country-grouped map markers must not become
the identity of the recent list. This remains a UI acceptance check, not a mobile
UI change in this PR.

## Outbox migration ordering

1. Initialize native install identity and paths without changing process
   environment. Stop/join legacy orchestration, its session/heartbeat/upload jobs,
   and associated callbacks. Do not start Engine yet.
2. Reuse the binding's existing `*clienttelemetry.Outbox` (the Go binding can lend
   its private `queue` inside the same package), or close its old owner and reopen
   the **same path** once. Android's shipping file is
   `openrung_telemetry_outbox.jsonl`, not the desktop engine's hyphenated filename.
   iOS keeps its existing App Group location/name. Do not copy a live locked file
   or create a parallel outbox over it.
3. If a SharedPreferences/legacy blob remains, decode and call existing
   `EnqueueBatch`. Clear that blob only for a nonnegative result; `-1` retains the
   only durable copy and must retry. Existing file-format migration, torn-tail
   repair, fsync, poison handling and broker event-ID dedup stay in Outbox. Keep
   the blob on malformed/unavailable input until the existing migration adapter
   has classified it. Lend the same instance through `MobileHost.Outbox`.
4. The store's sender must retain the platform's protected/owned physical socket
   path, app/platform headers, secure broker URL policy, cancellation, and
   `ErrBatchRejected` handling. Engine uses the sender already associated with
   the borrowed store; it never swaps it or constructs a competing lock. Disable
   legacy upload/session/heartbeat ownership; Engine now owns all three.
5. `Shutdown(flushBudget)` leaves pending durable events and keeps the borrowed
   store open for the next connect/service owner. At process-owner destruction,
   join Shutdown before `Outbox.Close`. Reopen that same file on process restart.
   The shared store retains its existing best-effort in-memory fallback on disk
   failure; a persistence failure cannot promise crash durability.

## Resolved divergences

- Core main already has v0.5.1's DNS port-53 hijack/probe hardening. This PR reuses
  it, the existing DoH/split builder, A3 durable outbox and socket/network hooks.
  Mobile's v0.5.0 pin crosses those already-reviewed DNS differences when it pins
  v0.6.0; this PR does not regenerate or weaken their goldens.
- Protected mobile TUN now supports WSS. Desktop's TUN WSS refusal and default
  config/probe behavior remain unchanged. Credentials, transport selection and
  bridge endpoints remain engine-owned.
- Unknown readiness/path/provider errors fail locally; only explicitly classified
  remote DNS/HTTPS failures authorize fallback. This preserves native startup
  and active-health classification rather than desktop's generic probe errors.
  During health, one unclassified error immediately fails the session; native
  adapters must preserve recognized remote timeout/stage types across the binding.
- Shared mobile health now preserves native traffic backoff and three-failure
  gating for classified remote failures only. Physical liveness combines protected
  neutral HTTPS and broker-front TCP, skips probes on observed mobile down states,
  and bounds outage holds as described above. A network epoch forces an immediate
  through-tunnel check on a live direct path. Neither is tunnel readiness evidence.
- Engine keeps its A4 single session across automatic recovery and its ranked,
  failed-relay-demoted ladder. Native reconnects can create replacement sessions;
  B2/B3 must use Engine's session identity for counters/events and compare Track C
  metrics at the app-version/platform/engine cohort level. No new success or
  heartbeat owner is created by native recovery.
- Mobile stores are explicitly supplied; a missing store is a local host setup
  error, rather than silently selecting B1's in-memory default. Existing store
  disk-failure fallback remains best-effort. Reporter retirement additionally
  rejects stale callbacks that formerly could be attributed to a current global
  native session. Desktop output/storage formats and A4 vectors are unchanged.

## Validation and remaining acceptance

The v0.6.1 recovery/location correction is covered by the full `connectcore`
race suite and vet, including both reachability directions, known-down probe
suppression, expiry across direct/punch/WSS supervisor recovery, shared health/retry budgets,
resume after suspension past the old deadline, network-signal wakeup, a response slower than
three seconds, TLS rejection, cancellation, physical proxy bypass, and mobile
location projection with desktop fallback compatibility.

The original v0.6.0 prerequisite also received this local validation on macOS
arm64 / Go 1.26.4:

- `connectcore`: `go test -race ./...`, `go vet ./...`, `go build ./...`.
- Root, `desktop/`, `desktop-volunteer/`: `go build ./...`, `go test ./...`.
- Existing Go CI checks: root tagged vet/race suite (`with_utls,with_external_windivert`),
  Windows client and desktop compilation, desktop vpnservice vet/race and version
  injection test, volunteer vet/version test, brokerapi/punchcore/wsscore vet/race.
- Both real frontend builds; desktop frontend 21 tests, volunteer frontend 38.
- Contract vectors unchanged; sibling require pins still match sibling VERSIONs.
- Docker image smoke builds are left to existing GitHub CI (Docker is unavailable locally).

B2/B3 still must bind these APIs, pin the published tag and sync mobile contract
pins, rebuild both release artifacts without source patches, check ABI symbols,
run the Go binding/TypeScript/Jest/full native suites and native A4 runners, and
remove superseded Kotlin/Swift orchestration. Preserve leaf probes, platform TUN,
socket ownership, attribution and preference resolution until bound equivalents
pass independent parity tests. No Kotlin/Swift cutover or mobile build/device
validation is claimed here.

Device acceptance remains stop-during-start, rapid reconnect/stale callbacks,
protection rejection, Wi-Fi/cellular changes, paused epoch changes, sleep/wake,
service/provider restart, migrated/pending telemetry, and real through-TUN DNS/
HTTPS evidence (including dead tunnel with live physical Internet). iOS must
measure the running extension against the agreed memory budget. Track C still
owns internal/beta cohort evidence, promotion/abort criteria and a tested recovery
release; this core PR supplies no device or public-release acceptance.
