package vpnservice

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"openrung/internal/clienttelemetry"
	"openrung/internal/punch"
	"openrung/internal/relay"
)

// defaultPunchPort is the hub punch coordinator port assumed when a relay does
// not advertise a punch_endpoint (its public host is then the hub).
const defaultPunchPort = "9444"

// maybePunch attempts a direct NAT-punched path to a punch-capable volunteer,
// bypassing the relay hub's data plane. On success it returns a live
// Establishment whose loopback bridge sing-box dials in place of the hub; the
// caller must run Bridge.Serve and Close it on teardown. On any failure (not
// punch-capable, symmetric NAT, hub declined, timeout) it returns nil and the
// caller silently falls back to the hub relay path — the outcome is never worse
// than not punching. Ported from cmd/client/main.go maybePunch.
func (s *Service) maybePunch(ctx context.Context, mgr *clienttelemetry.Manager, selected relay.Descriptor) *punch.Establishment {
	if !s.PunchEnabled || !selected.PunchCapable {
		return nil
	}

	recordPunch(mgr, "punch_attempted", selected.ID, nil, nil)
	dialer := &punch.Dialer{
		Hub:     punch.HubClient{BaseURL: punchBaseURL(selected), HTTPClient: punchHTTPClient(s.PunchInsecure)},
		RelayID: selected.ID,
	}
	est, res, err := dialer.Establish(ctx)
	if err != nil {
		s.appendLog(fmt.Sprintf("punch failed (%s); using relay hub", punchReason(res.Reason)))
		recordPunch(mgr, "punch_failed", selected.ID,
			map[string]string{"reason": res.Reason, "nat_class": res.NATClass}, nil)
		return nil
	}

	recordPunch(mgr, "punch_succeeded", selected.ID,
		map[string]string{"nat_class": res.NATClass},
		map[string]int64{"punch_rtt_ms": res.RTTMillis})
	return est
}

// punchBaseURL resolves the hub punch coordinator base URL: the relay's
// advertised punch_endpoint (correct scheme/host/port), then a legacy
// http://<relay-public-host>:9444 fallback. The desktop app has no override.
func punchBaseURL(selected relay.Descriptor) string {
	if selected.PunchEndpoint != "" {
		return selected.PunchEndpoint
	}
	return "http://" + net.JoinHostPort(selected.PublicHost, defaultPunchPort)
}

// punchHTTPClient returns the HTTP client for the hub punch coordination API.
// With insecure set it skips TLS verification, for a hub serving a self-signed
// cert on its HTTPS punch endpoint (volunteer-run hubs on bare IPs cannot get a
// CA cert). This weakens ONLY the hub coordination channel: the punched QUIC
// data path still pins the volunteer's per-session cert by fingerprint, and the
// tunnel itself is VLESS+REALITY keyed by broker-delivered credentials, so a hub
// MITM can at worst force a fallback to the relay path, never read or redirect
// the tunnel.
func punchHTTPClient(insecure bool) *http.Client {
	if !insecure {
		return nil // punch.HubClient uses its default client
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // self-signed hub cert; data path is independently secured
		},
	}
}

func recordPunch(mgr *clienttelemetry.Manager, event, relayID string, attrs map[string]string, meas map[string]int64) {
	if mgr == nil {
		return
	}
	mgr.Record(event, relayID, attrs, meas)
}

// punchReason humanizes a PunchResult.Reason for the log console.
func punchReason(reason string) string {
	switch reason {
	case "":
		return "unknown"
	case "discovery":
		return "symmetric NAT"
	case "declined":
		return "hub declined"
	default:
		return reason
	}
}
