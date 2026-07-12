package punchcore

import (
	"encoding/binary"
	"net"
)

// wire codecs -------------------------------------------------------------

func buildReflectRequest(nonce []byte) []byte {
	buf := make([]byte, reflectMinRequest)
	copy(buf, reflectMagicRequest)
	copy(buf[len(reflectMagicRequest):], nonce)
	// remainder stays zero padding to reach the anti-amplification floor
	return buf
}

// parseReflectRequest validates a request datagram and returns its nonce.
func parseReflectRequest(data []byte) ([]byte, bool) {
	if len(data) < reflectMinRequest {
		return nil, false
	}
	if string(data[:len(reflectMagicRequest)]) != reflectMagicRequest {
		return nil, false
	}
	nonce := make([]byte, reflectNonceLen)
	copy(nonce, data[len(reflectMagicRequest):len(reflectMagicRequest)+reflectNonceLen])
	return nonce, true
}

func buildReflectReply(nonce []byte, addr *net.UDPAddr) []byte {
	buf := make([]byte, 0, len(reflectMagicReply)+reflectNonceLen+1+16+2)
	buf = append(buf, reflectMagicReply...)
	buf = append(buf, nonce...)
	if ip4 := addr.IP.To4(); ip4 != nil {
		buf = append(buf, 4)
		buf = append(buf, ip4...)
	} else {
		buf = append(buf, 6)
		buf = append(buf, addr.IP.To16()...)
	}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], uint16(addr.Port))
	buf = append(buf, port[:]...)
	return buf
}

// parseReflectReply validates a reply and returns the echoed nonce and observed
// endpoint.
func parseReflectReply(data []byte) (nonce []byte, observed *net.UDPAddr, ok bool) {
	off := len(reflectMagicReply)
	if len(data) < off+reflectNonceLen+1 {
		return nil, nil, false
	}
	if string(data[:off]) != reflectMagicReply {
		return nil, nil, false
	}
	nonce = make([]byte, reflectNonceLen)
	copy(nonce, data[off:off+reflectNonceLen])
	off += reflectNonceLen
	family := data[off]
	off++
	var ipLen int
	switch family {
	case 4:
		ipLen = 4
	case 6:
		ipLen = 16
	default:
		return nil, nil, false
	}
	if len(data) < off+ipLen+2 {
		return nil, nil, false
	}
	ip := make(net.IP, ipLen)
	copy(ip, data[off:off+ipLen])
	off += ipLen
	port := binary.BigEndian.Uint16(data[off : off+2])
	return nonce, &net.UDPAddr{IP: ip, Port: int(port)}, true
}
