package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the tunnel control-protocol version. The relay sends it
// in the HELLO frame and the hub rejects mismatches. It stays 1: the punch
// stream-typing extension is negotiated with additive HELLO/HELLO_ACK bools so a
// mixed fleet of old and new relays/hubs keeps working (bumping the version
// would make either side hard-reject the other and break tunnelling entirely).
const ProtocolVersion = 1

// maxFrameSize bounds a single length-prefixed control frame so a peer cannot
// announce an enormous handshake and exhaust memory.
const maxFrameSize = 16 << 10 // 16 KiB

// Stream-type discriminator. When both ends negotiate stream typing (see
// HelloFrame/HelloAckFrame StreamTyping), the hub writes one of these bytes as
// the first byte of every stream it opens, so the relay can tell client data
// (piped to Xray) from punch-control messages. Old peers never negotiate typing
// and never see the byte, so their raw client-data streams are unchanged.
const (
	StreamTypeData    byte = 0x00 // legacy client traffic → loopback Xray
	StreamTypeControl byte = 0x01 // punch coordination (PunchDirective/PunchAck)
)

var (
	errFrameTooLarge    = errors.New("tunnel control frame exceeds maximum size")
	errProtocolMismatch = errors.New("unsupported tunnel protocol version")
)

// HelloFrame is the first frame a relay sends after the TLS handshake. It
// authenticates the relay and announces the metadata the hub forwards
// to the broker. Fields mirror relay.RegisterRequest.
type HelloFrame struct {
	ProtocolVersion  int    `json:"protocol_version"`
	Token            string `json:"token,omitempty"`
	RealityPublicKey string `json:"reality_public_key"`
	ShortID          string `json:"short_id"`
	ServerName       string `json:"server_name"`
	ClientID         string `json:"client_id"`
	Flow             string `json:"flow"`
	ExitMode         string `json:"exit_mode"`
	MaxSessions      int    `json:"max_sessions"`
	MaxMbps          int    `json:"max_mbps"`
	Label            string `json:"label,omitempty"`
	// RelayVersion retains the legacy volunteer_version JSON key for compatibility
	// with deployed relay runtimes and hubs.
	RelayVersion string `json:"volunteer_version"`
	// StreamTyping announces that this relay understands the stream-type
	// discriminator byte and can handle punch-control streams. Additive: omitted
	// by old relays, which the hub then treats as untyped (data-only).
	StreamTyping bool `json:"stream_typing,omitempty"`
	// PunchCapable requests that the hub advertise this relay as punch-capable to
	// clients. Only meaningful when StreamTyping is also set.
	PunchCapable bool `json:"punch_capable,omitempty"`
}

// HelloAckFrame is the hub's reply to a HelloFrame. On success it carries the
// public endpoint the hub allocated for this relay and the broker relay ID.
type HelloAckFrame struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	PublicHost string `json:"public_host,omitempty"`
	PublicPort int    `json:"public_port,omitempty"`
	RelayID    string `json:"relay_id,omitempty"`
	// StreamTyping confirms the hub will emit the stream-type discriminator byte
	// on the streams it opens. The relay only reads the byte when this is set,
	// so an old hub (which never sets it) drives a new relay in legacy mode.
	StreamTyping bool `json:"stream_typing,omitempty"`
	// ReflectorAddrs are the hub's UDP reflector endpoints, informational for the
	// relay (the authoritative list is also carried in each PunchDirective).
	ReflectorAddrs []string `json:"reflector_addrs,omitempty"`
}

// writeFrame marshals v to JSON and writes it as a 4-byte big-endian
// length-prefixed frame.
func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal tunnel frame: %w", err)
	}
	if len(payload) > maxFrameSize {
		return errFrameTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write tunnel frame header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write tunnel frame payload: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed JSON frame into v.
func readFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxFrameSize {
		return errFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read tunnel frame payload: %w", err)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("decode tunnel frame: %w", err)
	}
	return nil
}
