package connectcore

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"openrung/internal/client"
)

// TUN-mode replacements for the proxy-mode readiness and probe helpers
// (docs/adr/001 PR B3). Proxy mode dials the loopback mixed inbound to learn
// that sing-box came up and probes the internet through it; a TUN inbound
// binds no port, so readiness is the tunnel device carrying its configured
// address and the probes ride the default route the TUN has taken over.

// interfaceAddrs is a package var so the readiness test can present a fixed
// address set instead of the host's real interfaces.
var interfaceAddrs = net.InterfaceAddrs

// tunInterfaceReady reports the TUN inbound up once one of the tunnel
// addresses from the generated config appears on a local interface: sing-box
// creates the device and assigns them before it installs routes, and a crashed
// or never-started process leaves neither. It takes the loopback port the
// proxy-mode probe uses so both satisfy the same engine seam; TUN mode has no
// such port and ignores it.
func tunInterfaceReady(ctx context.Context, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		ip := addrIP(addr)
		if ip == nil {
			continue
		}
		if ip.Equal(tunnelAddressIPv4) || ip.Equal(tunnelAddressIPv6) {
			return nil
		}
	}
	return errors.New("no tunnel interface carries the tunnel address yet")
}

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

func addrIP(addr net.Addr) net.IP {
	switch typed := addr.(type) {
	case *net.IPNet:
		return typed.IP
	case *net.IPAddr:
		return typed.IP
	}
	// Point-to-point interfaces can stringify as "ip/mask" or a bare address.
	text := addr.String()
	if slash := strings.IndexByte(text, '/'); slash >= 0 {
		text = text[:slash]
	}
	return net.ParseIP(text)
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
