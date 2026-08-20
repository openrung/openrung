# brokerapi

`brokerapi` is OpenRung's shared Go client for end-user connection and session
broker requests. It is a standalone module so the CLI, desktop app, and Android
and iOS gomobile bindings all use one implementation of:

- opportunistic Encrypted Client Hello for the Cloudflare broker front;
- verified SNI-less TLS for the CloudFront broker front;
- provider-bound SNI-less TLS for the Azure Front Door discovery fallback;
- relay-list signature and freshness verification;
- secure broker URL enforcement;
- identity, application, platform, and no-store request headers;
- redirect refusal;
- relay discovery and front racing;
- telemetry, WSS-ticket, health-probe, and speed-test requests;
- the exported client-facing relay schema (`relay_schema.go`): the decoded
  descriptor and list-response models, shared wire constants, and the
  read-side node-class rule every client decodes the verified bytes with.

The ECH config is compiled into this module. It is never bootstrapped through
DNS. A certificate-authenticated Cloudflare retry config is adopted in memory
after a successful handshake. ECH is opportunistic: if it fails or is dropped,
the transport quickly retries with ordinary TLS and normal hostname and
certificate verification. ECH is attempted only for direct connections to
`broker.openrung.org:443`; custom brokers, loopback development, and proxy
CONNECT paths use the standard TLS behavior.

CloudFront cannot receive this deployment's ECH config, so its front conceals
the hostname a different way: a direct connection to a native
`*.cloudfront.net` distribution on port 443 sends no ClientHello server name at
all, and CloudFront selects the distribution from the encrypted HTTP `Host`
header. The certificate is still verified in full — chain, validity, server
authentication, and the exact distribution hostname — against the same trust
roots and clock the transport uses everywhere else; only the place the hostname
is asserted changes. An ECH config list is never combined with a suppressed
server name, and there is no plain-SNI fallback: a retry would hand an on-path
censor the exact name this hides, so the front fails closed and discovery
races the independent Cloudflare front instead. Custom CNAME fronts keep
ordinary SNI, because the default certificate CloudFront serves without it
cannot be verified against a CNAME.

Two consequences are worth knowing. CloudFront returns no ALPN when the
ClientHello omits SNI, so that leg negotiates HTTP/1.1 rather than HTTP/2;
connection pooling and keep-alive are unaffected. And `Response.TLS` on that
leg reports an empty `ServerName` and no `VerifiedChains`, because the
verification runs in the transport's own hook rather than in crypto/tls.

In FIPS 140-3 mode (`GODEBUG=fips140=on`) this front is refused rather than
dialed, before any connection is opened. crypto/tls filters verified chains
through a FIPS policy with no exported equivalent, so the replacement hook
cannot honor it, and running under a weaker certificate policy than the rest of
the process is not a trade this module makes. Discovery still reaches the
Cloudflare front.

Native Azure Front Door endpoints also suppress SNI, but Azure's shared
no-SNI certificate cannot authenticate the exact `*.azurefd.net` endpoint. The
transport pins the shared edge SAN and a public trust chain, which proves an
Azure edge rather than this deployment. `FirstReachable` therefore races all
endpoint-bound candidates first and starts endpoint-unbound candidates only
after every stronger candidate has failed. The relay directory remains
authentic because its signed envelope is verified independently of TLS.

That signature does not make every broker response safe over a provider-bound
connection. In particular, a WSS session ticket is a short-lived bearer that an
impersonating front could forward from the real broker and redeem first.
`RequestWSSTicket` refuses endpoint-unbound fronts before issuing any HTTP
request; application-level ticket failover should filter them as well.

This removes the cleartext TLS signal from direct connections, not every
hostname signal. DNS resolution of the distribution name still carries it. So
does any configured proxy, and more completely than the `CONNECT` line alone
suggests: `net/http` performs the tunnelled handshake itself rather than
through this transport, so a proxied broker request sends an ordinary
SNI-bearing ClientHello. Certificate verification is unaffected there; only the
concealment is.

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

The mobile repository binds cancellation-owning wrappers into the same Go
AAR/XCFramework as its tunnel library, avoiding a second Go runtime. Native
directory discovery returns verified relay JSON, telemetry passes through
`SendTelemetryBatchJSON`, and WSS ticket operations remain inside the native
VPN owners rather than crossing the React Native bridge.

The separately maintained mobile app also routes its exact Cloudflare and
CloudFront update-manifest candidates through this transport. The manifest's
signature, freshness, rollback, cache, and multi-origin selection policy remain
in the application layer, with an exact redirecting GitHub release URL as its
narrow JavaScript fallback. Broker-SNI concealment still holds only where ECH
survives on Cloudflare; its ordinary-TLS fallback sends the hostname, while the
native CloudFront front is unconditionally SNI-less.

`RequestWSSTicket` owns one hardened POST and response validation and refuses
endpoint-unbound fronts. Overall deadline, broker-front failover, and the single
bounded Retry-After round remain application orchestration policy for now.

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
