// Package orpunch is the gomobile-facing wrapper around the OpenRung NAT
// hole-punch client (internal/punch). It is bound into the mobile app's
// sing-box/libbox AAR so Android (and later iOS) can reach a CGNAT volunteer
// directly, bypassing the relay hub's data path, exactly like the desktop
// client's cmd/client maybePunch flow.
//
// gomobile only marshals a restricted set of types across the JNI boundary, so
// every exported symbol here uses String/int/bool/interface only. The rich Go
// types (*net.UDPConn, *quic.Conn, punch.Establishment, context.Context) never
// cross the boundary — they are owned entirely inside Session.
//
// A hole punch that does not succeed is NOT reported as an error: Dial returns a
// Session with OK()==false carrying a structured Reason/NATClass for telemetry,
// and the caller silently falls back to the relay hub endpoint. Dial returns a
// non-nil error only for programmer misuse (nil protector, empty config).
package orpunch

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	"openrung/internal/punch"
)

// hubHTTPTimeout bounds each hub coordination request. Mirrors the default
// punch.HubClient timeout so the mobile wrapper behaves like the desktop client.
const hubHTTPTimeout = 10 * time.Second

// SocketProtector is implemented on the platform side (Kotlin/Swift). Protect is
// invoked with the punch UDP socket's raw file descriptor at creation, before any
// datagram is sent, so the platform can excuse it from the app's own VPN tunnel.
// On Android the implementation calls VpnService.protect(fd), the same seam
// libbox already uses for its outbound sockets.
type SocketProtector interface {
	Protect(fd int)
}

// Config carries the punch coordination parameters from the platform. It is a
// gomobile-safe struct: only String/bool fields, so gomobile generates a plain
// Java class with a constructor and getters/setters.
type Config struct {
	// HubBaseURL is the hub punch coordinator base URL — the relay descriptor's
	// punch_endpoint, or a derived http://<publicHost>:9444 when it is empty.
	HubBaseURL string
	// RelayID is the broker relay id of the punch-capable volunteer.
	RelayID string
	// Insecure skips TLS verification of the hub HTTP coordination endpoint only
	// (for a volunteer-run hub with a self-signed cert). The punched QUIC data
	// path still pins the volunteer's per-session certificate by fingerprint and
	// the tunnel itself is VLESS+REALITY, so a hub MITM can at worst force a
	// fallback to the relay path, never read or redirect traffic.
	Insecure bool
}

// Session is an opaque handle to a punch attempt. On success (OK()==true) it owns
// the live punched path and the loopback bridge goroutine; the caller points
// sing-box at BridgeHost():BridgePort() and adds PeerIP() to
// route_exclude_address. Close() tears everything down and is always safe to call,
// including after a failed attempt.
type Session struct {
	ok        bool
	reason    string
	natClass  string
	rttMillis int64
	sessionID string

	est    *punch.Establishment
	cancel context.CancelFunc
}

// OK reports whether the punch succeeded and BridgeHost/BridgePort/PeerIP are
// valid. When false, the caller falls back to the relay hub endpoint.
func (s *Session) OK() bool { return s.ok }

// Reason is the failure reason for telemetry when OK()==false ("" on success).
// One of: config, socket, nonce, discovery, request, declined:<hubError>, token,
// punch, quic, bridge.
func (s *Session) Reason() string { return s.reason }

// NATClass is the client's locally-derived NAT mapping class: eim, symmetric, or
// unknown. Advisory; the hub's reflector-observed class is authoritative.
func (s *Session) NATClass() string { return s.natClass }

// RTTMillis is the end-to-end punch establishment time in milliseconds (0 unless
// OK()).
func (s *Session) RTTMillis() int64 { return s.rttMillis }

// SessionID is the hub-assigned punch session id (empty until the hub replies).
func (s *Session) SessionID() string { return s.sessionID }

// BridgeHost is the loopback host sing-box should dial in place of the relay
// ("127.0.0.1" when OK()).
func (s *Session) BridgeHost() string {
	if s.est == nil {
		return ""
	}
	return s.est.BridgeHost
}

// BridgePort is the loopback TCP port sing-box should dial (0 unless OK()).
func (s *Session) BridgePort() int {
	if s.est == nil {
		return 0
	}
	return s.est.BridgePort
}

// PeerIP is the volunteer's reflexive UDP IP on the punched path. It MUST be
// added to the sing-box TUN's route_exclude_address so the QUIC datagrams the
// punch socket exchanges are not captured by auto_route/strict_route.
func (s *Session) PeerIP() string {
	if s.est == nil {
		return ""
	}
	return s.est.PeerIP
}

// Close cancels the bridge context and releases the punched QUIC connection and
// UDP socket. Safe to call multiple times and on a failed Session.
func (s *Session) Close() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.est != nil {
		err := s.est.Close()
		s.est = nil
		return err
	}
	return nil
}

// Dial runs the full client punch flow — hub config fetch, reflector discovery,
// punch request, UDP hole punch, QUIC dial, loopback bridge — and, on success,
// starts serving the bridge on an internal goroutine bound to the Session's
// context. The blocking work (hub HTTP up to ~10s, punch TTL ~6s, QUIC handshake
// ~5s) runs synchronously in the caller's thread, so the platform must call Dial
// off the main/UI thread.
func Dial(cfg *Config, protector SocketProtector) (*Session, error) {
	if cfg == nil {
		return nil, errors.New("orpunch: nil config")
	}
	if cfg.HubBaseURL == "" || cfg.RelayID == "" {
		return nil, errors.New("orpunch: config requires HubBaseURL and RelayID")
	}
	if protector == nil {
		return nil, errors.New("orpunch: nil protector")
	}

	ctx, cancel := context.WithCancel(context.Background())

	dialer := &punch.Dialer{
		Hub:     punch.HubClient{BaseURL: cfg.HubBaseURL, HTTPClient: hubHTTPClient(cfg.Insecure)},
		RelayID: cfg.RelayID,
		Control: func(fd uintptr) { protector.Protect(int(fd)) },
	}

	est, res, err := dialer.Establish(ctx)
	if err != nil {
		cancel()
		return &Session{
			ok:        false,
			reason:    res.Reason,
			natClass:  res.NATClass,
			sessionID: res.SessionID,
		}, nil
	}

	// The bridge must outlive Establish: sing-box dials the loopback listener for
	// the whole tunnel lifetime. It stops when the Session is Closed (ctx cancel)
	// or the underlying QUIC connection drops.
	go func() { _ = est.Bridge.Serve(ctx) }()

	return &Session{
		ok:        true,
		reason:    res.Reason,
		natClass:  res.NATClass,
		rttMillis: res.RTTMillis,
		sessionID: res.SessionID,
		est:       est,
		cancel:    cancel,
	}, nil
}

// hubHTTPClient returns the HTTP client for the hub coordination API. With
// insecure set it skips TLS verification for a self-signed hub cert; nil uses
// punch.HubClient's bounded default. This weakens only the coordination channel —
// the data path is independently secured (see Config.Insecure).
func hubHTTPClient(insecure bool) *http.Client {
	if !insecure {
		return &http.Client{Timeout: hubHTTPTimeout}
	}
	return &http.Client{
		Timeout: hubHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // self-signed hub cert; data path is independently secured
		},
	}
}
