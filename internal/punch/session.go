// Package punch is the QUIC session, transport, and bridge layer of OpenRung's
// NAT hole punching, built on the shared protocol core
// github.com/openrung/openrung/punchcore (wire format, discovery, and punch
// mechanics live there; the quic-go transport and per-repo session flows live
// here).
package punch

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/openrung/openrung/punchcore"
)

// quicHandshakeTimeout bounds the QUIC handshake over the freshly punched hole.
const quicHandshakeTimeout = 5 * time.Second

// Dialer runs the client side of a punch: discover, coordinate via the hub, punch,
// and stand up a loopback bridge that sing-box dials in place of the relay.
type Dialer struct {
	Hub     punchcore.HubClient
	RelayID string
	Logger  *slog.Logger
}

// Establishment is a live punched path. The caller starts Bridge.Serve with a
// session-lived context and points sing-box at BridgeHost:BridgePort. PeerIP must
// be added to sing-box's route_exclude_address so the punched QUIC datagrams are
// not captured by the client's own TUN.
type Establishment struct {
	Bridge     *ClientBridge
	BridgeHost string
	BridgePort int
	PeerIP     string
	SessionID  string
	NATClass   string

	sock *net.UDPConn
}

// Close tears down the bridge and releases the punched socket.
func (e *Establishment) Close() error {
	if e.Bridge != nil {
		_ = e.Bridge.Close()
	}
	if e.sock != nil {
		_ = e.sock.Close()
	}
	return nil
}

func (d *Dialer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Establish attempts the full client punch flow. On success it returns a live
// Establishment (the caller must run Bridge.Serve) and an OK PunchResult. On any
// failure it returns a non-nil error and a PunchResult whose Reason/NATClass are
// suitable for telemetry; the caller then falls back to the hub relay. A
// *punchcore.HubHTTPError with status 404/409 signals a stale relay id (re-fetch
// relays).
func (d *Dialer) Establish(ctx context.Context) (*Establishment, punchcore.PunchResult, error) {
	pol := punchcore.DesktopPolicy()
	start := time.Now()
	res := punchcore.PunchResult{}

	cfg, err := d.Hub.FetchConfig(ctx)
	if err != nil {
		res.Reason = "config"
		return nil, res, err
	}

	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		res.Reason = "socket"
		return nil, res, err
	}
	established := false
	defer func() {
		if !established {
			_ = sock.Close()
		}
	}()

	nonceHex, nonceRaw, err := punchcore.GenerateNonce()
	if err != nil {
		res.Reason = "nonce"
		return nil, res, err
	}

	reflexive, class, gatherErr := pol.Gather(ctx, sock, cfg.ReflectorAddrs, nonceRaw)
	res.NATClass = class
	local := punchcore.LocalCandidates(sock)
	if len(reflexive) == 0 && len(local) == 0 {
		res.Reason = "discovery"
		if gatherErr == nil {
			gatherErr = fmt.Errorf("no candidates gathered")
		}
		return nil, res, gatherErr
	}

	resp, err := d.Hub.RequestPunch(ctx, punchcore.PunchRequest{
		RelayID:         d.RelayID,
		ClientNonce:     nonceHex,
		ClientReflexive: reflexive,
		ClientLocal:     local,
		ClientClass:     class,
		QUICALPN:        punchcore.ALPN,
		ProtoVersion:    punchcore.ProtoVersion,
	})
	if err != nil {
		res.Reason = "request"
		return nil, res, err
	}
	res.SessionID = resp.SessionID
	if !resp.OK {
		res.Reason = "declined:" + resp.Error
		return nil, res, fmt.Errorf("hub declined punch: %s", resp.Error)
	}

	token, err := punchcore.DecodeToken(resp.PunchToken)
	if err != nil {
		res.Reason = "token"
		return nil, res, err
	}

	ttl := time.Duration(resp.TTLMillis) * time.Millisecond
	if ttl <= 0 {
		ttl = punchcore.DefaultTTL
	}
	deadline := time.Now().Add(ttl)
	peers := append(append([]punchcore.Endpoint{}, resp.VolunteerReflexive...), resp.VolunteerLocal...)

	confirmed, err := pol.Attempt(ctx, sock, peers, resp.SessionID, token, deadline)
	if err != nil {
		res.Reason = "punch"
		return nil, res, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, quicHandshakeTimeout)
	defer cancel()
	conn, err := DialQUIC(dialCtx, sock, confirmed, resp.CertFingerprint)
	if err != nil {
		res.Reason = "quic"
		return nil, res, err
	}

	bridge, err := NewClientBridge(conn, token, d.logger())
	if err != nil {
		_ = conn.CloseWithError(0, "")
		res.Reason = "bridge"
		return nil, res, err
	}

	host, port := bridge.Endpoint()
	established = true
	res.OK = true
	res.RTTMillis = time.Since(start).Milliseconds()
	return &Establishment{
		Bridge:     bridge,
		BridgeHost: host,
		BridgePort: port,
		PeerIP:     confirmed.IP.String(),
		SessionID:  resp.SessionID,
		NATClass:   resp.VolunteerClass,
		sock:       sock,
	}, res, nil
}

// RespondToDirective runs the relay side of a punch. It synchronously gathers
// its own reflexive candidates and returns a PunchAck for the hub to relay to the
// client, then punches and serves QUIC in the background (bounded by the
// directive TTL and ctx), bridging streams to targetHost:targetPort (the loopback
// Xray listener). ctx should be the long-lived tunnel context so the background
// punch survives the control stream closing but stops on tunnel shutdown.
func RespondToDirective(ctx context.Context, dir punchcore.PunchDirective, targetHost string, targetPort int, logger *slog.Logger) punchcore.PunchAck {
	pol := punchcore.DesktopPolicy()
	if logger == nil {
		logger = slog.Default()
	}
	fail := func(reason string) punchcore.PunchAck {
		return punchcore.PunchAck{SessionID: dir.SessionID, OK: false, Error: reason}
	}

	token, err := punchcore.DecodeToken(dir.PunchToken)
	if err != nil {
		return fail("bad token")
	}

	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fail("socket")
	}

	_, nonceRaw, err := punchcore.GenerateNonce()
	if err != nil {
		_ = sock.Close()
		return fail("nonce")
	}

	gatherCtx, gatherCancel := context.WithTimeout(ctx, punchcore.GatherTimeout+500*time.Millisecond)
	reflexive, class, _ := pol.Gather(gatherCtx, sock, dir.ReflectorAddrs, nonceRaw)
	gatherCancel()
	local := punchcore.LocalCandidates(sock)
	if len(reflexive) == 0 && len(local) == 0 {
		_ = sock.Close()
		return fail("discovery")
	}

	cert, fingerprint, err := GenerateSessionCert()
	if err != nil {
		_ = sock.Close()
		return fail("cert")
	}

	ttl := time.Duration(dir.TTLMillis) * time.Millisecond
	if ttl <= 0 {
		ttl = punchcore.DefaultTTL
	}
	peers := append(append([]punchcore.Endpoint{}, dir.ClientReflexive...), dir.ClientLocal...)

	go func() {
		defer sock.Close()

		// The TTL bounds only the punch attempt (hole opening). Once the hole is
		// open the QUIC session must live for the whole tunnel lifetime, so the
		// bridge runs under ctx (the long-lived tunnel context), not the TTL
		// deadline — otherwise every punched session would be torn down ~TTL
		// seconds after it is established.
		deadline := time.Now().Add(ttl)
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		_, err := pol.Attempt(attemptCtx, sock, peers, dir.SessionID, token, deadline)
		cancel()
		if err != nil {
			logger.Info("punch attempt failed", "session", dir.SessionID, "error", err)
			return
		}
		ln, err := ListenQUIC(sock, cert)
		if err != nil {
			logger.Warn("punch listen quic failed", "session", dir.SessionID, "error", err)
			return
		}
		if err := RelayBridge(ctx, ln, token, targetHost, targetPort, logger); err != nil && ctx.Err() == nil {
			logger.Info("punch bridge closed", "session", dir.SessionID, "error", err)
		}
	}()

	return punchcore.PunchAck{
		SessionID:          dir.SessionID,
		OK:                 true,
		VolunteerReflexive: reflexive,
		VolunteerLocal:     local,
		VolunteerClass:     class,
		CertFingerprint:    fingerprint,
	}
}
