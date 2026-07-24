# brokerapi

`brokerapi` is OpenRung's shared Go client for end-user connection and session
broker requests. It is a standalone module so the CLI, desktop app, and future
gomobile bindings all use one implementation of:

- opportunistic Encrypted Client Hello for the Cloudflare broker front;
- relay-list signature and freshness verification;
- secure broker URL enforcement;
- identity, application, platform, and no-store request headers;
- redirect refusal;
- relay discovery and front racing;
- telemetry, WSS-ticket, health-probe, and speed-test requests.

The ECH config is compiled into this module. It is never bootstrapped through
DNS. A certificate-authenticated Cloudflare retry config is adopted in memory
after a successful handshake. ECH is opportunistic: if it fails or is dropped,
the transport quickly retries with ordinary TLS and normal hostname and
certificate verification. ECH is attempted only for direct connections to
`broker.openrung.org:443`; CloudFront, custom brokers, loopback development,
and proxy CONNECT paths use the standard TLS behavior.

Relay-list responses from every non-loopback broker must carry a valid
Ed25519 signature from the compiled production key set. Loopback HTTP is the
only cleartext and unsigned development exception. Redirects are never
followed, including from loopback, so neither identity nor that exception can
escape to another origin.

## Use

```go
api := brokerapi.NewClient(nil, brokerapi.Options{
    AppVersion: "1.2.3",
    Platform:   brokerapi.PlatformDesktop,
})

result, err := api.FirstReachable(
    ctx,
    brokerapi.BrokerCandidates(""),
    brokerapi.ListOptions{
        Limit: 5,
        Identity: brokerapi.Identity{
            ClientID:  clientID,
            SessionID: sessionID,
        },
    },
)
```

`result.RelayList.JSON()` returns the exact verified response bytes. The
application can decode those bytes into its own relay model without exposing
server-internal relay fields through this module.

For mobile, bind a small cancellation-owning wrapper into the same Go
AAR/XCFramework as the existing tunnel library. Do not ship a second Go
runtime in a separate Android AAR. A wrapper should return verified relay JSON
and accept telemetry JSON through `SendTelemetryBatchJSON`; JavaScript `fetch`
must not remain on any pre-tunnel broker path that needs ECH.

The separately maintained mobile app also fetches the signed update manifest
from the Cloudflare broker front. That manifest has its own signing keys,
rollback rules, and multi-origin policy and is not implemented by this initial
module extraction. The mobile integration must move that request behind the
same Go ECH transport (or stop using the Cloudflare broker URL) before claiming
complete broker-SNI concealment.

`RequestWSSTicket` owns one hardened POST and response validation. Overall
deadline, broker-front failover, and the single bounded Retry-After round remain
application orchestration policy for now.

Relay registration/heartbeat and the relay data path are intentionally outside
this module.

## Versioning

The module version is in `VERSION` and release tags are namespaced as
`brokerapi/vX.Y.Z`. Consumers outside this repository should pin a released
tag. Repository modules use a local `replace` while developing the coordinated
change.

Changing the embedded ECH config, relay signing pins, public API, or wire
behavior requires a fresh module version. A merge to `main` creates the
corresponding namespaced tag; pull requests must not create tags themselves.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
