# CloudFront-first discovery rollout

The shared discovery attempt order is:

1. CloudFront (`https://d2r7mdpyevvs1m.cloudfront.net/`) without SNI, retaining
   exact distribution hostname verification.
2. Azure (`https://cdn-edge-cxdnhsg2aadmaubj.z02.azurefd.net/`) with normal SNI
   and exact endpoint hostname verification.
3. Cloudflare (`https://broker.openrung.org/`) with existing opportunistic ECH.
4. Azure again without SNI, authenticating the shared Azure edge certificate
   and relying on the signed relay list for broker identity.

The first three retain the 2.5-second stagger and race: a later successful
peer can cancel pending requests. The fourth is a separate phase, started only
after every endpoint-bound attempt fails. Every non-loopback relay list still
requires signature verification. Azure's two modes use separate connection
pools, preventing a stronger request from reusing a weaker connection.

CloudFront first avoids sending Azure SNI when CloudFront succeeds before the
next stagger tick. Slow or failed CloudFront attempts still permit Azure SNI;
ordinary DNS can also expose endpoint names. This reduces hostname exposure
without promising that discovery hides every hostname.

`DefaultBrokerURL` remains the stable Cloudflare URL. An exact built-in URL
persisted as the primary (including Azure) uses the shared default ordering.
Whitespace is trimmed; other URL spelling differences, including a missing
trailing slash, remain genuine overrides. Custom overrides run alone before
defaults. The engine retains the configured primary for recovery, separately
from the verified winner used for session requests, so a fallback cannot erase
an override on reconnect. Desktop, Android, and iOS use the same engine fetcher;
directory reads delegate to the same brokerapi policy.

All hosts, including desktop and CLI, retarget session telemetry to each
verified discovery winner. This also drains queued events to the new front
after recovery; in-flight uploads keep their captured endpoint and TLS mode.
Normal-SNI Azure winners retain hostname verification for telemetry, including persistent outbox
flushes and heartbeats; recovery to no-SNI explicitly clears the old mode. Before any successful discovery, telemetry retains its configured/bootstrap destination.

## Releases to prepare

- `brokerapi/v0.6.1` (from v0.6.0): discovery ordering, explicit Azure TLS modes,
  isolated connection pools, winning-mode metadata, and regression tests.
- `connectcore/v0.6.2` (from v0.6.1): pins brokerapi v0.6.1, preserves the
  configured primary during recovery, makes telemetry follow the verified winner
  on every host, and embeds broker-front vector version 3.
- No punchcore or wsscore release is required. Root/desktop builds resolve
  in-tree modules through existing local replaces; ship rebuilt desktop/CLI
  application artifacts through their normal version and release process.

Do not tag or publish as part of this PR. **Merging its VERSION changes to
main automatically creates both module tags** via the existing tag workflows.
Keep the PR unmerged until those releases are intended. Before mobile upgrades,
verify both tags exist; connectcore's required brokerapi tag must be fetchable.

## Follow-up in openrung-mobile-app

The separate mobile checkout was inspected for this rollout; this PR does not
modify it or publish application artifacts.

1. Update `android/punchbridge/go.mod` and `go.sum` from brokerapi v0.6.0 to
   v0.6.1 and connectcore v0.6.1 to v0.6.2, using released module versions.
   This binding module feeds both Android and iOS native builds. Do not commit
   local source replacements as release dependencies.
2. Change `testdata/contract/pin.json`'s ref to `connectcore/v0.6.2` and run
   `npm run contract:sync`, then `npm run contract:check`. Re-vendor
   `broker_fronts.json` version 3 byte-for-byte, with regenerated digest and
   version metadata. Update the expected broker-front vector version to 3 in
   the Kotlin, Swift, and Jest contract suites.
3. Review and update `scripts/engine_contract_seams.py`: its current explicit
   v0.6.1 guard must accept the reviewed v0.6.2 test seams. Run the binding's
   engine-vector/recovery checks against the new pinned module.
4. Config-only changes are **not sufficient** to ship discovery ordering.
   `src/config.ts`'s `AppConfig.DEFAULT_BROKER_URL`, Android's
   `android/app/src/main/java/com/openrung/config/AppConfig.kt`, and
   `ios/Shared/AppConfig.swift` currently pass the exact Cloudflare URL with a
   trailing slash. They can retain that value: brokerapi recognizes it as a
   built-in and starts CloudFront without SNI first. Preserve genuine saved
   overrides; no persisted-settings migration is needed. Native arrays also
   serve paths with their own policy; they do not implement discovery racing.
   Use `BrokerCandidates` for discovery rather than constructing a URL-only
   candidate set, which preserves legacy Azure behavior. Keep Azure excluded
   from bearer-ticket paths, even when it wins discovery with normal SNI.
   Bootstrap telemetry and `UPDATE_MANIFEST_URLS` remain separate policies.
   Any custom telemetry/outbox sender must pass through the supplied context
   so it retains the winning Azure TLS mode.
5. Run mobile Go binding tests, Kotlin/Swift/Jest contract and VPN recovery
   suites, `npm run transport:check`, and the repository's version checks.
   Rebuild and validate `android/build-libbox-release.sh`'s AAR and
   `ios/build-libbox-release.sh`'s XCFramework. Test fresh connects, saved
   Cloudflare primaries, custom overrides, blocked CloudFront with Azure normal-SNI
   fallback, the final Azure no-SNI phase, telemetry mode changes, and
   reconnect after a network change on both platforms before app releases.

## Operational notes

Front selection rationale, reachability evidence, and the open origin/client-IP
work are tracked in the private operations notes, not here.
