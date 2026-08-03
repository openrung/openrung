package wsscore

import "github.com/gorilla/websocket"

// SessionEnd explains why a client session stopped. A transport that closes
// itself, or a peer that shuts down in an orderly way, is not a lost path: only
// SessionEndTransport means the session ended without the peer saying so, which
// is what censorship, a dropped radio, or a dead CDN edge look like.
//
// The distinction is observable rather than assumed. An orderly WebSocket close
// frame reaches the client through the CDN; a broken path yields an abnormal
// closure instead, because no close frame ever arrives.
type SessionEnd int

const (
	// SessionEndNone means the session has not ended.
	SessionEndNone SessionEnd = iota
	// SessionEndLocal means the caller closed the session.
	SessionEndLocal
	// SessionEndIdle means the local no-stream idle guard closed the session.
	SessionEndIdle
	// SessionEndLifetime means the bounded session lifetime elapsed.
	SessionEndLifetime
	// SessionEndRemote means the peer closed the session in an orderly way.
	SessionEndRemote
	// SessionEndTransport means the session ended without an orderly close:
	// the path was lost.
	SessionEndTransport
)

// Graceful reports whether the session ended in an orderly way. A graceful end
// is expected operational behavior and must not be reported as path loss; only
// SessionEndTransport (and SessionEndNone, which means the caller asked too
// early) are not graceful.
func (e SessionEnd) Graceful() bool {
	switch e {
	case SessionEndLocal, SessionEndIdle, SessionEndLifetime, SessionEndRemote:
		return true
	default:
		return false
	}
}

// String returns a stable, bounded token safe to carry in telemetry.
func (e SessionEnd) String() string {
	switch e {
	case SessionEndLocal:
		return "local"
	case SessionEndIdle:
		return "idle"
	case SessionEndLifetime:
		return "lifetime"
	case SessionEndRemote:
		return "remote"
	case SessionEndTransport:
		return "transport"
	default:
		return "none"
	}
}

// sessionEndForCloseCode maps an observed close code. A session ended in an
// orderly way when a close frame arrived AND says the peer chose to stop. A
// frame reporting an error condition, and the absence of any frame, both mean
// the path was lost.
//
// The peer choosing to stop is the whole set below. CloseNoStatusReceived is
// gorilla's designation for a close frame whose payload carried no status
// (conn.go assigns it whenever that payload is under two bytes); such a frame
// is legal per RFC 6455 section 5.5.1 and is still the peer closing, so
// requiring a status would report an orderly shutdown as censorship — the exact
// misreport this type exists to prevent. CloseServiceRestart and
// CloseTryAgainLater are a peer stopping deliberately, which is what a relay
// redeploy looks like.
//
// Everything else stays loss, and each for a reason: zero means no frame ever
// arrived, CloseAbnormalClosure is gorilla's designation for exactly that,
// CloseProtocolError / CloseUnsupportedData / CloseInvalidFramePayloadData /
// CloseMessageTooBig / CloseMandatoryExtension report a broken exchange,
// ClosePolicyViolation is a rejection worth surfacing rather than renewing
// through, CloseInternalServerErr is the relay failing, and 1014 bad-gateway
// (which gorilla does not name) is an edge reporting its origin unreachable.
//
// The frame is authored by whatever terminates the WebSocket, which for a
// fronted session is the CDN edge rather than the relay behind it. Two
// consequences follow. A censor cannot forge one, because it sits outside the
// TLS session it would have to write into, so the signal survives the threat it
// exists to distinguish. But an edge that reports an orderly close after its
// own origin died abruptly would have a client renew quietly instead of
// reporting loss; the renewal then fails at the ticket or handshake stage,
// which is still recorded. The code decides only whether to renew quietly or
// report loss, and never grants trust.
func sessionEndForCloseCode(code int) SessionEnd {
	switch code {
	case websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
		websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater:
		return SessionEndRemote
	default:
		return SessionEndTransport
	}
}
