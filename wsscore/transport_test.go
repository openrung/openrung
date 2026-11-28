// SPDX-License-Identifier: GPL-3.0-or-later

package wsscore

import (
	"context"
	"io"
	"net"
	"testing"
)

func benchTCPPair(b *testing.B) (net.Conn, net.Conn) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	server := <-accepted
	if server == nil {
		b.Fatal("accept failed")
	}
	return dialed, server
}

// BenchmarkCopyOpaqueStream forwards one short stream the way the relay
// sidecar does: a yamux stream spliced to a fresh TCP connection by
// CopyOpaque. B/op is the per-stream garbage.
func BenchmarkCopyOpaqueStream(b *testing.B) {
	clientConn, sidecarConn := benchTCPPair(b)
	client, err := NewClientSession(clientConn)
	if err != nil {
		b.Fatal(err)
	}
	sidecar, err := NewServerSession(sidecarConn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = client.Close()
		_ = sidecar.Close()
	})
	msg := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		upstream, target := benchTCPPair(b)
		go func() {
			_, _ = io.CopyN(target, target, int64(len(msg))) // the Reality listener answering
			_ = target.Close()
		}()
		done := make(chan struct{})
		go func() {
			stream, err := sidecar.Accept()
			if err == nil {
				CopyOpaque(context.Background(), stream, upstream, DefaultStreamIdleTimeout)
			}
			close(done)
		}()
		stream, err := client.Open()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := stream.Write(msg); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(stream, msg); err != nil {
			b.Fatal(err)
		}
		_ = stream.Close()
		<-done
	}
}
