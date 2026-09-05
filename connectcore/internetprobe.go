package connectcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// proxyProbeClient builds an HTTP client routed through the sing-box mixed
// inbound on 127.0.0.1:proxyPort, so a probe proves the relay end to end (the
// endpoints are HTTPS, which the mixed inbound carries via CONNECT). Redirects
// are not followed and keep-alives are disabled so probe sockets never outlive
// the candidate that served them; callers must closeIdle when done.
func proxyProbeClient(proxyPort int) *http.Client {
	return &http.Client{
		Timeout: InternetProbeRequestTimeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", proxyPort)}),
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func closeIdle(client *http.Client) {
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// probeSweep tries each probe endpoint once; any 2xx answer proves internet
// access. Mirrors one iteration of the mobile InternetProbe endpoint loop.
func probeSweep(ctx context.Context, client *http.Client) error {
	var lastErr error
	for _, endpoint := range InternetProbeURLs {
		if err := probeOnce(ctx, client, endpoint); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("no probe endpoints configured")
	}
	return lastErr
}

func probeOnce(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return describeProbeError(req, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("internet probe HTTP %d from %s", resp.StatusCode, endpoint)
	}
	// Pull a byte of body like the mobile probe, so the response demonstrably
	// traversed the tunnel rather than being a header-only proxy artifact.
	buf := make([]byte, 1)
	_, _ = resp.Body.Read(buf)
	return nil
}

// describeProbeError names a failed name lookup for what it is. Only the TUN
// probe resolves locally (the proxy probe hands the hostname to the mixed
// inbound), and there a lookup failure means the OS's DNS did not make it
// through the tunnel — a DNS problem, not an unreachable relay. Without the
// distinction the user reads "internet probe failed" and blames the relay
// (issue #175). Every other error is returned unchanged.
func describeProbeError(req *http.Request, err error) error {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return err
	}
	return fmt.Errorf("DNS lookup of %s failed through the VPN: %w", req.URL.Hostname(), err)
}

// verifyInternetViaProxy gates CONNECTED on end-to-end internet through the
// tunnel's loopback mixed inbound.
func verifyInternetViaProxy(ctx context.Context, proxyPort int) (int64, error) {
	client := proxyProbeClient(proxyPort)
	defer closeIdle(client)
	return verifyInternet(ctx, client)
}

// verifyInternet mirrors the mobile InternetProbe.verify: sweep the endpoints
// until one answers or the overall deadline expires, pausing between sweeps.
// The returned duration includes the retries (internet_probe_ms semantics).
// Only the client differs between capture modes — through the loopback proxy
// inbound, or straight out over the route the TUN has taken over.
func verifyInternet(ctx context.Context, client *http.Client) (int64, error) {
	started := time.Now()
	deadline := started.Add(InternetProbeOverallTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := probeSweep(ctx, client); err != nil {
			lastErr = err
		} else {
			return time.Since(started).Milliseconds(), nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(InternetProbeRetryDelay):
		}
	}
	return 0, fmt.Errorf("VPN started, but the internet probe failed: %w", lastErr)
}

// healthSweepViaProxy is one mid-session health-monitor sweep through the
// tunnel's loopback mixed inbound — a single pass over the endpoints, no retry
// loop; the monitor's consecutive-failure counter is the retry policy.
func healthSweepViaProxy(ctx context.Context, proxyPort int) error {
	client := proxyProbeClient(proxyPort)
	defer closeIdle(client)
	return probeSweep(ctx, client)
}
