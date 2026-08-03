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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	checker := &frontChecker{brokerURL: *brokerURL, host: parsed.Hostname(), limit: *limit}
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

type frontChecker struct {
	brokerURL string
	host      string
	limit     int

	// suppressSNI mirrors what the shipping transport does for this host,
	// established by the first check and read by every later one.
	suppressSNI bool
}

func (c *frontChecker) address() string { return net.JoinHostPort(c.host, "443") }

func (c *frontChecker) run(ctx context.Context) *report {
	results := &report{}

	// 1. The shipping transport, not this tool, decides whether the name is
	// suppressed. Asking it first means every later check exercises the path
	// real clients actually take.
	results.run("transport TLS policy for this host", func() ([]string, error) {
		rule, suppressed := brokerapi.NoSNIFront(c.brokerURL)
		c.suppressSNI = suppressed
		if !suppressed {
			return []string{"ordinary SNI-bearing TLS: certificate must be valid for " + c.host}, nil
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
		list, details, err := c.fetchRelayList(ctx, brokerapi.NewClient(nil, brokerapi.Options{}))
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
			list, details, err := c.fetchRelayList(ctx, brokerapi.NewClient(sniBearingClient(), brokerapi.Options{}))
			if err != nil {
				return details, err
			}
			if len(shippedRelays) == 0 {
				return details, errors.New("the no-SNI fetch produced nothing to compare against")
			}
			if !sameRelayPayload(shippedRelays, list) {
				return details, errors.New(
					"the no-SNI and SNI paths returned different relay lists — they are not reaching the same origin",
				)
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
	nameErr := leaf.VerifyHostname(c.host)
	switch {
	case nameErr == nil:
		details = append(details, fmt.Sprintf("certificate is valid for %s", c.host))
	case c.suppressSNI:
		details = append(details, fmt.Sprintf(
			"NOTE: this certificate is not valid for %s, so TLS does not bind the connection to this endpoint; "+
				"authenticity rests on the Ed25519 relay-list signature checked below", c.host,
		))
	default:
		return details, fmt.Errorf("certificate is not valid for %s: %w", c.host, nameErr)
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
	serverName := c.host
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

func (c *frontChecker) fetchRelayList(ctx context.Context, client *brokerapi.Client) ([]byte, []string, error) {
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
	return payload, []string{
		fmt.Sprintf("%d bytes, signature verified under pinned key %s", len(payload), list.KeyID),
	}, nil
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

	relayPath, err := brokerapi.RelayListURL("https://"+unroutableHost+"/", c.limit)
	if err != nil {
		return nil, err
	}
	parsedPath, err := url.Parse(relayPath)
	if err != nil {
		return nil, err
	}
	request := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: frontcheck\r\nConnection: close\r\n\r\n",
		parsedPath.RequestURI(), unroutableHost,
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
			"the edge served a signed relay list for Host %q — routing is not driven by the endpoint name",
			unroutableHost,
		)
	}
	return []string{fmt.Sprintf("Host %q got HTTP %s and no relay signature", unroutableHost, resp.Status)}, nil
}

// sniBearingClient is an ordinary-TLS client, used only for the control check.
// It deliberately does not go through brokerapi's transport, which would
// suppress the name for exactly the hosts under test.
func sniBearingClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialTLSContext = nil
	return &http.Client{Transport: transport, Timeout: requestTimeout}
}

// sameRelayPayload compares the signed bytes. Relay lists carry a freshness
// bound and can legitimately change between two fetches, so a mismatch is only
// reported when the two paths disagree about the signing key or the relays
// themselves, not when the envelope was merely re-signed.
// A body that does not parse is never equal to anything, including another
// unparseable body: two identical CDN error pages must not read as agreement.
func sameRelayPayload(first, second []byte) bool {
	left, leftOK := relayIdentitySet(first)
	right, rightOK := relayIdentitySet(second)
	return leftOK && rightOK && left == right
}

func relayIdentitySet(payload []byte) (string, bool) {
	var body struct {
		KeyID  string `json:"key_id"`
		Relays []struct {
			ID         string `json:"id"`
			PublicHost string `json:"public_host"`
		} `json:"relays"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", false
	}
	entries := make([]string, 0, len(body.Relays))
	for _, relay := range body.Relays {
		entries = append(entries, relay.ID+"@"+relay.PublicHost)
	}
	slices.Sort(entries)
	return body.KeyID + "|" + strings.Join(entries, ","), true
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
