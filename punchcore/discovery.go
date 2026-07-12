package punchcore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// GatherTimeout is the overall reflexive-discovery budget of one Gather call.
// Exported because the volunteer session layer sizes its gather context from it.
const GatherTimeout = 2 * time.Second

const (
	gatherRounds    = 4
	gatherRoundWait = 250 * time.Millisecond
)

// DefaultTTL is the punch time budget used when the hub does not specify one.
const DefaultTTL = 6 * time.Second

// GenerateNonce returns a fresh nonce as a hex string (for JSON transport) and
// its raw bytes (for the reflector wire and hub correlation key).
func GenerateNonce() (hexNonce string, raw []byte, err error) {
	raw = make([]byte, reflectNonceLen)
	if _, err = rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate punch nonce: %w", err)
	}
	return hex.EncodeToString(raw), raw, nil
}

// NonceKey converts a hex nonce (as sent in PunchRequest.ClientNonce) into the
// reflector correlation key. The hub uses this to look up its own reflector
// observations for the client (see Reflector.Classify).
func NonceKey(hexNonce string) (string, error) {
	raw, err := hex.DecodeString(hexNonce)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	if len(raw) != reflectNonceLen {
		return "", errors.New("nonce has wrong length")
	}
	return string(raw), nil
}

// Gather probes each reflector address from sock (the same socket that will later
// punch and carry QUIC) and returns the observed server-reflexive endpoints plus
// the locally-derived NAT class. It performs no long-lived reads: it clears the
// read deadline before returning so the caller can hand the socket to Attempt.
func (p Policy) Gather(ctx context.Context, sock *net.UDPConn, reflectorAddrs []string, nonce []byte) ([]Endpoint, string, error) {
	if len(reflectorAddrs) == 0 {
		return nil, ClassUnknown, errors.New("no reflector addresses")
	}
	targets := make([]*net.UDPAddr, 0, len(reflectorAddrs))
	keys := make([]string, 0, len(reflectorAddrs))
	for _, address := range reflectorAddrs {
		if p.StrictReflectorAddrs {
			host, portValue, err := net.SplitHostPort(address)
			if err != nil {
				continue
			}
			ip := net.ParseIP(host)
			port, portErr := strconv.Atoi(portValue)
			// The retained socket is udp4, and deployed reflector addresses are
			// signed literal IPv4 tuples. Rejecting coordinator-controlled DNS names
			// also keeps Close cancellation from getting stuck in an uncancelable
			// net.ResolveUDPAddr lookup.
			if ip == nil || ip.To4() == nil || !isGloballyRoutable(ip) || portErr != nil || !inRange(port, 1, 65535) {
				continue
			}
			targets = append(targets, &net.UDPAddr{IP: ip.To4(), Port: port})
			keys = append(keys, address)
		} else {
			udp, err := net.ResolveUDPAddr("udp", address)
			if err != nil {
				continue
			}
			targets = append(targets, udp)
			// Historical desktop keying: the Nth accepted target is keyed by the Nth
			// entry of the ORIGINAL list (not by its own address). Preserved verbatim
			// so the extraction is byte-for-byte behavior-neutral; do not "fix" without
			// a deliberate protocol-level change (the NAT class derived from these keys
			// is wire-visible via client_class/volunteer_class).
			keys = append(keys, reflectorAddrs[len(keys)])
		}
	}
	if len(targets) == 0 {
		return nil, ClassUnknown, errors.New("no resolvable reflector addresses")
	}

	observed := make(map[string]Endpoint) // key: reflector addr string
	buf := make([]byte, 1500)
	overall := time.Now().Add(GatherTimeout)

	for round := 0; round < gatherRounds && len(observed) < len(targets) && time.Now().Before(overall); round++ {
		if err := ctx.Err(); err != nil {
			if p.FailGatherOnCancel {
				return nil, ClassUnknown, err
			}
			break
		}
		for i, t := range targets {
			if _, done := observed[keys[i]]; done {
				continue
			}
			_, _ = sock.WriteToUDP(buildReflectRequest(nonce), t)
		}
		roundDeadline := time.Now().Add(gatherRoundWait)
		if roundDeadline.After(overall) {
			roundDeadline = overall
		}
		_ = sock.SetReadDeadline(roundDeadline)
		for time.Now().Before(roundDeadline) {
			n, src, err := sock.ReadFromUDP(buf)
			if err != nil {
				break // read deadline for this round
			}
			rn, obs, ok := parseReflectReply(buf[:n])
			if !ok || !bytes.Equal(rn, nonce) {
				continue
			}
			if key := matchReflector(src, targets, keys); key != "" {
				observed[key] = endpointFromUDP(obs, KindSrflx)
			}
		}
	}
	_ = sock.SetReadDeadline(time.Time{})

	reflexive := make([]Endpoint, 0, len(observed))
	ports := make(map[int]struct{})
	for _, ep := range observed {
		reflexive = append(reflexive, ep)
		ports[ep.Port] = struct{}{}
	}
	reflexive = dedupeEndpoints(reflexive)

	class := ClassUnknown
	switch {
	case len(observed) < 2:
		class = ClassUnknown
	case len(ports) == 1:
		class = ClassEIM
	default:
		class = ClassSymmetric
	}
	if len(reflexive) == 0 {
		return nil, class, errors.New("reflector did not observe any endpoint")
	}
	return reflexive, class, nil
}

func inRange(value, minimum, maximum int) bool {
	return value >= minimum && value <= maximum
}

func matchReflector(src *net.UDPAddr, targets []*net.UDPAddr, keys []string) string {
	for i, t := range targets {
		if t.Port == src.Port && t.IP.Equal(src.IP) {
			return keys[i]
		}
	}
	return ""
}

// LocalCandidates enumerates this host's non-loopback interface addresses paired
// with the socket's local port, as "host" candidates that help same-LAN and
// hairpin cases.
func LocalCandidates(sock *net.UDPConn) []Endpoint {
	la, ok := sock.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	port := la.Port
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]Endpoint, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue
		}
		out = append(out, Endpoint{IP: ip.String(), Port: port, Kind: KindHost})
	}
	return dedupeEndpoints(out)
}
