// Package punch implements NAT hole punching so a client and a CGNAT volunteer
// (both behind NAT, neither able to accept inbound) can exchange the opaque
// VLESS/Reality byte stream directly, bypassing the relay hub data path.
//
// The relay hub is the rendezvous: it hosts a UDP reflector (STUN-like) and
// relays a punch offer to the volunteer over the existing yamux control channel.
// Both peers learn their server-reflexive UDP endpoints from the reflector, spray
// simultaneous-open probes at each other, and then run QUIC over the punched
// socket. QUIC provides the reliable, ordered, multiplexed byte stream that
// Reality-over-TCP needs. Reality still terminates only at client and volunteer;
// no third party can decrypt the traffic.
package punch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ALPN is the QUIC application protocol negotiated on the punched path.
const ALPN = "openrung-punch/1"

// ProtoVersion is the punch coordination protocol version carried in the HTTP
// and control-channel messages. Additive JSON fields are wire-compatible; a
// breaking change bumps this.
const ProtoVersion = 1

// Hub HTTP endpoints (served by the relay hub on its own listener, not the
// broker — see cmd/relayhub).
const (
	PathPunchConfig  = "/api/v1/punch/config"
	PathPunchRequest = "/api/v1/punch/request"
	PathPunchResult  = "/api/v1/punch/result"
)

// PunchConfig is the hub -> client reply to GET PathPunchConfig, letting a client
// discover the reflector addresses (which include a second public IP that cannot
// be derived from the relay descriptor) before it probes.
type PunchConfig struct {
	ReflectorAddrs []string `json:"reflector_addrs"`
	ALPN           string   `json:"quic_alpn"`
	TTLMillis      int64    `json:"ttl_ms"`
}

// NAT mapping classes derived from probing the reflector's distinct IPs.
const (
	// ClassEIM is endpoint-independent mapping (full cone / restricted cone):
	// the reflexive port is stable across destinations, so the port advertised
	// to the peer is the port the peer will actually see. Punchable.
	ClassEIM = "eim"
	// ClassSymmetric is endpoint/address-dependent mapping: a fresh port per
	// destination, so the advertised reflexive port does not match what the peer
	// sees. Not punchable without port prediction; we skip these.
	ClassSymmetric = "symmetric"
	// ClassUnknown means classification was inconclusive (only one reflector IP
	// answered). Attempt anyway, then fall back.
	ClassUnknown = "unknown"
)

// Reflector wire protocol. A peer sends a padded request from the socket it will
// punch from; the reflector echoes the observed source ip:port. Request >= reply
// keeps the amplification factor below 1.
const (
	reflectMagicRequest = "ORPUNCHRQ" // 9 bytes
	reflectMagicReply   = "ORPUNCHRS" // 9 bytes
	// reflectNonceLen bytes of caller-chosen nonce echoed in the reply so the
	// caller can match replies to requests and the hub can correlate a peer's
	// observations across its distinct reflector IPs.
	reflectNonceLen = 16
	// reflectMinRequest is the minimum accepted request size. Shorter datagrams
	// are dropped before any reply so the reflector can never amplify traffic.
	reflectMinRequest = 64
)

// Punch wire protocol over the (not yet QUIC) UDP socket.
const (
	probeMagic    = "ORHOLE"    // 6 bytes
	probeAckMagic = "ORHOLEACK" // 9 bytes
	// tokenLen is the HMAC-SHA256 punch token length in bytes.
	tokenLen = sha256.Size
)

// Endpoint is a candidate transport address (host or server-reflexive).
type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Kind string `json:"kind"` // "host" | "srflx"
}

const (
	KindHost  = "host"
	KindSrflx = "srflx"
)

// UDPAddr resolves the endpoint to a *net.UDPAddr.
func (e Endpoint) UDPAddr() (*net.UDPAddr, error) {
	ip := net.ParseIP(e.IP)
	if ip == nil {
		return nil, fmt.Errorf("invalid endpoint ip %q", e.IP)
	}
	if e.Port < 1 || e.Port > 65535 {
		return nil, fmt.Errorf("invalid endpoint port %d", e.Port)
	}
	return &net.UDPAddr{IP: ip, Port: e.Port}, nil
}

func (e Endpoint) String() string { return net.JoinHostPort(e.IP, strconv.Itoa(e.Port)) }

func endpointFromUDP(addr *net.UDPAddr, kind string) Endpoint {
	return Endpoint{IP: addr.IP.String(), Port: addr.Port, Kind: kind}
}

// dedupeEndpoints removes duplicate ip:port pairs, preserving order.
func dedupeEndpoints(in []Endpoint) []Endpoint {
	seen := make(map[string]struct{}, len(in))
	out := make([]Endpoint, 0, len(in))
	for _, e := range in {
		if e.IP == "" || e.Port <= 0 {
			continue
		}
		key := e.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

// maxPunchPeers caps how many candidate endpoints of each kind (host, srflx) a
// single peer may advertise. A legitimate peer offers a handful — one reflexive
// per reflector vantage point plus a few LAN addresses — so a longer list is a
// bid to turn Attempt's probe spray into a reflected flood at third parties.
const maxPunchPeers = 8

// isGloballyRoutable reports whether ip is an ordinary public unicast address,
// i.e. NOT loopback, private (RFC1918/ULA), link-local, carrier-grade-NAT shared
// space (RFC6598 100.64.0.0/10), unspecified, or multicast.
func isGloballyRoutable(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false // 100.64.0.0/10 carrier-grade NAT shared space
	}
	return true
}

// SanitizePeers filters and clamps peer-advertised punch candidates so the probe
// spray in Attempt can never be aimed at an arbitrary third party:
//
//   - A "host" (LAN) candidate must NOT be globally routable. A public IP tagged
//     as a same-subnet address is an attempt to make the volunteer flood a
//     victim, never a real LAN peer, so it is dropped.
//   - Multicast/unspecified addresses and invalid ports are always dropped.
//   - Each kind (host, srflx) is independently capped at maxPunchPeers.
//
// Reflexive ("srflx") provenance is enforced upstream by the punch coordinator,
// which forwards only reflector-observed reflexive endpoints; a public srflx
// reaching here has therefore already been proven to belong to a real peer.
func SanitizePeers(in []Endpoint) []Endpoint {
	out := make([]Endpoint, 0, len(in))
	var hostN, srflxN int
	for _, e := range dedupeEndpoints(in) {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.Port < 1 || e.Port > 65535 || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		switch e.Kind {
		case KindHost:
			if isGloballyRoutable(ip) || hostN >= maxPunchPeers {
				continue
			}
			hostN++
		case KindSrflx:
			if srflxN >= maxPunchPeers {
				continue
			}
			srflxN++
		default:
			continue // unknown kind: never spray at it
		}
		out = append(out, e)
	}
	return out
}

// PunchRequest is the client -> hub HTTP body (POST /api/v1/punch/request).
type PunchRequest struct {
	RelayID         string     `json:"relay_id"`
	ClientNonce     string     `json:"client_nonce"`
	ClientReflexive []Endpoint `json:"client_reflexive,omitempty"`
	ClientLocal     []Endpoint `json:"client_local,omitempty"`
	ClientClass     string     `json:"client_class,omitempty"`
	QUICALPN        string     `json:"quic_alpn"`
	ProtoVersion    int        `json:"proto_version"`
}

// PunchResponse is the hub -> client HTTP reply.
type PunchResponse struct {
	OK                 bool       `json:"ok"`
	Error              string     `json:"error,omitempty"`
	SessionID          string     `json:"session_id,omitempty"`
	VolunteerReflexive []Endpoint `json:"volunteer_reflexive,omitempty"`
	VolunteerLocal     []Endpoint `json:"volunteer_local,omitempty"`
	VolunteerClass     string     `json:"volunteer_class,omitempty"`
	PunchToken         string     `json:"punch_token,omitempty"`
	CertFingerprint    string     `json:"cert_fingerprint,omitempty"`
	// TTLMillis is the relative punch budget from receipt (avoids cross-machine
	// clock skew; each side computes its own absolute deadline locally).
	TTLMillis int64 `json:"ttl_ms,omitempty"`
}

// PunchDirective is the hub -> volunteer message sent over the yamux punch-control
// stream (framed by internal/tunnel).
type PunchDirective struct {
	SessionID       string     `json:"session_id"`
	RelayID         string     `json:"relay_id"`
	ClientReflexive []Endpoint `json:"client_reflexive,omitempty"`
	ClientLocal     []Endpoint `json:"client_local,omitempty"`
	ClientClass     string     `json:"client_class,omitempty"`
	PunchToken      string     `json:"punch_token"`
	ReflectorAddrs  []string   `json:"reflector_addrs"`
	TTLMillis       int64      `json:"ttl_ms"`
	QUICALPN        string     `json:"quic_alpn"`
	ProtoVersion    int        `json:"proto_version"`
}

// PunchAck is the volunteer -> hub reply on the same punch-control stream.
type PunchAck struct {
	SessionID          string     `json:"session_id"`
	OK                 bool       `json:"ok"`
	VolunteerReflexive []Endpoint `json:"volunteer_reflexive,omitempty"`
	VolunteerLocal     []Endpoint `json:"volunteer_local,omitempty"`
	VolunteerClass     string     `json:"volunteer_class,omitempty"`
	CertFingerprint    string     `json:"cert_fingerprint,omitempty"`
	Error              string     `json:"error,omitempty"`
}

// PunchResult is best-effort client -> hub telemetry (POST /api/v1/punch/result).
type PunchResult struct {
	SessionID string `json:"session_id"`
	OK        bool   `json:"ok"`
	Reason    string `json:"reason,omitempty"`
	RTTMillis int64  `json:"rtt_ms,omitempty"`
	NATClass  string `json:"nat_class,omitempty"`
}

// ComputeToken derives the per-session punch token. The hub holds hubSessionSecret
// and issues the token to the client (HTTP) and volunteer (control channel); both
// present it in the UDP probes and the first QUIC stream.
func ComputeToken(secret []byte, sessionID, relayID, clientNonce string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID))
	mac.Write([]byte{'|'})
	mac.Write([]byte(relayID))
	mac.Write([]byte{'|'})
	mac.Write([]byte(clientNonce))
	return mac.Sum(nil)
}

// EncodeToken hex-encodes a token for JSON transport.
func EncodeToken(token []byte) string { return hex.EncodeToString(token) }

// DecodeToken parses a hex token and validates its length.
func DecodeToken(s string) ([]byte, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode punch token: %w", err)
	}
	if len(raw) != tokenLen {
		return nil, errors.New("punch token has wrong length")
	}
	return raw, nil
}
