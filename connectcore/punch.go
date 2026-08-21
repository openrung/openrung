package connectcore

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"

	"github.com/openrung/openrung/connectcore/clienttelemetry"
)

// defaultPunchPort is the hub punch coordinator port assumed when a relay does
// not advertise a punch_endpoint (its public host is then the hub).
const defaultPunchPort = "9444"

// PunchEstablisher runs one NAT punch attempt against relayID via the hub
// punch coordinator and, on success, returns the live punched path with a
// non-nil Bridge. On any failure it returns a nil path, a PunchResult whose
// Reason/NATClass are suitable for telemetry, and the error. This module owns
// when to punch and how the outcome is reported; the QUIC transport mechanics
// live outside it — in this repository the root module's internal/enginepunch
// adapts internal/punch's QUIC transport, and hosts assign its Establish to
// Engine.PunchEstablisher (or PunchOptions.Establish). A host with no
// establisher never punches.
type PunchEstablisher func(ctx context.Context, hub punchcore.HubClient, relayID string) (*PunchPath, punchcore.PunchResult, error)

// PunchBridge is a punched path's loopback bridge lifecycle. Serve runs the
// bridge until ctx ends; Close tears down the bridge and releases the punched
// socket, and must not run while sing-box could still dial the bridge.
type PunchBridge interface {
	Serve(ctx context.Context) error
	Close() error
}

// PunchPath is a live NAT-punched path as the engine consumes it: sing-box
// dials BridgeHost:BridgePort in place of the relay's public endpoint, PeerIP
// is excluded from TUN routes so the punched QUIC datagrams are not captured
// by the client's own tunnel, and Bridge carries the serve/teardown lifecycle.
// A path returned by AttemptPunch always has a non-nil Bridge: an establisher
// that produces one without is degraded to the hub fallback, never served.
type PunchPath struct {
	BridgeHost string
	BridgePort int
	PeerIP     string
	SessionID  string
	NATClass   string
	Bridge     PunchBridge
}

// Close tears down the punched path. It tolerates a nil path or missing
// bridge so teardown of a defensively-handled establishment cannot panic.
func (p *PunchPath) Close() error {
	if p == nil || p.Bridge == nil {
		return nil
	}
	return p.Bridge.Close()
}

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

	// Notify receives a typed NoticePunchOutcome for every attempted punch,
	// success or failure. Nil drops it.
	Notify func(Notice)

	// Establish is the host's punch implementation (see PunchEstablisher).
	// Nil disables punching regardless of Enabled, taking the hub path — but
	// the skip is recorded as punch_skipped (reason no_establisher): enabling
	// punching without wiring an establisher is a host misconfiguration, and
	// without the event the loss would be indistinguishable in telemetry from
	// "no punch-capable relays advertised".
	Establish PunchEstablisher
}

// AttemptPunch attempts a direct NAT-punched path to a punch-capable relay,
// bypassing the relay hub's data plane. On success it returns a live
// Establishment whose loopback bridge sing-box dials in place of the hub; the
// caller must run Bridge.Serve and Close it on teardown. On any failure (not
// punch-capable, punching disabled, symmetric NAT, hub declined, timeout) it
// returns nil and the caller silently falls back to the hub relay path — the
// outcome is never worse than not punching. All outcomes are recorded as
// telemetry.
func AttemptPunch(ctx context.Context, mgr *clienttelemetry.Manager, selected brokerapi.RelayDescriptor, opts PunchOptions) *PunchPath {
	if !opts.Enabled || !selected.PunchCapable {
		if opts.RecordSkipped && selected.PunchCapable && !opts.Enabled {
			mgr.Record("punch_skipped", selected.ID, map[string]string{"reason": "disabled"}, nil)
		}
		return nil
	}
	if opts.Establish == nil {
		// No punch implementation is wired; take the hub path, but leave a
		// telemetry and log trail (see PunchOptions.Establish for why the
		// skip must be observable).
		mgr.Record("punch_skipped", selected.ID, map[string]string{"reason": "no_establisher"}, nil)
		if opts.Log != nil {
			opts.Log("punch enabled but no establisher is wired; using relay hub")
		}
		return nil
	}

	mgr.Record("punch_attempted", selected.ID, nil, nil)
	hub := punchcore.HubClient{BaseURL: punchBaseURL(opts.BaseURL, selected), HTTPClient: punchHTTPClient(opts.Insecure)}
	est, res, err := opts.Establish(ctx, hub, selected.ID)
	if err != nil {
		if opts.Log != nil {
			opts.Log(fmt.Sprintf("punch failed (%s); using relay hub", punchReason(res.Reason)))
		}
		if opts.Notify != nil {
			opts.Notify(Notice{
				Kind:    NoticePunchOutcome,
				RelayID: selected.ID,
				Reason:  fmt.Sprintf("failed (%s); using relay hub", punchReason(res.Reason)),
			})
		}
		mgr.Record("punch_failed", selected.ID,
			map[string]string{"reason": res.Reason, "nat_class": res.NATClass}, nil)
		return nil
	}

	if est == nil || est.Bridge == nil {
		// A nil path or bridge from a non-erroring establisher is an
		// implementation bug in the host's PunchEstablisher; degrade it to
		// the hub fallback instead of panicking on first Bridge use.
		if opts.Log != nil {
			opts.Log("punch establisher returned no usable path; using relay hub")
		}
		if opts.Notify != nil {
			opts.Notify(Notice{
				Kind:    NoticePunchOutcome,
				RelayID: selected.ID,
				Reason:  "failed (invalid establishment); using relay hub",
			})
		}
		mgr.Record("punch_failed", selected.ID,
			map[string]string{"reason": "invalid_establishment", "nat_class": res.NATClass}, nil)
		return nil
	}

	if opts.Notify != nil {
		opts.Notify(Notice{
			Kind:    NoticePunchOutcome,
			RelayID: selected.ID,
			Reason:  fmt.Sprintf("punched direct path (nat %s)", res.NATClass),
		})
	}
	mgr.Record("punch_succeeded", selected.ID,
		map[string]string{"nat_class": res.NATClass},
		map[string]int64{"punch_rtt_ms": res.RTTMillis})
	return est
}

// maybePunch runs the engine's punch attempt with its configured options.
func (s *Engine) maybePunch(ctx context.Context, mgr *clienttelemetry.Manager, selected brokerapi.RelayDescriptor) *PunchPath {
	return AttemptPunch(ctx, mgr, selected, PunchOptions{
		Enabled:   s.PunchEnabled,
		BaseURL:   s.PunchURL,
		Insecure:  s.PunchInsecure,
		Log:       s.appendLog,
		Notify:    s.notify,
		Establish: s.PunchEstablisher,
	})
}

// punchBaseURL resolves the hub punch coordinator base URL: an explicit
// override wins, then the relay's advertised punch_endpoint (correct
// scheme/host/port), then a legacy http://<relay-public-host>:9444 fallback.
func punchBaseURL(override string, selected brokerapi.RelayDescriptor) string {
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
