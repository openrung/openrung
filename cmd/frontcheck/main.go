// SPDX-License-Identifier: GPL-3.0-or-later

// Command frontcheck runs the read-only acceptance checks a broker front must
// pass before it is added to the built-in discovery order.
//
// It exists because the SNI-less behaviour both no-SNI fronts depend on is
// undocumented and carries no SLA, and — on Azure — is a property of the edge
// fleet an endpoint happens to land on rather than of the product. A new
// endpoint therefore has to be measured, not assumed. Every check is a plain
// GET; nothing here changes any state.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

const (
	// Long enough for a cold edge on a slow path, short enough that a
	// blackholed front fails the run instead of hanging it.
	dialTimeout    = 15 * time.Second
	requestTimeout = 30 * time.Second

	// A Host no endpoint can be configured for, used to prove the edge routes
	// on the Host header rather than serving our origin to anything that
	// reaches the same address.
	unroutableHost = "frontcheck-unroutable.invalid"

	// maxRelayListAge bounds how far a served list's server_time may sit behind
	// this machine's clock. A front that caches the relay list serves a body
	// that still verifies — the signature covers a 30-minute not_after window,
	// and brokerapi allows 5 minutes of skew on top — so nothing else in this
	// tool would notice. A Cloudflare edge once served this deployment a stale
	// /api/v1/relays for about four hours, which is why the route in front of
	// the origin must have caching disabled and why this is checked directly
	// rather than inferred from comparing two fetches.
	maxRelayListAge = 5 * time.Minute

	// maxRelayListLead bounds the opposite direction: how far server_time may
	// sit AHEAD of this machine's clock before the staleness test above stops
	// meaning anything. See checkRelayListFreshness.
	maxRelayListLead = 5 * time.Minute
)

type checkResult struct {
	name    string
	err     error
	skipped bool
	details []string
}

type report struct {
	results []checkResult
}

func (r *report) run(name string, check func() ([]string, error)) {
	details, err := check()
	r.results = append(r.results, checkResult{name: name, err: err, details: details})
}

// skip records a check that does not apply to this kind of front, so the report
// says so rather than silently omitting it.
func (r *report) skip(name, reason string) {
	r.results = append(r.results, checkResult{name: name, skipped: true, details: []string{reason}})
}

func (r *report) failed() bool {
	for _, result := range r.results {
		if result.err != nil {
			return true
		}
	}
	return false
}

func (r *report) print(out *os.File) {
	for _, result := range r.results {
		status := "PASS"
		switch {
		case result.err != nil:
			status = "FAIL"
		case result.skipped:
			status = "SKIP"
		}
		fmt.Fprintf(out, "%s  %s\n", status, result.name)
		for _, detail := range result.details {
			fmt.Fprintf(out, "        %s\n", detail)
		}
		if result.err != nil {
			fmt.Fprintf(out, "        error: %s\n", oneLine(result.err.Error()))
		}
	}
}

func main() {
	brokerURL := flag.String("url", "", "candidate broker front URL, e.g. https://openrung-broker-xxxx.z01.azurefd.net/")
	limit := flag.Int("limit", brokerapi.DefaultRelayLimit, "relay-list limit to request")
	flag.Parse()

	if strings.TrimSpace(*brokerURL) == "" {
		fmt.Fprintln(os.Stderr, "frontcheck: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	parsed, err := brokerapi.EnforceSecureBrokerURL(*brokerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frontcheck: %v\n", err)
		os.Exit(2)
	}
	// EnforceSecureBrokerURL also admits plain http to loopback, which is valid
	// for development but has no TLS for any check here to describe.
	if !strings.EqualFold(parsed.Scheme, "https") {
		fmt.Fprintf(os.Stderr, "frontcheck: %s is not https; a broker front is reached over TLS\n", *brokerURL)
		os.Exit(2)
	}
	// A precondition rather than a check, because a proxy invalidates the whole
	// run rather than failing one part of it.
	if proxy, proxyErr := candidateProxy(parsed); proxyErr != nil || proxy != nil {
		reportProxyRefusal(parsed, proxy, proxyErr)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	checker := &frontChecker{brokerURL: *brokerURL, endpoint: parsed, limit: *limit}
	results := checker.run(ctx)
	results.print(os.Stdout)

	if results.failed() {
		fmt.Fprintf(os.Stderr, "\nfrontcheck: %s did NOT pass — do not advertise it as a broker front\n", *brokerURL)
		os.Exit(1)
	}
	fmt.Printf("\nfrontcheck: %s passed every applicable check.\n", *brokerURL)
	fmt.Println("Re-run it after any endpoint, CDN, or certificate change. For a front whose")
	fmt.Println("SNI is suppressed, the behaviour above is undocumented, carries no SLA, and")
	fmt.Println("can be withdrawn without notice, so a passing run describes today only.")
}

// candidateProxy reports the proxy net/http would select for this candidate, if
// any. It asks the same function the shipping transport is configured with
// rather than reading the environment directly, so NO_PROXY exemptions and the
// CGI special case are honoured exactly as they will be at request time.
func candidateProxy(endpoint *url.URL) (*url.URL, error) {
	return http.ProxyFromEnvironment(&http.Request{
		Method: http.MethodGet,
		URL:    endpoint,
		Header: make(http.Header),
	})
}

// reportProxyRefusal explains why a proxied environment cannot produce a
// meaningful result. This is not conservatism: net/http tunnels an HTTPS
// request through CONNECT and then performs its own handshake, so
// DialTLSContext — the only place SNI is suppressed — never runs. Every fetch
// in this tool would then travel an SNI-bearing path while the report claimed
// to describe a suppressed one, which is precisely the false pass the gate
// exists to prevent.
func reportProxyRefusal(endpoint, proxy *url.URL, err error) {
	fmt.Fprintf(os.Stderr, "frontcheck: refusing to run — a proxy is configured for %s\n", endpoint.Host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  proxy selection failed: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  net/http would send this request through %s\n", proxyLocation(proxy))
	}
	fmt.Fprintln(os.Stderr, "  Go tunnels proxied HTTPS with CONNECT and then runs its own SNI-bearing")
	fmt.Fprintln(os.Stderr, "  handshake, so the transport's no-SNI dialer never runs and no check here")
	fmt.Fprintln(os.Stderr, "  would describe how clients actually reach this front.")
	// net/http reads only HTTP(S)_PROXY and NO_PROXY — naming ALL_PROXY here
	// would send an operator to unset a variable it never consulted.
	fmt.Fprintln(os.Stderr, "  Unset HTTPS_PROXY (or add the host to NO_PROXY) and re-run.")
}

// proxyLocation names the proxy without any part of its credentials. A proxy
// URL routinely carries userinfo, and this message lands in terminal scrollback
// and CI logs; the scheme and authority are all an operator needs to recognize
// which configuration is in play.
//
// url.URL.Redacted is deliberately not used: it masks only the password, and a
// token-in-the-username proxy URL is a common enough shape that its output
// would still be a credential.
func proxyLocation(proxy *url.URL) string {
	if proxy == nil {
		return "an unnamed proxy"
	}
	location := proxy.Host
	if location == "" {
		location = proxy.Opaque
	}
	if proxy.Scheme != "" {
		location = proxy.Scheme + "://" + location
	}
	if strings.TrimSpace(location) == "" {
		return "an unnamed proxy"
	}
	return location
}

type frontChecker struct {
	brokerURL string
	// endpoint is the validated candidate with its port and base path intact,
	// so the handshake and the routing probe test exactly what the signed fetch
	// tests rather than a synthesized root URL on :443.
	endpoint *url.URL
	limit    int

	// suppressSNI mirrors what the shipping transport does for this host,
	// established by the first check and read by every later one.
	suppressSNI bool
}

func (c *frontChecker) host() string { return c.endpoint.Hostname() }

// address is what the shipping transport dials. Defaulting to 443 is only sound
// because main() has already refused anything but https; net/http would default
// an http candidate to 80, and this would then measure a different endpoint than
// the one the signed fetch used.
func (c *frontChecker) address() string {
	port := c.endpoint.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(c.endpoint.Hostname(), port)
}

func (c *frontChecker) run(ctx context.Context) *report {
	results := &report{}

	// 1. The shipping transport, not this tool, decides whether the name is
	// suppressed. Asking it first means every later check exercises the path
	// real clients actually take.
	results.run("transport TLS policy for this host", func() ([]string, error) {
		rule, suppressed := brokerapi.NoSNIFront(c.brokerURL)
		c.suppressSNI = suppressed
		if !suppressed {
			return []string{"ordinary SNI-bearing TLS: certificate must be valid for " + c.host()}, nil
		}
		return []string{"SNI suppressed — " + rule}, nil
	})

	// 2. The handshake itself, with the certificate reported in full so a
	// reviewer can see what was actually served rather than a verdict alone.
	results.run("handshake completes and satisfies that policy", func() ([]string, error) {
		return c.describeHandshake(ctx)
	})

	// 3. The real client on the real path: a signed relay list has to arrive
	// and verify under the pinned Ed25519 keys. On a no-SNI front this is what
	// makes the weaker TLS binding survivable, so it is the check that matters
	// most.
	var shippedRelays []byte
	results.run("signed relay list fetches and verifies over the shipping path", func() ([]string, error) {
		shippingClient, observed := observedShippingClient()
		list, details, err := c.fetchRelayList(ctx, brokerapi.NewClient(shippingClient, brokerapi.Options{}), observed)
		shippedRelays = list
		return details, err
	})

	// 4. Control: the same endpoint dialed with ordinary SNI must serve the
	// same list. Without this, a no-SNI path silently reaching some other
	// backend would look identical to success. It says nothing about a front
	// that already uses ordinary TLS — that would just repeat check 3.
	const controlCheck = "SNI-bearing control serves the same signed list"
	if !c.suppressSNI {
		results.skip(controlCheck, "front already uses SNI-bearing TLS; check 3 is the same path")
	} else {
		results.run(controlCheck, func() ([]string, error) {
			list, details, err := c.fetchRelayList(ctx, brokerapi.NewClient(sniBearingClient(), brokerapi.Options{}), nil)
			if err != nil {
				return details, err
			}
			if len(shippedRelays) == 0 {
				return details, errors.New("the no-SNI fetch produced nothing to compare against")
			}
			if !sameRelayPayload(shippedRelays, list) {
				return details, errors.New(describeRelayDifference(shippedRelays, list))
			}
			return append(details, "matches the no-SNI response"), nil
		})
	}

	// 5. Routing is genuinely driven by the Host header. If the edge served our
	// origin to any Host, the endpoint name would not be what selects it and
	// the front would be reachable in ways we have not reasoned about.
	results.run("edge routes on the Host header, not the address", func() ([]string, error) {
		return c.probeUnroutableHost(ctx)
	})

	return results
}

// describeHandshake dials exactly as the client does and reports the
// certificate, so an operator can record what the edge served on the day the
// front was accepted.
func (c *frontChecker) describeHandshake(ctx context.Context) ([]string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	state := conn.ConnectionState()
	if c.suppressSNI && state.ServerName != "" {
		return nil, fmt.Errorf("ClientHello carried server name %q", state.ServerName)
	}
	leaf := state.PeerCertificates[0]
	details := []string{
		fmt.Sprintf("TLS %s, ALPN %q", tlsVersionName(state.Version), state.NegotiatedProtocol),
		fmt.Sprintf("leaf subject %q, issuer %q", leaf.Subject.CommonName, leaf.Issuer.CommonName),
		fmt.Sprintf("leaf SANs %v", leaf.DNSNames),
		fmt.Sprintf("chain depth %d, leaf expires %s", len(state.PeerCertificates), leaf.NotAfter.Format(time.RFC3339)),
	}
	// Report the endpoint-name gap explicitly rather than letting a passing run
	// imply the connection is bound to this endpoint.
	nameErr := leaf.VerifyHostname(c.host())
	switch {
	case nameErr == nil:
		details = append(details, fmt.Sprintf("certificate is valid for %s", c.host()))
	case c.suppressSNI:
		details = append(details, fmt.Sprintf(
			"NOTE: this certificate is not valid for %s, so TLS does not bind the connection to this endpoint; "+
				"authenticity rests on the Ed25519 relay-list signature checked below", c.host(),
		))
	default:
		return details, fmt.Errorf("certificate is not valid for %s: %w", c.host(), nameErr)
	}
	return details, nil
}

// dial reproduces the client's handshake for this front. Verification is left
// to brokerapi on the fetch checks; here the certificate is inspected and
// reported, which is why the handshake itself does not verify.
func (c *frontChecker) dial(ctx context.Context) (*tls.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", c.address())
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.address(), err)
	}
	serverName := c.host()
	if c.suppressSNI {
		serverName = ""
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // inspected below, and brokerapi enforces the real policy
	})
	handshakeCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if len(conn.ConnectionState().PeerCertificates) == 0 {
		_ = conn.Close()
		return nil, errors.New("server presented no certificate")
	}
	return conn, nil
}

func (c *frontChecker) fetchRelayList(
	ctx context.Context,
	client *brokerapi.Client,
	observed *observedTLS,
) ([]byte, []string, error) {
	defer client.CloseIdleConnections()
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	list, err := client.ListRelays(requestCtx, c.brokerURL, brokerapi.ListOptions{Limit: c.limit})
	if err != nil {
		return nil, nil, err
	}
	if !list.SignatureVerified {
		return nil, nil, errors.New(
			"relay list was accepted without a verified signature — a front must serve signed lists",
		)
	}
	payload := list.JSON()
	details := []string{
		fmt.Sprintf("%d bytes, signature verified under pinned key %s", len(payload), list.KeyID),
	}

	age, err := checkRelayListFreshness(payload, time.Now())
	if err != nil {
		return nil, details, err
	}
	details = append(details, describeRelayListAge(age))

	// Measure, rather than infer, that the fetch took the path the first check
	// reported. Without this, a PASS would rest on the absence of a proxy
	// instead of on the connection that actually carried the signed list.
	if observed != nil {
		state, ok := observed.state()
		switch {
		case !ok:
			return nil, details, errors.New("the request completed without a TLS connection to inspect")
		case c.suppressSNI && state.ServerName != "":
			return nil, details, fmt.Errorf(
				"the signed list arrived over a connection whose ClientHello carried server name %q — "+
					"this fetch did not take the no-SNI path",
				state.ServerName,
			)
		case !c.suppressSNI && !strings.EqualFold(state.ServerName, c.host()):
			return nil, details, fmt.Errorf(
				"the signed list arrived with ClientHello server name %q, want %q",
				state.ServerName, c.host(),
			)
		}
		details = append(details, fmt.Sprintf("carried over TLS with ClientHello server name %q", state.ServerName))
	}
	return payload, details, nil
}

// checkRelayListFreshness bounds server_time in BOTH directions and returns the
// age for reporting.
//
// The upper bound is the staleness test. The lower bound is not symmetry for its
// own sake: staleness is measured against this machine's clock, so a clock that
// runs behind hides exactly the failure the upper bound looks for. A clock four
// hours slow makes a four-hour-old cached list read as zero seconds old, and it
// passes signature verification too, because not_after is relative to the same
// stale server_time. A list far enough ahead of local time therefore means
// freshness cannot be established at all, and saying so is the only honest
// outcome.
func checkRelayListFreshness(payload []byte, now time.Time) (time.Duration, error) {
	age, err := relayListAge(payload, now)
	if err != nil {
		return 0, err
	}
	switch {
	case age > maxRelayListAge:
		return age, fmt.Errorf(
			"the served list is %s old (limit %s) — either this front is caching the relay list, "+
				"which its route must not do, or this machine's clock is ahead of the broker's",
			age.Round(time.Second), maxRelayListAge,
		)
	case age < -maxRelayListLead:
		return age, fmt.Errorf(
			"server_time is %s ahead of this machine's clock (limit %s), so this run cannot tell a freshly "+
				"signed list from a cached one — a clock this far behind would make a stale list read as current. "+
				"Correct this machine's clock and re-run",
			(-age).Round(time.Second), maxRelayListLead,
		)
	}
	return age, nil
}

func describeRelayListAge(age time.Duration) string {
	if age < 0 {
		return fmt.Sprintf("server_time is %s ahead of this machine's clock, within tolerance", (-age).Round(time.Second))
	}
	return fmt.Sprintf("server_time is %s old, so it was signed for this request", age.Round(time.Second))
}

// relayListAge reports how far the signed body's server_time sits behind now. A
// clock behind the broker's yields a negative age.
func relayListAge(payload []byte, now time.Time) (time.Duration, error) {
	var body struct {
		ServerTime *time.Time `json:"server_time"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return 0, fmt.Errorf("signed body is not JSON: %w", err)
	}
	if body.ServerTime == nil {
		return 0, errors.New("signed body carries no server_time, so its freshness cannot be established")
	}
	return now.Sub(*body.ServerTime), nil
}

// observedTLS records the TLS state of the connection net/http actually used.
// It wraps the transport rather than replacing it, so the no-SNI dialer
// underneath stays in play — brokerapi.NewClient uses a caller's Transport
// verbatim, so replacing it would silently opt out of the very behaviour under
// test.
type observedTLS struct {
	inner http.RoundTripper
	mu    sync.Mutex
	seen  *tls.ConnectionState
}

func (o *observedTLS) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.inner.RoundTrip(req)
	if err == nil && resp.TLS != nil {
		state := *resp.TLS
		o.mu.Lock()
		o.seen = &state
		o.mu.Unlock()
	}
	return resp, err
}

func (o *observedTLS) state() (tls.ConnectionState, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		return tls.ConnectionState{}, false
	}
	return *o.seen, true
}

// observedShippingClient returns the client real clients use, with its TLS state
// exposed for inspection.
func observedShippingClient() (*http.Client, *observedTLS) {
	client := brokerapi.NewHTTPClient(0)
	observer := &observedTLS{inner: client.Transport}
	client.Transport = observer
	return client, observer
}

// probeUnroutableHost sends a Host no endpoint can claim over a no-SNI
// connection. Anything other than our relay list is a pass: the point is that
// the address alone does not serve this origin.
func (c *frontChecker) probeUnroutableHost(ctx context.Context) ([]string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// The probe has to request the SAME path the signed fetch uses. Against a
	// candidate with a base path, a 404 for some other path would prove nothing
	// about whether the real route honours the Host header.
	probe, err := c.unroutableProbeURL()
	if err != nil {
		return nil, err
	}
	request := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: frontcheck\r\nConnection: close\r\n\r\n",
		probe.RequestURI(), probe.Host,
	)
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	if _, err := conn.Write([]byte(request)); err != nil {
		return nil, fmt.Errorf("write probe request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		// A refused or torn-down request is a perfectly good negative result.
		return []string{fmt.Sprintf("edge refused the request (%v)", err)}, nil
	}
	defer resp.Body.Close()
	if resp.Header.Get(brokerapi.RelaySignatureHeader) != "" {
		return nil, fmt.Errorf(
			"the edge served a signed relay list for Host %q at %s — routing is not driven by the endpoint name",
			probe.Host, probe.RequestURI(),
		)
	}
	return []string{fmt.Sprintf(
		"Host %q at %s got HTTP %s and no relay signature",
		probe.Host, probe.RequestURI(), resp.Status,
	)}, nil
}

// unroutableProbeURL keeps the candidate's port and base path and swaps only the
// host, so the request differs from a real one in exactly the dimension under
// test.
func (c *frontChecker) unroutableProbeURL() (*url.URL, error) {
	base := *c.endpoint
	base.Host = unroutableHost
	if port := c.endpoint.Port(); port != "" {
		base.Host = net.JoinHostPort(unroutableHost, port)
	}
	relayURL, err := brokerapi.RelayListURL(base.String(), c.limit)
	if err != nil {
		return nil, err
	}
	return url.Parse(relayURL)
}

// sniBearingClient is an ordinary-TLS client, used only for the control check.
// It deliberately does not go through brokerapi's transport, which would
// suppress the name for exactly the hosts under test.
func sniBearingClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialTLSContext = nil
	return &http.Client{Transport: transport, Timeout: requestTimeout}
}

// volatileEnvelopeFields are re-stamped on every signing and say nothing about
// which origin answered.
var volatileEnvelopeFields = []string{"server_time", "not_after"}

// volatileRelayFields move on the fleet's own heartbeat clock rather than in
// response to which front was asked, so comparing them measures elapsed time,
// not origin.
//
// Measured 2026-08-03 over 20 fetches two seconds apart: leaving these in
// mismatched 31% of adjacent pairs, which would have made this gate unusable.
// Excluding them left only a genuine relay-set change. registered_at never
// moved in that sample but is excluded too — it changes on re-registration,
// which client_id, reality_public_key, and short_id already catch.
//
// Everything else is compared, including every connection-critical relay field,
// because a front serving a different or stale signed configuration is exactly
// what this exists to catch.
var volatileRelayFields = []string{"last_heartbeat_at", "expires_at", "registered_at"}

// sameRelayPayload reports whether two fetches describe the same relay
// configuration. It compares whole descriptors rather than a chosen subset, so
// a field added to the wire schema later is covered without editing this code.
//
// A body that does not parse is never equal to anything, including another
// unparseable body: two identical CDN error pages must not read as agreement.
//
// A genuine fleet change between the two fetches — a relay entering or leaving
// in the seconds between them — still reports as a mismatch. Measured at about
// one run in twenty, self-correcting on a re-run, and the safe direction for an
// acceptance gate to err in; describeRelayDifference says which case it was.
func sameRelayPayload(first, second []byte) bool {
	left, leftOK := canonicalRelayConfiguration(first)
	right, rightOK := canonicalRelayConfiguration(second)
	return leftOK && rightOK && left == right
}

// describeRelayDifference explains a mismatch, because the two causes call for
// opposite responses: a changed relay set is usually the fleet moving under the
// check and clears on a re-run, while the same relays carrying a different
// configuration means the two paths really did reach different origins.
func describeRelayDifference(first, second []byte) string {
	left, leftOK := relayIdentifiers(first)
	right, rightOK := relayIdentifiers(second)
	if !leftOK || !rightOK {
		return "one path did not return a relay list at all"
	}
	if !slices.Equal(left, right) {
		return fmt.Sprintf(
			"the two fetches returned different relay sets (%v vs %v) — most likely the fleet changed "+
				"between them rather than the fronts disagreeing; re-run to confirm",
			left, right,
		)
	}
	return "the same relays were returned with a different configuration — the two paths are not reaching the same origin"
}

// relayIdentifiers returns the sorted relay ids, used only to characterize a
// mismatch that canonicalRelayConfiguration has already found.
func relayIdentifiers(payload []byte) ([]string, bool) {
	var body struct {
		Relays *[]struct {
			ID string `json:"id"`
		} `json:"relays"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Relays == nil {
		return nil, false
	}
	ids := make([]string, 0, len(*body.Relays))
	for _, relay := range *body.Relays {
		ids = append(ids, relay.ID)
	}
	slices.Sort(ids)
	return ids, true
}

// canonicalRelayConfiguration renders a relay list into a stable string.
// encoding/json sorts map keys, so re-marshalling the decoded body is enough to
// make field order irrelevant at every level.
func canonicalRelayConfiguration(payload []byte) (string, bool) {
	// UseNumber keeps numeric fields as their original literals, so a
	// coordinate or bandwidth figure can never differ through float64
	// round-tripping rather than through the origin.
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return "", false
	}
	relays, ok := body["relays"].([]any)
	if !ok {
		// Not a relay list at all: an error page, or a body with no relays
		// array. Never comparable.
		return "", false
	}
	for _, field := range volatileEnvelopeFields {
		delete(body, field)
	}
	// Relay order is per-request ranking, not identity — broker-side ranking
	// sorts on live metrics and is not order-stable — so the descriptors are
	// compared as a set. A change to WHICH relays are selected per request would
	// need this check revisited, not just re-sorted.
	delete(body, "relays")
	encoded := make([]string, 0, len(relays))
	for _, relay := range relays {
		descriptor, ok := relay.(map[string]any)
		if !ok {
			return "", false
		}
		for _, field := range volatileRelayFields {
			delete(descriptor, field)
		}
		blob, err := json.Marshal(descriptor)
		if err != nil {
			return "", false
		}
		encoded = append(encoded, string(blob))
	}
	slices.Sort(encoded)

	envelope, err := json.Marshal(body)
	if err != nil {
		return "", false
	}
	return string(envelope) + "|" + strings.Join(encoded, "|"), true
}

// oneLine keeps the report readable when a front answers with a CDN error page:
// a rejected fetch carries the response body, which can run to hundreds of
// kilobytes of HTML.
func oneLine(message string) string {
	const limit = 300
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > limit {
		return message[:limit] + "… (truncated)"
	}
	return message
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}
