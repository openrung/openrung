package connectcore

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/openrung/openrung/punchcore"

	"openrung/internal/clienttelemetry"
	"openrung/internal/punch"
	"openrung/internal/relay"
)

// defaultPunchPort is the hub punch coordinator port assumed when a relay does
// not advertise a punch_endpoint (its public host is then the hub).
const defaultPunchPort = "9444"

// PunchOptions configures a single punch attempt. The zero value is the safe
// default: punching disabled, no override, hub TLS verified.
type PunchOptions struct {
	// Enabled attempts a direct NAT-punched path to punch-capable relays
	// before falling back to the relay hub's data plane.
	Enabled bool

	// BaseURL overrides the hub punch coordinator base URL (the CLI's
	// -punch-url). Empty uses the relay's advertised punch_endpoint.
	BaseURL string

	// Insecure skips TLS verification of the hub punch API; see
	// punchHTTPClient for why that stays safe. It defaults to false: only a
	// caller that knowingly talks to a self-signed hub sets it.
	Insecure bool

	// RecordSkipped emits punch_skipped for a punch-capable relay when
	// punching is disabled. Only the CLI reports that event; the desktop
	// engine never has.
	RecordSkipped bool

	// Log receives one human-readable line when an attempt fails. Nil drops
	// it (the CLI keeps its stdout quiet on a silent fallback).
	Log func(string)
}

// AttemptPunch attempts a direct NAT-punched path to a punch-capable relay,
// bypassing the relay hub's data plane. On success it returns a live
// Establishment whose loopback bridge sing-box dials in place of the hub; the
// caller must run Bridge.Serve and Close it on teardown. On any failure (not
// punch-capable, punching disabled, symmetric NAT, hub declined, timeout) it
// returns nil and the caller silently falls back to the hub relay path — the
// outcome is never worse than not punching. All outcomes are recorded as
// telemetry.
func AttemptPunch(ctx context.Context, mgr *clienttelemetry.Manager, selected relay.Descriptor, opts PunchOptions) *punch.Establishment {
	if !opts.Enabled || !selected.PunchCapable {
		if opts.RecordSkipped && selected.PunchCapable && !opts.Enabled {
			mgr.Record("punch_skipped", selected.ID, map[string]string{"reason": "disabled"}, nil)
		}
		return nil
	}

	mgr.Record("punch_attempted", selected.ID, nil, nil)
	dialer := &punch.Dialer{
		Hub:     punchcore.HubClient{BaseURL: punchBaseURL(opts.BaseURL, selected), HTTPClient: punchHTTPClient(opts.Insecure)},
		RelayID: selected.ID,
	}
	est, res, err := dialer.Establish(ctx)
	if err != nil {
		if opts.Log != nil {
			opts.Log(fmt.Sprintf("punch failed (%s); using relay hub", punchReason(res.Reason)))
		}
		mgr.Record("punch_failed", selected.ID,
			map[string]string{"reason": res.Reason, "nat_class": res.NATClass}, nil)
		return nil
	}

	mgr.Record("punch_succeeded", selected.ID,
		map[string]string{"nat_class": res.NATClass},
		map[string]int64{"punch_rtt_ms": res.RTTMillis})
	return est
}

// maybePunch runs the engine's punch attempt with its configured options.
func (s *Engine) maybePunch(ctx context.Context, mgr *clienttelemetry.Manager, selected relay.Descriptor) *punch.Establishment {
	return AttemptPunch(ctx, mgr, selected, PunchOptions{
		Enabled:  s.PunchEnabled,
		BaseURL:  s.PunchURL,
		Insecure: s.PunchInsecure,
		Log:      s.appendLog,
	})
}

// punchBaseURL resolves the hub punch coordinator base URL: an explicit
// override wins, then the relay's advertised punch_endpoint (correct
// scheme/host/port), then a legacy http://<relay-public-host>:9444 fallback.
func punchBaseURL(override string, selected relay.Descriptor) string {
	if override != "" {
		return override
	}
	if selected.PunchEndpoint != "" {
		return selected.PunchEndpoint
	}
	return "http://" + net.JoinHostPort(selected.PublicHost, defaultPunchPort)
}

// punchHTTPClient returns the HTTP client for the hub punch coordination API.
// With insecure set it skips TLS verification, for a hub serving a self-signed
// cert on its HTTPS punch endpoint (relay hubs on bare IPs cannot get a
// CA cert). This weakens ONLY the hub coordination channel: the punched QUIC
// data path still pins the relay's per-session cert by fingerprint, and the
// tunnel itself is VLESS+REALITY keyed by broker-delivered credentials, so a hub
// MITM can at worst force a fallback to the relay path, never read or redirect
// the tunnel.
func punchHTTPClient(insecure bool) *http.Client {
	if !insecure {
		return nil // punchcore.HubClient uses its default client
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // opt-in for a self-signed hub cert; data path is independently secured
		},
	}
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
