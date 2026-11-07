// SPDX-License-Identifier: GPL-3.0-or-later

//go:build with_utls

package singboxruntime

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	sbtls "github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
)

const (
	groupX25519         = 0x001d
	groupX25519MLKEM768 = 0x11ec
)

// TestRealityClientHelloKeepsX25519MLKEM768 pins the Reality client to the
// uTLS fingerprint's full key-share list: X25519MLKEM768 must be offered and
// must precede X25519. The root and desktop go.mod files replace sing-box with
// a build that keeps it; this fails if that replacement is dropped.
func TestRealityClientHelloKeepsX25519MLKEM768(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	helloCh := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			helloCh <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			helloCh <- nil
			return
		}
		record := make([]byte, binary.BigEndian.Uint16(header[3:5]))
		if _, err := io.ReadFull(conn, record); err != nil {
			helloCh <- nil
			return
		}
		helloCh <- record
	}()

	cfg, err := sbtls.NewRealityClient(context.Background(), logger.NOP(), "127.0.0.1", option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: "www.example.com",
		UTLS:       &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "chrome"},
		Reality: &option.OutboundRealityOptions{
			Enabled:   true,
			PublicKey: "xncM9nbSxEQSfS0zf5whHN62A5ujE7d3onLn78E_6WE",
			ShortID:   "0123456789abcdef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The listener hangs up after reading the hello, so the handshake fails.
	go func() { _, _ = sbtls.ClientHandshake(ctx, conn, cfg) }()

	var record []byte
	select {
	case record = <-helloCh:
	case <-ctx.Done():
	}
	if record == nil {
		t.Fatal("no Client Hello captured")
	}
	groups, shares, err := parseHelloGroups(record)
	if err != nil {
		t.Fatal(err)
	}
	if !precedes(groups, groupX25519MLKEM768, groupX25519) {
		t.Errorf("supported_groups = %#x, want X25519MLKEM768 before X25519", groups)
	}
	if !precedes(shares, groupX25519MLKEM768, groupX25519) {
		t.Errorf("key_share groups = %#x, want X25519MLKEM768 before X25519", shares)
	}
}

func precedes(list []uint16, first, second uint16) bool {
	firstAt, secondAt := -1, -1
	for i, v := range list {
		if v == first && firstAt < 0 {
			firstAt = i
		}
		if v == second && secondAt < 0 {
			secondAt = i
		}
	}
	return firstAt >= 0 && secondAt >= 0 && firstAt < secondAt
}

// parseHelloGroups returns the supported_groups list and the key_share groups
// of a TLS handshake record holding a ClientHello.
func parseHelloGroups(record []byte) (groups, shares []uint16, err error) {
	short := io.ErrUnexpectedEOF
	p := 4 + 2 + 32 // handshake header, legacy version, random
	if len(record) < p+1 {
		return nil, nil, short
	}
	p += 1 + int(record[p]) // session id
	if len(record) < p+2 {
		return nil, nil, short
	}
	p += 2 + int(binary.BigEndian.Uint16(record[p:])) // cipher suites
	if len(record) < p+1 {
		return nil, nil, short
	}
	p += 1 + int(record[p]) // compression methods
	if len(record) < p+2 {
		return nil, nil, short
	}
	end := p + 2 + int(binary.BigEndian.Uint16(record[p:]))
	p += 2
	if end > len(record) {
		return nil, nil, short
	}
	for p+4 <= end {
		typ := binary.BigEndian.Uint16(record[p:])
		n := int(binary.BigEndian.Uint16(record[p+2:]))
		if p+4+n > end {
			return nil, nil, short
		}
		data := record[p+4 : p+4+n]
		p += 4 + n
		switch typ {
		case 10: // supported_groups
			for i := 2; i+2 <= len(data); i += 2 {
				groups = append(groups, binary.BigEndian.Uint16(data[i:]))
			}
		case 51: // key_share
			for i := 2; i+4 <= len(data); {
				shares = append(shares, binary.BigEndian.Uint16(data[i:]))
				i += 4 + int(binary.BigEndian.Uint16(data[i+2:]))
			}
		}
	}
	return groups, shares, nil
}
