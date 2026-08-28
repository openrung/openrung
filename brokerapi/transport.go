// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// The ECH attempt and one authenticated retry stay below discovery's
	// 2.5-second cross-front stagger. A censor that drops ECH therefore reaches
	// ordinary TLS quickly enough to preserve current reachability.
	brokerECHTimeout = 2 * time.Second
)

// embeddedCloudflareECHConfigList is a serialized ECHConfigList, including its
// outer uint16 length. It is deliberately compiled into the module. Never
// replace it with an HTTPS/SVCB lookup: blocking that DNS bootstrap is the
// censorship technique this protects against.
//
// Captured from Cloudflare's certificate-authenticated ECH retry on
// 2026-07-24 without an ECH DNS lookup. Successful authenticated retry
// handshakes promote newer server-provided configs in memory.
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

type echConfigState struct {
	mu         sync.RWMutex
	configList []byte
	generation uint64
}

func newECHConfigState(configList []byte) *echConfigState {
	return &echConfigState{configList: bytes.Clone(configList)}
}

func (s *echConfigState) snapshot() ([]byte, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bytes.Clone(s.configList), s.generation
}

// promote accepts only a retry list whose ECH handshake and ordinary
// certificate verification both succeeded. The generation check prevents an
// older concurrent handshake from replacing a newer config.
func (s *echConfigState) promote(oldGeneration uint64, configList []byte) {
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

type echDialer struct {
	networkDial       func(context.Context, string, string) (net.Conn, error)
	baseTLSConfig     *tls.Config
	state             *echConfigState
	echTimeout        time.Duration
	tlsHandshakeLimit time.Duration
}

func (d *echDialer) dialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// Neither of these fronts can receive this deployment's ECH config, but both
	// serve a default certificate to a ClientHello that carries no server name
	// and select the endpoint from the encrypted HTTP Host header instead. They
	// differ in what that certificate proves: see cloudFrontVerification and
	// azureFrontDoorVerification.
	if distribution, native := cloudFrontDistributionAddress(address); native {
		return d.dialNoSNITLS(ctx, network, address, cloudFrontVerification(distribution))
	}
	if azureFrontDoorAddress(address) {
		return d.dialNoSNITLS(ctx, network, address, azureFrontDoorVerification())
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

	// If another discovery front won, never perform a late plain-SNI retry.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// ECH is opportunistic. Redial from scratch without it for networks that
	// drop ECH, preserving ordinary TLS hostname and certificate verification.
	return d.dialTLS(ctx, network, address, host, nil, d.tlsHandshakeLimit)
}

func (d *echDialer) dialTLS(
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
	return handshake(ctx, plainConn, config, handshakeLimit)
}

// handshake owns the one TLS client handshake shape shared by the ECH, plain,
// and no-SNI dials: the connection is closed on failure so a caller never has
// to unwind a half-open socket.
func handshake(
	ctx context.Context,
	plainConn net.Conn,
	config *tls.Config,
	handshakeLimit time.Duration,
) (net.Conn, error) {
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

func newTransport(base *http.Transport, state *echConfigState, echTimeout time.Duration) *http.Transport {
	transport := base.Clone()
	dialer := &echDialer{
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
	defaultECHState  = newECHConfigState(embeddedCloudflareECHConfigList)
	defaultTransport = newTransport(
		http.DefaultTransport.(*http.Transport),
		defaultECHState,
		brokerECHTimeout,
	)
)

// NewHTTPClient returns an HTTP client that keeps a built-in front's hostname
// out of the ClientHello on direct connections: the Cloudflare front
// opportunistically uses the embedded ECH config and falls back to ordinary
// TLS, which does send the name, while a native CloudFront distribution and an
// Azure Front Door endpoint omit SNI unconditionally. Custom brokers, loopback,
// and proxy CONNECT paths retain standard TLS. Returned clients share
// connection pools and authenticated retry-config state.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     defaultTransport,
		Timeout:       timeout,
		CheckRedirect: refuseRedirect,
	}
}

// NewHTTPClientWithDialControl is NewHTTPClient over a base dialer whose
// Control is the supplied hook, for hosts whose own sockets must be excluded
// from a device-wide tunnel (Android's VpnService.protect: the broker request
// that discovers relays must not be captured into the tunnel it is trying to
// establish). The ECH and no-SNI dial behavior is identical — the hook runs on
// the underlying TCP socket before any TLS — and the returned client shares
// the package's authenticated ECH retry-config state, but owns its own
// connection pool. A control error fails the dial: an unprotected socket must
// never fall back to the captured path. A nil control returns NewHTTPClient.
func NewHTTPClientWithDialControl(
	timeout time.Duration,
	control func(network, address string, conn syscall.RawConn) error,
) *http.Client {
	if control == nil {
		return NewHTTPClient(timeout)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	// Mirror http.DefaultTransport's dialer shape; only Control is added.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   control,
	}
	base.DialContext = dialer.DialContext
	return &http.Client{
		Transport:     newTransport(base, defaultECHState, brokerECHTimeout),
		Timeout:       timeout,
		CheckRedirect: refuseRedirect,
	}
}
