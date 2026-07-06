package punch

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

const probeInterval = 50 * time.Millisecond

// ErrPunchTimeout is returned when no peer probe was seen before the deadline.
var ErrPunchTimeout = errors.New("nat hole punch timed out")

type probeKind int

const (
	probeKindNone probeKind = iota
	probeKindProbe
	probeKindAck
)

func buildProbePacket(magic, sessionID string, token []byte) []byte {
	buf := make([]byte, 0, len(magic)+2+len(sessionID)+len(token))
	buf = append(buf, magic...)
	var sl [2]byte
	binary.BigEndian.PutUint16(sl[:], uint16(len(sessionID)))
	buf = append(buf, sl[:]...)
	buf = append(buf, sessionID...)
	buf = append(buf, token...)
	return buf
}

// parseProbePacket validates a probe/ack datagram against the expected session
// and token (constant-time) and returns which kind it is.
func parseProbePacket(data []byte, sessionID string, token []byte) probeKind {
	// Check the longer ACK magic first: probeMagic ("ORHOLE") is a prefix of
	// probeAckMagic ("ORHOLEACK").
	if k := matchProbe(data, probeAckMagic, sessionID, token); k {
		return probeKindAck
	}
	if k := matchProbe(data, probeMagic, sessionID, token); k {
		return probeKindProbe
	}
	return probeKindNone
}

func matchProbe(data []byte, magic, sessionID string, token []byte) bool {
	off := len(magic)
	if len(data) < off+2 {
		return false
	}
	if string(data[:off]) != magic {
		return false
	}
	sl := int(binary.BigEndian.Uint16(data[off : off+2]))
	off += 2
	if len(data) < off+sl+tokenLen {
		return false
	}
	if string(data[off:off+sl]) != sessionID {
		return false
	}
	off += sl
	return subtle.ConstantTimeCompare(data[off:off+tokenLen], token) == 1
}

// Attempt runs a simultaneous-open UDP hole punch from sock towards the peer
// candidates. It sprays token-authenticated probes and, on the first valid peer
// probe, answers with an ack; a received ack confirms the bidirectional 4-tuple.
// It returns the confirmed peer address (the actual observed source, which may
// differ from any advertised candidate).
//
// The socket is left UNCONNECTED and its read deadline cleared, so the caller can
// hand it straight to quic-go (a connected *net.UDPConn errors on WriteTo, which
// quic-go uses). Attempt runs entirely in the caller's goroutine and starts no
// background reader, so there is no race with quic-go's read loop afterwards.
func Attempt(ctx context.Context, sock *net.UDPConn, peers []Endpoint, sessionID string, token []byte, deadline time.Time) (*net.UDPAddr, error) {
	// SanitizePeers clamps the candidate count and drops globally-routable "host"
	// addresses, so a malicious peer cannot make this socket spray probes at an
	// arbitrary victim (open UDP reflector) or exhaust the sender with a huge
	// candidate list.
	peerAddrs := make([]*net.UDPAddr, 0, len(peers))
	for _, p := range SanitizePeers(peers) {
		if addr, err := p.UDPAddr(); err == nil {
			peerAddrs = append(peerAddrs, addr)
		}
	}
	if len(peerAddrs) == 0 {
		return nil, errors.New("no punch peer candidates")
	}

	probe := buildProbePacket(probeMagic, sessionID, token)
	ack := buildProbePacket(probeAckMagic, sessionID, token)
	buf := make([]byte, 1500)

	var provisional *net.UDPAddr
	nextSend := time.Now()

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			_ = sock.SetReadDeadline(time.Time{})
			return nil, err
		}
		now := time.Now()
		if !now.Before(nextSend) {
			for _, pa := range peerAddrs {
				_, _ = sock.WriteToUDP(probe, pa)
			}
			nextSend = now.Add(probeInterval)
		}

		readDeadline := nextSend
		if readDeadline.After(deadline) {
			readDeadline = deadline
		}
		_ = sock.SetReadDeadline(readDeadline)
		n, src, err := sock.ReadFromUDP(buf)
		if err != nil {
			continue // per-round read timeout; loop to send again
		}
		switch parseProbePacket(buf[:n], sessionID, token) {
		case probeKindProbe:
			// Peer reached us: the inbound hole is open. Answer so the peer can
			// confirm too, and remember the source as a provisional peer.
			_, _ = sock.WriteToUDP(ack, src)
			if provisional == nil {
				provisional = src
			}
		case probeKindAck:
			// Our probe reached the peer and its ack reached us: 4-tuple pinned.
			lingerAck(sock, ack, src)
			_ = sock.SetReadDeadline(time.Time{})
			return src, nil
		}
	}

	_ = sock.SetReadDeadline(time.Time{})
	if provisional != nil {
		// Inbound hole is open even though the final ack was lost; hand back the
		// observed peer. The QUIC handshake is the real gate — if the path is
		// only half-open it fails and the caller falls back to the hub relay.
		return provisional, nil
	}
	return nil, ErrPunchTimeout
}

// lingerAck sends a few extra acks so the peer, which may still be waiting, can
// also complete before we stop reading.
func lingerAck(sock *net.UDPConn, ack []byte, dst *net.UDPAddr) {
	for i := 0; i < 3; i++ {
		_, _ = sock.WriteToUDP(ack, dst)
		time.Sleep(20 * time.Millisecond)
	}
}
