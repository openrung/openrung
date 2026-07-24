// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	cloudflareBrokerHost = "broker.openrung.org"

	// Keep the entire ECH phase below desktop discovery's 2.5-second
	// cross-front stagger and the WSS ticket path's 5-second per-front limit.
	// A censor that blackholes ECH therefore reaches the ordinary TLS fallback
	// while the same Cloudflare request still has time to succeed.
	brokerECHTimeout = 2 * time.Second
)

// embeddedCloudflareECHConfigList is a serialized ECHConfigList, including its
// outer uint16 length. It is deliberately compiled into every Go client: never
// replace this with an HTTPS/SVCB lookup, because blocking that DNS bootstrap
// is how the networks this protects disable ECH.
//
// This list was captured from Cloudflare's certificate-authenticated ECH retry
// on 2026-07-24, without an ECH DNS lookup.
//
// Cloudflare authenticates a newer list in the outer TLS handshake when this
// one becomes stale. brokerECHConfigState adopts such a retry list only after
// a connection using it succeeds.
var embeddedCloudflareECHConfigList = []byte{
	0x00, 0x45, 0xfe, 0x0d, 0x00, 0x41, 0x19, 0x00,
	0x20, 0x00, 0x20, 0xe2, 0xaf, 0xd5, 0x98, 0x82,
	0xdc, 0xb5, 0xfd, 0xcd, 0xc8, 0x84, 0x9c, 0x8e,
	0x40, 0x33, 0x7b, 0xff, 0xda, 0xad, 0xca, 0x65,
	0xac, 0x36, 0xcf, 0xbf, 0x38, 0x9c, 0x56, 0xd1,
	0xb6, 0x99, 0x14, 0x00, 0x04, 0x00, 0x01, 0x00,
	0x01, 0x00, 0x12, 0x63, 0x6c, 0x6f, 0x75, 0x64,
	0x66, 0x6c, 0x61, 0x72, 0x65, 0x2d, 0x65, 0x63,
	0x68, 0x2e, 0x63, 0x6f, 0x6d, 0x00, 0x00,
}

type brokerECHConfigState struct {
	mu         sync.RWMutex
	configList []byte
	generation uint64
}

func newBrokerECHConfigState(configList []byte) *brokerECHConfigState {
	return &brokerECHConfigState{configList: bytes.Clone(configList)}
}

func (s *brokerECHConfigState) snapshot() ([]byte, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bytes.Clone(s.configList), s.generation
}

// promote installs a certificate-authenticated retry list after a successful
// ECH retry. The generation check prevents an older concurrent handshake from
// replacing a config that another request refreshed first.
func (s *brokerECHConfigState) promote(oldGeneration uint64, configList []byte) {
	if len(configList) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != oldGeneration {
		return
	}
	s.configList = bytes.Clone(configList)
	s.generation++
}

type brokerECHDialer struct {
	networkDial       func(context.Context, string, string) (net.Conn, error)
	baseTLSConfig     *tls.Config
	state             *brokerECHConfigState
	echTimeout        time.Duration
	tlsHandshakeLimit time.Duration
}

func (d *brokerECHDialer) dialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !isCloudflareBrokerAddress(address) {
		return d.dialTLS(ctx, network, address, host, nil, d.tlsHandshakeLimit)
	}

	configList, generation := d.state.snapshot()
	echCtx, cancelECH := context.WithTimeout(ctx, d.echTimeout)
	conn, echErr := d.dialTLS(echCtx, network, address, host, configList, 0)
	if echErr == nil {
		cancelECH()
		return conn, nil
	}

	var rejection *tls.ECHRejectionError
	if errors.As(echErr, &rejection) && len(rejection.RetryConfigList) != 0 && echCtx.Err() == nil {
		retryList := bytes.Clone(rejection.RetryConfigList)
		conn, retryErr := d.dialTLS(echCtx, network, address, host, retryList, 0)
		if retryErr == nil {
			cancelECH()
			d.state.promote(generation, retryList)
			return conn, nil
		}
	}
	cancelECH()

	// Cancellation means the caller no longer wants this request (for example,
	// another desktop discovery front won). Never leak a plain SNI afterward.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// ECH is an opportunistic privacy layer, not a reachability requirement.
	// Redial from scratch without ECH so networks that drop ECH handshakes keep
	// today's broker behavior and certificate verification.
	return d.dialTLS(ctx, network, address, host, nil, d.tlsHandshakeLimit)
}

func (d *brokerECHDialer) dialTLS(
	ctx context.Context,
	network string,
	address string,
	host string,
	configList []byte,
	handshakeLimit time.Duration,
) (net.Conn, error) {
	plainConn, err := d.networkDial(ctx, network, address)
	if err != nil {
		return nil, err
	}

	config := &tls.Config{}
	if d.baseTLSConfig != nil {
		config = d.baseTLSConfig.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = strings.TrimSuffix(host, ".")
	}
	config.EncryptedClientHelloConfigList = bytes.Clone(configList)
	if len(configList) != 0 && config.MinVersion != 0 && config.MinVersion < tls.VersionTLS13 {
		config.MinVersion = tls.VersionTLS13
	}

	tlsConn := tls.Client(plainConn, config)
	handshakeCtx := ctx
	if handshakeLimit > 0 {
		var cancel context.CancelFunc
		handshakeCtx, cancel = context.WithTimeout(ctx, handshakeLimit)
		defer cancel()
	}
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		_ = plainConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func isCloudflareBrokerAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	return port == "443" && strings.EqualFold(host, cloudflareBrokerHost)
}

func newBrokerTransport(
	base *http.Transport,
	state *brokerECHConfigState,
	echTimeout time.Duration,
) *http.Transport {
	transport := base.Clone()
	dialer := &brokerECHDialer{
		networkDial:       transport.DialContext,
		baseTLSConfig:     transport.TLSClientConfig,
		state:             state,
		echTimeout:        echTimeout,
		tlsHandshakeLimit: transport.TLSHandshakeTimeout,
	}
	transport.DialTLSContext = dialer.dialTLSContext
	return transport
}

var (
	defaultBrokerECHState  = newBrokerECHConfigState(embeddedCloudflareECHConfigList)
	defaultBrokerTransport = newBrokerTransport(
		http.DefaultTransport.(*http.Transport),
		defaultBrokerECHState,
		brokerECHTimeout,
	)
	defaultBrokerHTTPClient = &http.Client{Transport: defaultBrokerTransport}
)

// NewBrokerHTTPClient returns an HTTP client whose direct connections to the
// Cloudflare broker front opportunistically use the embedded ECH config.
// CloudFront, custom brokers, loopback development, and proxy CONNECT paths
// keep the standard transport behavior. All returned clients share connection
// pools and certificate-authenticated ECH retry-config state.
func NewBrokerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: defaultBrokerTransport,
		Timeout:   timeout,
	}
}
