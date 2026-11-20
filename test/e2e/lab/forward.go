// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"io"
	"log"
	"net"
)

// Forward relays every TCP connection accepted on listen to target until the
// listener fails. The upstream connection is dialled first: when the network
// refuses it, the accepted connection is reset, and when the network drops
// it, the accepted connection stalls until the dial gives up.
func Forward(listen, target string) error {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	log.Printf("forwarding %s to %s", listen, target)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go forwardConn(conn.(*net.TCPConn), target)
	}
}

func forwardConn(conn *net.TCPConn, target string) {
	defer conn.Close()
	dialed, err := net.Dial("tcp", target)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		_ = conn.SetLinger(0) // close with a reset
		return
	}
	upstream := dialed.(*net.TCPConn)
	defer upstream.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src *net.TCPConn) {
		_, _ = io.Copy(dst, src)
		_ = dst.CloseWrite()
		done <- struct{}{}
	}
	go pipe(upstream, conn)
	go pipe(conn, upstream)
	<-done
	<-done
}
