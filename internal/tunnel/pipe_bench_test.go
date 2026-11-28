// SPDX-License-Identifier: GPL-3.0-or-later

package tunnel

import (
	"io"
	"net"
	"testing"

	"github.com/hashicorp/yamux"
)

func benchTCPPair(b *testing.B) (net.Conn, net.Conn) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); ch <- c }()
	d, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	return d, <-ch
}

// BenchmarkHubConnection forwards one short client connection through
// pipe(clientConn, stream), as the hub does; B/op is the per-connection
// garbage. The relay side echoes through copyHalf so only tunnel code counts.
func BenchmarkHubConnection(b *testing.B) {
	c1, c2 := benchTCPPair(b)
	hub, _ := yamux.Server(c1, yamuxConfig())
	relaySide, _ := yamux.Client(c2, yamuxConfig())
	go func() {
		for {
			s, err := relaySide.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = copyHalf(s, s); _ = s.Close() }() // echo, like a relay answering
		}
	}()
	msg := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		client, hubSide := benchTCPPair(b)
		stream, err := hub.Open()
		if err != nil {
			b.Fatal(err)
		}
		done := make(chan struct{})
		go func() { pipe(hubSide, stream); close(done) }()
		_, _ = client.Write(msg)
		_, _ = io.ReadFull(client, msg)
		_ = client.Close()
		<-done
	}
}
