# Broker origin TLS (end-to-end HTTPS for the CloudFront front)

The broker runs plaintext HTTP on `:8080`. Fronts terminate client TLS at the
edge, but the **edge → origin** leg was HTTP. On the AWS CloudFront front
(distribution `E2PLKW8FO3JZSA`, `d2r7mdpyevvs1m.cloudfront.net`) that leg
carried the high-value **Foundation registration token** (`Authorization`
header on the shipped runtime's `POST /api/v1/volunteers/register` request) in
cleartext across the public internet. This runbook closes that leg with a
TLS-terminating reverse proxy on the broker box so the token is encrypted
**relay → CloudFront edge → origin**.

The legacy `POST /api/v1/volunteers/register` route reaches the same handler and
is retained as a compatibility alias with no scheduled removal date. New
operational checks should use the canonical relay route shown here.

```
relay ──HTTPS──► CloudFront edge ──HTTPS(:443)──► Caddy ──HTTP(127.0.0.1:8080)──► broker
                 (E2PLKW8FO3JZSA)   broker-origin.openrung.org      (container, unchanged)
```

The broker container is **not touched** — the proxy is purely additive, and the
plaintext `:8080` path stays open for volunteer-run relays and for the
Cloudflare Worker front. Set up 2026-07-13.

## What must not be undone

- **`broker-origin.openrung.org` must stay DNS-only (grey cloud) in Cloudflare.**
  It is an `A` record → `54.238.185.205`. Orange-clouding (proxying) it would
  reintroduce Cloudflare's datacenter challenge on the origin and loop the
  Cloudflare Worker's subrequest back into the edge. Both CDN fronts depend on
  this record resolving straight to the broker IP.
- **Keep `:8080` open.** Volunteer-run relays (Hetzner + Lightsail, see
  `deploy/volunteer/hetzner-up.sh`) register directly against
  `http://54.238.185.205:8080`, and the Cloudflare Worker front fetches the
  origin on `:8080`. Do not firewall it off as part of this change.
- **The CloudFront behavior must use `Managed-AllViewerExceptHostHeader`, not
  `Managed-AllViewer`** — see the CloudFront gotcha below. This is the load-bearing
  requirement; get it wrong and the origin handshake fails with a 502.
- Do not disable/mask the `caddy` service; it both serves TLS and auto-renews
  the cert.

## Broker box: Caddy TLS terminator

Host: Lightsail `typhoon-broker`, `ssh -i ~/.ssh/id_ed25519_openrung ubuntu@54.238.185.205`.

Caddy was chosen for native Let's Encrypt auto-renewal (no cron/certbot timer to
manage). ACM certs cannot be installed on Lightsail, and a self-signed cert
would not validate at CloudFront — so a publicly-trusted Let's Encrypt cert is
required.

### Install

```sh
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl gnupg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy   # installs the systemd service, enabled
```

### Config

`/etc/caddy/Caddyfile` (also committed as `deploy/broker/Caddyfile` for
reference):

```caddyfile
{
	# no global options needed — Caddy's default ECDSA (P-256) leaf works with
	# CloudFront's origin connection; do not force RSA.
}

broker-origin.openrung.org {
	reverse_proxy 127.0.0.1:8080 {
		header_up -CF-Connecting-IP
		header_up X-Forwarded-For {remote_host}
	}
	log {
		output file /var/log/caddy/broker-origin.access.log {
			roll_size 20MiB
			roll_keep 5
		}
		format json
	}
}
```

The log directory is provided by a systemd drop-in (the packaged unit sandboxes
the service, so a plain `mkdir` is not enough — systemd must own the path):

`/etc/systemd/system/caddy.service.d/logdir.conf`

```ini
[Service]
LogsDirectory=caddy
LogsDirectoryMode=0750
```

Apply changes as the `caddy` user (never `sudo caddy validate`/`fmt` as root — it
creates a root-owned log file that the service then can't open):

```sh
sudo -u caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
```

The JSON access log redacts sensitive request headers by default — Caddy replaces
`Authorization`, `Cookie`, `Set-Cookie`, and `Proxy-Authorization` with `REDACTED`
— so the Foundation bearer token is never written to
`/var/log/caddy/broker-origin.access.log`.

### Firewall

`:443` was opened on the Lightsail instance (additive; `:80` was already open and
is used for the ACME HTTP-01 challenge):

```sh
aws lightsail open-instance-public-ports --instance-name typhoon-broker \
  --region ap-northeast-1 --port-info fromPort=443,toPort=443,protocol=TCP
```

Open ports afterward: `22, 80, 443, 8080`.

### Cert + renewal

- Cert: `CN=broker-origin.openrung.org`, Caddy-default **ECDSA (P-256)**, Let's
  Encrypt, 90-day. Issued via **HTTP-01** on `:80`. (ECDSA is fine for CloudFront
  — see the note below.)
- Renewal: **automatic and native to Caddy** (ARI-scheduled; renews well before
  expiry as long as the service runs). No timer or cron to maintain. Stored in
  `/var/lib/caddy/.local/share/caddy/`.
- No ACME account email is set (Caddy auto-renews, so expiry notices are
  unnecessary, and it avoids attaching an operator identity to issuance).

## CloudFront front: `E2PLKW8FO3JZSA`

Two config changes (CloudFront/ACM are global — use `--region us-east-1`; the
CLI profile's default region is a broken AZ value so pass it explicitly):

1. **Origin protocol `http-only` → `https-only`** (`HTTPSPort` was already 443,
   `OriginSslProtocols=[TLSv1.2]`). This is the change that encrypts the leg.
2. **Origin request policy `Managed-AllViewer` → `Managed-AllViewerExceptHostHeader`**
   (`216adef6-…` → `b689b0a8-53d0-40ab-baf2-68738e2966ac`). **Required** — see the
   gotcha. This policy still forwards everything except `Host` (all other headers
   incl. `Authorization`, all cookies, all query strings), so the Foundation
   token still reaches the origin.

Both are applied with `get-distribution-config` → edit `DistributionConfig` →
`update-distribution --if-match <ETag>`, then `aws cloudfront wait
distribution-deployed`.

### The CloudFront gotcha (the 502 root cause): SNI, not the Host header

With `Managed-AllViewer`, CloudFront forwards the viewer `Host` header **and uses
it as the origin SNI** — so with a client hitting the distribution it sent
`SNI: d2r7mdpyevvs1m.cloudfront.net`. Caddy has no cert for that name and rejected
the handshake (Caddy debug log: `no certificate available for
'd2r7mdpyevvs1m.cloudfront.net'`) → 502. `Managed-AllViewerExceptHostHeader` makes
CloudFront send the **origin** hostname (`broker-origin.openrung.org`) as both
`Host` and SNI, which Caddy serves and routes. CloudFront still validates the
returned cert against the *origin domain name*, which matches. This SNI mismatch
was the sole cause of the 502.

> **Note — the cert key type (ECDSA) is not the problem.** During debugging an
> RSA cert (`key_type rsa2048`) was tried and initially looked like the fix; it
> was a red herring. CloudFront's captured origin `ClientHello` offers
> `ECDHE-ECDSA-*` cipher suites, the P-256 curve, `ecdsa_secp256r1_sha256`, and
> TLS 1.3 — i.e. it fully supports an ECDSA (P-256) leaf, which AWS documents for
> origin connections. This was verified end-to-end: with the default **ECDSA**
> cert and `AllViewerExceptHostHeader` in place, CloudFront returns `200`. Caddy's
> default key type is used; do **not** force RSA.

## Verify

```sh
# Origin TLS directly (publicly-trusted cert, signed relay list):
curl -v https://broker-origin.openrung.org/api/v1/relays        # 200, cert verifies

# Discovery end-to-end through CloudFront (works from a datacenter IP; the
# Cloudflare Worker front 403s datacenter IPs by design):
curl https://d2r7mdpyevvs1m.cloudfront.net/api/v1/relays?limit=1 # 200, X-OpenRung-Relays-Signature present

# Volunteer-class relay plaintext path still intact:
curl http://54.238.185.205:8080/api/v1/relays                   # 200

# Confirm CloudFront connects over TLS (SNI = origin, not the CF domain):
sudo tail /var/log/caddy/broker-origin.access.log   # request.tls.server_name = broker-origin.openrung.org
```

### The load-bearing test: does the Foundation token survive the origin leg?

This is the whole point of the change, and it needs a **discriminating** test.
`POST {}` → `400 public_host is required` does **not** prove it: prod permits
anonymous registration (`OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true`), so an
*unauthenticated* request gets the same 400 even if `Authorization` were stripped.

Send a `node_class: foundation` registration (which *requires* the Foundation
token) with `public_host` **omitted**, so no relay is ever created — only the auth
decision differs.

**Handle the token safely.** Never put it in a `curl -H "…$FT"` argument or a
plain assignment: shell history can retain the assignment, and curl
[warns](https://curl.se/docs/manpage.html#-H) that command-line arguments (the
expanded header) are not reliably hidden from `ps`/other users. Keep it out of
both history and argv — write it into a **`mktemp`** file (root-owned, mode
`0600`, **unpredictable** name, so there is no fixed path in a shared dir like
`/dev/shm` for another user to pre-create or symlink; note `umask` cannot repair
an *existing* file) and feed that to curl with `-H @file`, under a `trap` that
removes it **however the shell exits** (success, error, or signal). On the broker
box (the token lives in the root-owned env file), one root shell:

```sh
sudo bash -c '
  set -eu; umask 077
  hdr=$(mktemp)                                              # root-owned, 0600, unpredictable
  trap "shred -u \"$hdr\" 2>/dev/null || rm -f \"$hdr\"" EXIT   # always removed
  # sed strips ONLY the first NAME= prefix, so a token containing = (base64
  # padding) survives verbatim; awk -F= "{print \$2}" would truncate it and 403.
  sed -n "s|^OPENRUNG_FOUNDATION_TOKEN=|Authorization: Bearer |p" /etc/openrung/broker.env > "$hdr"
  CF=https://d2r7mdpyevvs1m.cloudfront.net/api/v1/relays/register
  # WITH token   -> 400 public_host is required  => token SURVIVED CloudFront->origin
  curl -sS -H @"$hdr" -o /dev/null -w "with-token: %{http_code}\n" \
    -X POST -H "Content-Type: application/json" -d "{\"node_class\":\"foundation\"}" "$CF"
  # WITHOUT token -> 403 requires the foundation registration token  => header loss shows here
  curl -sS          -o /dev/null -w "no-token:   %{http_code}\n" \
    -X POST -H "Content-Type: application/json" -d "{\"node_class\":\"foundation\"}" "$CF"
'
```

From a host that isn't the broker, hold the token in a **hidden prompt** instead
(`read`/`printf` are shell builtins — no process argv). Force `bash` so the trap
fires consistently, and keep it **portable** — macOS has no `/dev/shm` and no GNU
`shred`, so this uses `mktemp` + `rm`:

```sh
bash -c '
  set -eu; umask 077
  hdr=$(mktemp); trap "rm -f \"$hdr\"" EXIT INT TERM
  read -rs -p "Foundation token: " FT </dev/tty; echo
  printf "Authorization: Bearer %s\n" "$FT" > "$hdr"; unset FT
  CF=https://d2r7mdpyevvs1m.cloudfront.net/api/v1/relays/register
  curl -sS -H @"$hdr" -o /dev/null -w "with-token: %{http_code}\n" \
    -X POST -H "Content-Type: application/json" -d "{\"node_class\":\"foundation\"}" "$CF"
  curl -sS          -o /dev/null -w "no-token:   %{http_code}\n" \
    -X POST -H "Content-Type: application/json" -d "{\"node_class\":\"foundation\"}" "$CF"
'
```

`400` vs `403` is the signal: if `AllViewerExceptHostHeader` (or the origin leg)
dropped `Authorization`, the first call would return `403` like the second.
Verified 2026-07-13: `400` with the token, `403` without. As a no-secret
cross-check, the Caddy access log records `request.headers.Authorization =
["REDACTED"]` on a CloudFront-fronted request that carried one (Caddy redacts the
value, so the token is never written to disk).

## Rollback

The proxy is additive and reversible. To revert the CloudFront front to the
previous (verified-working) plaintext-origin state, restore the original
`DistributionConfig` (`OriginProtocolPolicy=http-only` +
`OriginRequestPolicyId=Managed-AllViewer`):

```sh
aws cloudfront get-distribution-config --id E2PLKW8FO3JZSA --region us-east-1   # note ETag
# set OriginProtocolPolicy=http-only and OriginRequestPolicyId=216adef6-5c7f-47e4-b989-5492eafa07d3
aws cloudfront update-distribution --id E2PLKW8FO3JZSA --region us-east-1 \
  --distribution-config file://rollback.json --if-match <ETag>
```

Caddy itself can be stopped without affecting the broker or `:8080`
(`sudo systemctl stop caddy`) — only the origin `:443` and the CloudFront front
depend on it.

## Follow-ups

- **Loopback-wide rate-limit / telemetry collapse — RESOLVED 2026-07-13; now keyed per-CloudFront-edge.**
  The broker trusts only Cloudflare ranges for forwarded client IPs
  (`internal/broker/clientip.go`) and did not trust the new loopback hop, so it
  recorded `127.0.0.1` as the client for *every* CloudFront-fronted request — the
  whole front collapsed onto one relay-list rate-limit bucket (2 req/s, burst 30)
  and one telemetry client IP. Fixed by adding
  `OPENRUNG_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128` to `/etc/openrung/broker.env`
  (durable) and recreating the broker container; the broker now keys on the
  unspoofable CloudFront **edge** IP that Caddy forwards as the sole
  `X-Forwarded-For` value (client-supplied `CF-Connecting-IP`/`XFF` are stripped).
  No Caddy change was needed. This restores the **pre-proxy** behavior exactly.

- **Per-*viewer* rate-limit / telemetry on the CloudFront path — OPEN (achievable, not yet done).**
  Per-edge keying (above) is a current *implementation* choice, **not** a CloudFront
  limitation: clients sharing one CloudFront edge still share that edge's
  2 req/s / burst-30 bucket and telemetry identity, which can still saturate under
  a **mass-failover** surge (many clients in a region funnelling through a few
  nearby edges). CloudFront *does* expose an attested per-viewer signal that
  `AllViewerExceptHostHeader` already forwards to the origin: **`CloudFront-Viewer-Address`**
  (`<viewer-ip>:<port>`, plus the `CloudFront-Viewer-*` geo/ASN suite). Verified
  2026-07-13 that it is **unspoofable through CloudFront** — a request sent through
  the distribution with a forged `CloudFront-Viewer-Address: 203.0.113.99:4444`
  arrived at the origin as the real viewer IP (`159.117.71.211:59399`); CloudFront
  overwrites any client-supplied value. To adopt per-viewer keying, map the IP part
  of `CloudFront-Viewer-Address` into the client IP the broker keys on (e.g. Caddy
  rewrites `X-Forwarded-For` from it for this vhost). **Caveat that must be handled
  first:** the origin `:443` is internet-facing, so a *direct* hit (not via
  CloudFront) could forge `CloudFront-Viewer-Address`. Trust it only after
  authenticating the request actually came through CloudFront — a shared-secret
  custom origin header CloudFront injects, or restricting `:443` ingress to
  CloudFront's origin-facing IP ranges — otherwise per-viewer keying would be
  *more* spoofable than the current edge-IP key, not less.
- **Cloudflare Worker front origin leg is still plaintext.**
  `broker.openrung.org` (the Worker) still fetches `http://broker-origin…:8080`.
  Now that `:443` origin TLS exists, the Worker could be pointed at
  `https://broker-origin.openrung.org` to close its leg too — a separate change
  to the higher-traffic primary front, deliberately out of scope here.
