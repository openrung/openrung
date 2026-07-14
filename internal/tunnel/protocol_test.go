package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := HelloFrame{
		ProtocolVersion:  ProtocolVersion,
		Token:            "secret",
		RealityPublicKey: "pk",
		ShortID:          "sid",
		ServerName:       "www.example.com",
		ClientID:         "cid",
		Flow:             "xtls-rprx-vision",
		ExitMode:         "direct",
		MaxSessions:      4,
		MaxMbps:          10,
		Label:            "lbl",
		RelayVersion:     "dev",
	}
	if err := writeFrame(&buf, in); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"volunteer_version":"dev"`)) {
		t.Fatalf("legacy volunteer_version wire key missing from %q", buf.Bytes())
	}
	if bytes.Contains(buf.Bytes(), []byte(`"relay_version"`)) {
		t.Fatalf("unexpected relay_version wire key in %q", buf.Bytes())
	}
	var out HelloFrame
	if err := readFrame(&buf, &out); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	frame := HelloFrame{Label: strings.Repeat("a", maxFrameSize+1)}
	if err := writeFrame(&buf, frame); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("expected errFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected nothing written, got %d bytes", buf.Len())
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxFrameSize+1)
	if err := readFrame(bytes.NewReader(header[:]), &HelloFrame{}); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("expected errFrameTooLarge, got %v", err)
	}
}
