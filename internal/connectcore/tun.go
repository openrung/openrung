package connectcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"openrung/internal/client"
)

// TUN-mode replacements for the proxy-mode readiness and probe helpers
// (docs/adr/001 PR B3). Proxy mode dials the loopback mixed inbound to learn
// that sing-box came up and probes the internet through it; a TUN inbound
// binds no port, so readiness asks the kernel whether the tunnel now owns the
// default path and the probes ride that path.

// tunnelAddress{IPv4,IPv6} are the addresses BuildSingBoxConfig gives the TUN
// inbound. The engine never overrides them, so they are resolved once.
var (
	tunnelAddressIPv4 = tunnelAddressIP(client.DefaultTunnelIPv4Address)
	tunnelAddressIPv6 = tunnelAddressIP(client.DefaultTunnelIPv6Address)
)

func tunnelAddressIP(prefixed string) net.IP {
	ip, _, err := net.ParseCIDR(prefixed)
	if err != nil {
		return nil
	}
	return ip
}

// tunRouteProbeTargets are route-lookup destinations, one per address family.
// Nothing is ever sent to them: connecting a UDP socket performs the route
// lookup and binds the source the kernel chose, without a packet on the wire.
// Two families are tried because a v6-only host has no v4 route to answer with
// (and vice versa).
var tunRouteProbeTargets = []struct{ network, address string }{
	{"udp4", "1.1.1.1:53"},
	{"udp6", "[2606:4700:4700::1111]:53"},
}

// routeSourceIP is a package var so tests can present a fixed routing answer.
var routeSourceIP = kernelRouteSourceIP

// kernelRouteSourceIP reports which local address the kernel would send from
// to reach dst.
func kernelRouteSourceIP(network, dst string) (net.IP, error) {
	conn, err := net.Dial(network, dst)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New("route lookup returned no UDP address")
	}
	return addr.IP, nil
}

// tunInterfaceReady reports the TUN inbound up once the kernel would send
// internet-bound traffic from the tunnel's own address — that is, once
// sing-box has both created the device and installed the routes that capture
// the default path.
//
// Finding the tunnel address on some interface is deliberately NOT the test.
// 172.19.0.1 sits inside the range Docker carves its bridge networks from, so
// a host with a matching bridge would report ready before this sing-box had
// done anything at all, and the direct internet probe that follows would then
// succeed over the ordinary network and mark an untunneled session CONNECTED.
// Asking the kernel which source it picks answers "do the routes belong to our
// tunnel", which is the property readiness actually needs, and it closes the
// same window for a device that exists but has no routes yet.
//
// It takes the loopback port the proxy-mode probe uses so both satisfy the
// same engine seam; TUN mode has no such port and ignores it.
func tunInterfaceReady(ctx context.Context, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var lastErr error
	for _, target := range tunRouteProbeTargets {
		source, err := routeSourceIP(target.network, target.address)
		if err != nil {
			// No route in this family (a v4-only or v6-only host); the other
			// family still decides.
			lastErr = err
			continue
		}
		if source.Equal(tunnelAddressIPv4) || source.Equal(tunnelAddressIPv6) {
			return nil
		}
		lastErr = fmt.Errorf("internet traffic still leaves from %s, not the tunnel", source)
	}
	if lastErr == nil {
		lastErr = errors.New("no route available to test the tunnel against")
	}
	return lastErr
}

// directProbeClient builds the probe client for TUN mode. It sets no proxy —
// the TUN has taken over the default route, so an ordinary request already
// traverses the tunnel — and deliberately does not fall back to
// http.ProxyFromEnvironment: an inherited proxy variable would send the probe
// somewhere other than the tunnel being verified.
func directProbeClient() *http.Client {
	return &http.Client{
		Timeout: InternetProbeRequestTimeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// verifyInternetViaTUN gates CONNECTED on end-to-end internet through the TUN,
// with the same sweep-and-retry policy the proxy-mode probe uses.
func verifyInternetViaTUN(ctx context.Context, _ int) (int64, error) {
	client := directProbeClient()
	defer closeIdle(client)
	return verifyInternet(ctx, client)
}

// healthSweepViaTUN is one mid-session health sweep through the TUN.
func healthSweepViaTUN(ctx context.Context, _ int) error {
	client := directProbeClient()
	defer closeIdle(client)
	return probeSweep(ctx, client)
}
