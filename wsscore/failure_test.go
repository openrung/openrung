package wsscore

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	taxonomyDistribution = "d111111abcdef8.cloudfront.net"
	taxonomyCNAME        = "cdn.example.com"
	taxonomyTicket       = "taxonomy-single-use-ticket"
)

// tlsMode selects one of the two production TLS paths. The no-SNI path
// completes TLS inside noSNITLSDialContext and marks the phase booleans there;
// the ordinary-SNI path lets gorilla run TLS and marks the same booleans
// through httptrace. Both cases set NativeFrontNoSNI exactly as the shipped
// clients do, so the recognition rule alone selects the path.
type tlsMode struct {
	name            string
	host            string
	certificateName string
}

var tlsModes = []tlsMode{
	{name: "no SNI", host: taxonomyDistribution, certificateName: "*.cloudfront.net"},
	{name: "ordinary SNI", host: taxonomyCNAME, certificateName: taxonomyCNAME},
}

// dialTaxonomyFailure performs a dial that must fail and returns its error.
func dialTaxonomyFailure(t *testing.T, mode tlsMode, dialer *websocket.Dialer, handshakeTimeout time.Duration) error {
	t.Helper()
	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://" + mode.host + BridgePath, Ticket: taxonomyTicket,
		WebSocketDialer: dialer, NativeFrontNoSNI: true,
		HandshakeTimeout: handshakeTimeout, PingInterval: -1,
	})
	if client != nil {
		_ = client.Close()
		t.Fatal("DialClient established a client for a failing edge")
	}
	if err == nil {
		t.Fatal("DialClient returned no error for a failing edge")
	}
	return err
}

// assertTaxonomyFailure checks the token, the fixed message, and that nothing
// identifying survived into the error a consumer may serialize. Privacy rule 6
// requires this on every failure mode the suite exercises, so every live-wire
// case funnels through here.
func assertTaxonomyFailure(t *testing.T, err error, wantReason, wantMessage, edgeTarget string) {
	t.Helper()
	if reason := FailureReason(err); reason != wantReason {
		t.Fatalf("FailureReason = %q, want %q (error: %v)", reason, wantReason, err)
	}
	if err.Error() != wantMessage {
		t.Fatalf("error message = %q, want %q", err.Error(), wantMessage)
	}
	assertNoIdentifyingLeak(t, err.Error(), edgeTarget)
}

// assertNoIdentifyingLeak fails when text carries anything a censor-side
// observer or a telemetry reader must never receive: the front hostname, the
// CDN vendor name, an address literal, or ticket bytes.
func assertNoIdentifyingLeak(t *testing.T, text, edgeTarget string) {
	t.Helper()
	forbidden := []string{
		taxonomyDistribution, taxonomyCNAME, "cloudfront", "example",
		taxonomyTicket, TicketBearerPrefix, "127.0.0.1", "::1", "10.10.34",
	}
	if edgeTarget != "" {
		host, port, err := net.SplitHostPort(edgeTarget)
		if err == nil {
			forbidden = append(forbidden, edgeTarget, host, port)
		} else {
			forbidden = append(forbidden, edgeTarget)
		}
	}
	lowered := strings.ToLower(text)
	for _, needle := range forbidden {
		if needle == "" {
			continue
		}
		if strings.Contains(lowered, strings.ToLower(needle)) {
			t.Fatalf("failure text %q leaked %q", text, needle)
		}
	}
}

// newRawEdge answers each accepted connection with handle, so a case can reply
// to the client's dial with non-TLS bytes, an abrupt close, or its own TLS
// server configuration.
func newRawEdge(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}

// mappedDialer sends every dial to target while leaving the signed hostname to
// drive SNI selection and certificate verification.
func mappedDialer(target string, roots *x509.CertPool) *websocket.Dialer {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if roots != nil {
		config.RootCAs = roots
	}
	return &websocket.Dialer{
		TLSClientConfig: config,
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	}
}

// newObservedHTTPEdge starts a TLS edge that records the Authorization value of
// every request it receives, so a case can assert that no HTTP request — and
// therefore no ticket — was ever sent.
func newObservedHTTPEdge(t *testing.T, certificateName string, handler http.Handler) (*websocket.Dialer, string, <-chan string) {
	t.Helper()
	certificate, roots := newTestServerCertificate(t, certificateName)
	requests := make(chan string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Header.Get(TicketAuthorizationHeader):
		default:
		}
		handler.ServeHTTP(w, r)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	target := server.Listener.Addr().String()
	return mappedDialer(target, roots), target, requests
}

func TestClassifyConnectionRefused(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closedTarget := listener.Addr().String()
			_ = listener.Close()

			failure := dialTaxonomyFailure(t, mode, mappedDialer(closedTarget, nil), time.Second)
			assertTaxonomyFailure(t, failure, ReasonConnectionRefused,
				"WSS handshake failed ("+ReasonConnectionRefused+")", closedTarget)
		})
	}
}

func TestClassifyHungDialAsTCPTimeout(t *testing.T) {
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// A blackholed SYN: the censor never answers and the handshake
			// deadline expires before any TCP connection exists.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	failure := dialTaxonomyFailure(t, tlsModes[0], dialer, 150*time.Millisecond)
	assertTaxonomyFailure(t, failure, ReasonTCPTimeout,
		"WSS handshake failed ("+ReasonTCPTimeout+")", "")
}

func TestClassifyMidTLSCloseAsTLSReset(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			target := newRawEdge(t, func(conn net.Conn) {
				// Wait for the ClientHello, then tear the connection down
				// without answering: the client is mid-TLS either way.
				_, _ = conn.Read(make([]byte, 1))
			})
			failure := dialTaxonomyFailure(t, mode, mappedDialer(target, nil), 2*time.Second)
			assertTaxonomyFailure(t, failure, ReasonTLSReset,
				"WSS handshake failed ("+ReasonTLSReset+")", target)
		})
	}
}

func TestClassifyPlaintextResponseAsNotTLS(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			target := newRawEdge(t, func(conn net.Conn) {
				// An injected blockpage on :443 — never benign toward a CDN.
				_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
				_, _ = conn.Read(make([]byte, 1))
			})
			failure := dialTaxonomyFailure(t, mode, mappedDialer(target, nil), 2*time.Second)
			assertTaxonomyFailure(t, failure, ReasonTLSNotTLS,
				"WSS handshake failed ("+ReasonTLSNotTLS+")", target)
		})
	}
}

func TestClassifyCertificateFailures(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			for _, test := range []struct {
				name             string
				certificateName  string
				discardTrustRoot bool
				clientTime       time.Time
				wantReason       string
			}{
				{
					name: "wrong hostname", certificateName: "other.invalid",
					wantReason: ReasonCertVerify,
				},
				{
					name: "untrusted root", certificateName: mode.certificateName,
					discardTrustRoot: true, wantReason: ReasonCertVerify,
				},
				{
					name: "expired", certificateName: mode.certificateName,
					clientTime: time.Now().Add(2 * time.Hour), wantReason: ReasonCertExpired,
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					dialer, target, requests := newObservedHTTPEdge(t, test.certificateName,
						http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
					if test.discardTrustRoot {
						dialer.TLSClientConfig.RootCAs = x509.NewCertPool()
					}
					if !test.clientTime.IsZero() {
						dialer.TLSClientConfig.Time = func() time.Time { return test.clientTime }
					}

					failure := dialTaxonomyFailure(t, mode, dialer, 2*time.Second)
					assertTaxonomyFailure(t, failure, test.wantReason,
						"WSS handshake failed ("+test.wantReason+")", target)
					// The load-bearing ticket invariant: verification fails
					// before any HTTP request reaches the peer, so no ticket
					// bytes can ever be handed to a hostile endpoint.
					select {
					case authorization := <-requests:
						t.Fatalf("an HTTP request carrying %q was sent before certificate verification", authorization)
					default:
					}
				})
			}
		})
	}
}

func TestClassifyRemoteTLSAlert(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			certificate, roots := newTestServerCertificate(t, mode.certificateName)
			target := newRawEdge(t, func(conn net.Conn) {
				// A TLS-terminating middlebox that refuses the client's
				// version: crypto/tls answers with a fatal remote alert.
				server := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{certificate},
					MinVersion:   tls.VersionTLS12,
					MaxVersion:   tls.VersionTLS12,
				})
				_ = server.HandshakeContext(context.Background())
			})
			dialer := mappedDialer(target, roots)
			dialer.TLSClientConfig.MinVersion = tls.VersionTLS13

			failure := dialTaxonomyFailure(t, mode, dialer, 2*time.Second)
			assertTaxonomyFailure(t, failure, ReasonTLSAlert,
				"WSS handshake failed ("+ReasonTLSAlert+")", target)
		})
	}
}

func TestClassifyEdgeHTTPStatuses(t *testing.T) {
	for _, test := range []struct {
		status     int
		wantReason string
	}{
		{status: http.StatusUnauthorized, wantReason: ReasonHTTP401},
		{status: http.StatusForbidden, wantReason: ReasonHTTP403},
		{status: http.StatusMisdirectedRequest, wantReason: ReasonHTTP421},
		{status: http.StatusTooManyRequests, wantReason: ReasonRateLimited},
		{status: http.StatusBadGateway, wantReason: ReasonHTTP502},
		{status: http.StatusServiceUnavailable, wantReason: ReasonHTTP503},
		{status: http.StatusInternalServerError, wantReason: ReasonHTTPOther},
		{status: http.StatusTeapot, wantReason: ReasonHTTPOther},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			mode := tlsModes[0]
			dialer, target, requests := newObservedHTTPEdge(t, mode.certificateName,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					// A CloudFront error response carries an X-Amz-Cf-Id
					// correlation identifier and a body; neither may survive.
					w.Header().Set("X-Amz-Cf-Id", "correlation-identifier-"+taxonomyDistribution)
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte("edge refused " + taxonomyDistribution))
				}))

			failure := dialTaxonomyFailure(t, mode, dialer, 2*time.Second)
			assertTaxonomyFailure(t, failure, test.wantReason,
				"WSS handshake failed ("+test.wantReason+")", target)
			if authorization := <-requests; authorization != TicketBearerPrefix+taxonomyTicket {
				t.Fatalf("edge received Authorization %q, want the bearer ticket", authorization)
			}
		})
	}
}

// hijackWebSocketResponse answers the upgrade by hand so a case can return a
// 101 that fails gorilla's own header validation, or one that omits the
// required subprotocol.
func hijackWebSocketResponse(t *testing.T, acceptKey bool, subprotocol string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		accept := "not-a-valid-accept-key-value-000000000000="
		if acceptKey {
			sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			accept = base64.StdEncoding.EncodeToString(sum[:])
		}
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n"
		if subprotocol != "" {
			response += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
		}
		_, _ = buffered.WriteString(response + "\r\n")
		_ = buffered.Flush()
		// Hold the hijacked connection open until the client releases it.
		_, _ = conn.Read(make([]byte, 1))
	})
}

func TestClassifyWebSocketUpgradeRejection(t *testing.T) {
	mode := tlsModes[0]
	dialer, target, _ := newObservedHTTPEdge(t, mode.certificateName,
		hijackWebSocketResponse(t, false, Subprotocol))
	failure := dialTaxonomyFailure(t, mode, dialer, 2*time.Second)
	assertTaxonomyFailure(t, failure, ReasonWSUpgrade,
		"WSS handshake failed ("+ReasonWSUpgrade+")", target)
}

func TestClassifyMissingSubprotocolKeepsPinnedMessage(t *testing.T) {
	mode := tlsModes[0]
	dialer, target, _ := newObservedHTTPEdge(t, mode.certificateName,
		hijackWebSocketResponse(t, true, ""))
	failure := dialTaxonomyFailure(t, mode, dialer, 2*time.Second)
	assertTaxonomyFailure(t, failure, ReasonWSSubprotocol,
		"WSS subprotocol was not negotiated", target)
}

func TestClassifySilentEdgeAsResponseTimeout(t *testing.T) {
	for _, mode := range tlsModes {
		t.Run(mode.name, func(t *testing.T) {
			release := make(chan struct{})
			dialer, target, requests := newObservedHTTPEdge(t, mode.certificateName,
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			// Cleanup is LIFO: release the blocked handler before the server
			// close registered by newObservedHTTPEdge waits for it.
			t.Cleanup(func() { close(release) })

			failure := dialTaxonomyFailure(t, mode, dialer, 400*time.Millisecond)
			assertTaxonomyFailure(t, failure, ReasonResponseTimeout,
				"WSS handshake failed ("+ReasonResponseTimeout+")", target)
			if authorization := <-requests; authorization != TicketBearerPrefix+taxonomyTicket {
				t.Fatalf("edge received Authorization %q, want the bearer ticket", authorization)
			}
		})
	}
}

func TestClassifyStalledResponseAsHandshakeTimeout(t *testing.T) {
	// The edge starts answering and then stops. The first response byte arrived,
	// so this is the residual timeout bucket rather than response_timeout — which
	// is the only thing that distinguishes "handshake admitted, data killed" from
	// "edge went quiet mid-answer".
	mode := tlsModes[0]
	dialer, target, _ := newObservedHTTPEdge(t, mode.certificateName,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, buffered, err := hijacker.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
			_ = buffered.Flush()
			_, _ = conn.Read(make([]byte, 1))
		}))

	failure := dialTaxonomyFailure(t, mode, dialer, 400*time.Millisecond)
	assertTaxonomyFailure(t, failure, ReasonHandshakeTimeout,
		"WSS handshake failed ("+ReasonHandshakeTimeout+")", target)
}

func TestClassifyCancellationAndPreserveErrorsIs(t *testing.T) {
	dialing := make(chan struct{})
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialing)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	failures := make(chan error, 1)
	go func() {
		client, err := DialClient(ctx, ClientOptions{
			URL: "wss://" + taxonomyDistribution + BridgePath, Ticket: taxonomyTicket,
			WebSocketDialer: dialer, NativeFrontNoSNI: true, PingInterval: -1,
		})
		if client != nil {
			_ = client.Close()
		}
		failures <- err
	}()
	<-dialing
	cancel()

	var failure error
	select {
	case failure = <-failures:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled dial did not return")
	}
	if failure == nil {
		t.Fatal("cancelled dial returned no error")
	}
	assertTaxonomyFailure(t, failure, ReasonCancelled,
		"WSS handshake failed ("+ReasonCancelled+")", "")
	if !errors.Is(failure, context.Canceled) {
		t.Fatal("a cancelled dial no longer satisfies errors.Is(err, context.Canceled)")
	}
	if errors.Is(failure, context.DeadlineExceeded) {
		t.Fatal("a cancelled dial must not match unrelated sentinels")
	}
}

func TestSocketProtectionSentinelBypassesTheTaxonomy(t *testing.T) {
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			// wsscore strips the OpError that would carry the dialed edge
			// address, so the sentinel arrives bare.
			return nil, ErrSocketProtectionFailed
		},
	}
	failure := dialTaxonomyFailure(t, tlsModes[0], dialer, time.Second)
	if !errors.Is(failure, ErrSocketProtectionFailed) {
		t.Fatalf("dial error = %v, want ErrSocketProtectionFailed", failure)
	}
	if reason := FailureReason(failure); reason != "" {
		t.Fatalf("FailureReason(sentinel) = %q, want \"\"", reason)
	}
	var dialErr *DialError
	if errors.As(failure, &dialErr) {
		t.Fatal("the socket-protection sentinel was replaced by a DialError")
	}
	assertNoIdentifyingLeak(t, failure.Error(), "")
}

func TestFailureReasonIgnoresForeignErrors(t *testing.T) {
	if reason := FailureReason(errors.New("x")); reason != "" {
		t.Fatalf("FailureReason(foreign) = %q, want \"\"", reason)
	}
	if reason := FailureReason(nil); reason != "" {
		t.Fatalf("FailureReason(nil) = %q, want \"\"", reason)
	}
	wrapped := fmt.Errorf("outer: %w", newDialError(ReasonTLSTimeout))
	if reason := FailureReason(wrapped); reason != ReasonTLSTimeout {
		t.Fatalf("FailureReason(wrapped) = %q, want %q", reason, ReasonTLSTimeout)
	}
	// No Unwrap: a DialError can never be used to reach an underlying error.
	if _, ok := any(newDialError(ReasonTLSTimeout)).(interface{ Unwrap() error }); ok {
		t.Fatal("DialError exposes Unwrap, which would re-expose the destroyed error chain")
	}
}

// TestDestroyedErrorTextNeverLeaks drives failures whose underlying error text
// deliberately embeds the hostname, an injected address, and the ticket, and
// asserts the classifier's output carries none of it.
func TestDestroyedErrorTextNeverLeaks(t *testing.T) {
	for _, test := range []struct {
		name       string
		dialErr    error
		wantReason string
	}{
		{
			name: "resolver failure naming the front",
			dialErr: &net.DNSError{
				Err: "no such host", Name: taxonomyDistribution, IsNotFound: true,
			},
			wantReason: ReasonDNSFailure,
		},
		{
			name: "opaque error carrying credentials and addresses",
			dialErr: fmt.Errorf("dial tcp 10.10.34.36:443 -> %s with %s%s failed",
				taxonomyDistribution, TicketBearerPrefix, taxonomyTicket),
			wantReason: ReasonUnclassified,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dialer := &websocket.Dialer{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				NetDialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, test.dialErr
				},
			}
			failure := dialTaxonomyFailure(t, tlsModes[0], dialer, time.Second)
			assertTaxonomyFailure(t, failure, test.wantReason,
				"WSS handshake failed ("+test.wantReason+")", "")
		})
	}
}

type phaseMarks struct {
	tcpConnected, tlsStarted, tlsDone, gotFirstResponseByte, bogonAddress bool
}

func (m phaseMarks) recorder() *dialPhases {
	phases := &dialPhases{}
	phases.tcpConnected.Store(m.tcpConnected)
	phases.tlsStarted.Store(m.tlsStarted)
	phases.tlsDone.Store(m.tlsDone)
	phases.gotFirstResponseByte.Store(m.gotFirstResponseByte)
	phases.bogonAddress.Store(m.bogonAddress)
	return phases
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "fake i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return false }

// falseTimeoutWrapper implements net.Error with Timeout() == false while
// wrapping another error — the shape a custom transport wrapper can produce.
// errors.As binds it before anything it wraps, so it shadows a wrapped errno's
// own Timeout answer; the classifier's explicit ETIMEDOUT match is what keeps
// the timeout token.
type falseTimeoutWrapper struct{ err error }

func (w falseTimeoutWrapper) Error() string   { return "wrapped: " + w.err.Error() }
func (w falseTimeoutWrapper) Timeout() bool   { return false }
func (w falseTimeoutWrapper) Temporary() bool { return false }
func (w falseTimeoutWrapper) Unwrap() error   { return w.err }

// TestClassifyDialFailurePrecedence locks the deterministic precedence of the
// frozen taxonomy, including the phase-dependent splits that a live-wire case
// cannot provoke reliably.
func TestClassifyDialFailurePrecedence(t *testing.T) {
	tcpOnly := phaseMarks{tcpConnected: true}
	midTLS := phaseMarks{tcpConnected: true, tlsStarted: true}
	afterTLS := phaseMarks{tcpConnected: true, tlsStarted: true, tlsDone: true}
	responding := phaseMarks{
		tcpConnected: true, tlsStarted: true, tlsDone: true, gotFirstResponseByte: true,
	}

	for _, test := range []struct {
		name       string
		err        error
		resp       *http.Response
		marks      phaseMarks
		wantReason string
	}{
		{
			name: "a response outranks every network signal",
			err:  websocket.ErrBadHandshake, resp: &http.Response{StatusCode: http.StatusForbidden},
			marks: phaseMarks{tcpConnected: true, bogonAddress: true}, wantReason: ReasonHTTP403,
		},
		{
			name: "a poisoned answer outranks the doomed connection's error",
			err:  syscall.ECONNREFUSED, marks: phaseMarks{bogonAddress: true},
			wantReason: ReasonDNSBogon,
		},
		{
			name: "cancellation", err: context.Canceled, marks: midTLS,
			wantReason: ReasonCancelled,
		},
		{
			name: "resolver failure", err: &net.DNSError{Err: "no such host"},
			wantReason: ReasonDNSFailure,
		},
		{
			name: "refused", err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
			wantReason: ReasonConnectionRefused,
		},
		{
			name: "network unreachable", err: syscall.ENETUNREACH,
			wantReason: ReasonNetworkUnreachable,
		},
		{
			name: "host unreachable", err: syscall.EHOSTUNREACH,
			wantReason: ReasonNetworkUnreachable,
		},
		{
			name: "reset before TLS", err: syscall.ECONNRESET, marks: tcpOnly,
			wantReason: ReasonConnectionReset,
		},
		{
			name: "reset during TLS", err: syscall.ECONNRESET, marks: midTLS,
			wantReason: ReasonTLSReset,
		},
		{
			name: "reset after TLS", err: syscall.ECONNRESET, marks: afterTLS,
			wantReason: ReasonResponseReset,
		},
		{
			name: "non-TLS bytes", err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			marks: midTLS, wantReason: ReasonTLSNotTLS,
		},
		{
			name: "expired certificate",
			err: &tls.CertificateVerificationError{
				Err: x509.CertificateInvalidError{Reason: x509.Expired},
			},
			marks: midTLS, wantReason: ReasonCertExpired,
		},
		{
			name:  "certificate not yet valid",
			err:   x509.CertificateInvalidError{Reason: x509.TooManyIntermediates},
			marks: midTLS, wantReason: ReasonCertVerify,
		},
		{
			name: "untrusted chain",
			err: &tls.CertificateVerificationError{
				Err: x509.UnknownAuthorityError{},
			},
			marks: midTLS, wantReason: ReasonCertVerify,
		},
		{
			name: "wrong hostname", err: x509.HostnameError{Host: taxonomyDistribution},
			marks: midTLS, wantReason: ReasonCertVerify,
		},
		{
			name: "remote alert", err: &net.OpError{Op: "remote error", Err: errors.New("tls: protocol version not supported")},
			marks: midTLS, wantReason: ReasonTLSAlert,
		},
		{
			name: "timeout before TCP", err: fakeTimeout{}, wantReason: ReasonTCPTimeout,
		},
		{
			name: "deadline before TCP", err: context.DeadlineExceeded, wantReason: ReasonTCPTimeout,
		},
		{
			name: "timeout during TLS", err: fakeTimeout{}, marks: midTLS,
			wantReason: ReasonTLSTimeout,
		},
		{
			name: "timeout awaiting the response", err: fakeTimeout{}, marks: afterTLS,
			wantReason: ReasonResponseTimeout,
		},
		{
			name: "timeout mid-response", err: fakeTimeout{}, marks: responding,
			wantReason: ReasonHandshakeTimeout,
		},
		{
			name: "unexpected EOF during TLS", err: io.ErrUnexpectedEOF, marks: midTLS,
			wantReason: ReasonTLSReset,
		},
		{
			name: "EOF awaiting the response", err: io.EOF, marks: afterTLS,
			wantReason: ReasonResponseReset,
		},
		{
			name: "broken pipe before TLS", err: syscall.EPIPE, marks: tcpOnly,
			wantReason: ReasonConnectionReset,
		},
		{
			name:       "ETIMEDOUT behind a wrapper whose Timeout reports false",
			err:        falseTimeoutWrapper{err: syscall.ETIMEDOUT},
			wantReason: ReasonTCPTimeout,
		},
		// The Winsock rows below prove the Windows mappings on whatever host
		// runs the suite: the raw numbers from the shared table (winsock.go)
		// are matched unconditionally, so no Windows runner is needed.
		{
			name: "winsock reset during TLS", err: WSAECONNRESET, marks: midTLS,
			wantReason: ReasonTLSReset,
		},
		{
			name: "winsock connection aborted before TLS", err: WSAECONNABORTED, marks: tcpOnly,
			wantReason: ReasonConnectionReset,
		},
		{
			name: "winsock timeout awaiting the response", err: WSAETIMEDOUT, marks: afterTLS,
			wantReason: ReasonResponseTimeout,
		},
		{
			name: "opaque TLS residual", err: errors.New("tls: bad record MAC"), marks: midTLS,
			wantReason: ReasonTLSHandshake,
		},
		{
			name: "opaque residual", err: errors.New("something else"), marks: tcpOnly,
			wantReason: ReasonUnclassified,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason := classifyDialFailure(t.Context(), test.err, test.resp, test.marks.recorder())
			if reason != test.wantReason {
				t.Fatalf("classifyDialFailure = %q, want %q", reason, test.wantReason)
			}
			assertNoIdentifyingLeak(t, newDialError(reason).Error(), "")
		})
	}
}

func TestClassifyHTTPStatusCoversTheFixedList(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusSwitchingProtocols: ReasonWSUpgrade,
		http.StatusUnauthorized:       ReasonHTTP401,
		http.StatusForbidden:          ReasonHTTP403,
		http.StatusMisdirectedRequest: ReasonHTTP421,
		http.StatusTooManyRequests:    ReasonRateLimited,
		http.StatusBadGateway:         ReasonHTTP502,
		http.StatusServiceUnavailable: ReasonHTTP503,
		http.StatusOK:                 ReasonHTTPOther,
		http.StatusNotFound:           ReasonHTTPOther,
		599:                           ReasonHTTPOther,
	} {
		if reason := classifyHTTPStatus(status); reason != want {
			t.Errorf("classifyHTTPStatus(%d) = %q, want %q", status, reason, want)
		}
	}
}

func TestIsBogonAddress(t *testing.T) {
	for address, want := range map[string]bool{
		// Iran's classic injected answers, and the rest of reserved space.
		"10.10.34.36:443":       true,
		"10.0.0.1:443":          true,
		"172.16.0.1:443":        true,
		"192.168.1.1:443":       true,
		"127.0.0.1:443":         true,
		"169.254.1.1:443":       true,
		"100.64.0.1:443":        true,
		"0.1.2.3:443":           true,
		"240.0.0.1:443":         true,
		"[::1]:443":             true,
		"[fc00::1]:443":         true,
		"[fe80::1]:443":         true,
		"[::ffff:10.1.1.1]:443": true,
		// Routable space, including the documentation ranges a test edge may
		// legitimately use.
		"13.35.1.1:443":      false,
		"1.1.1.1:443":        false,
		"192.0.2.1:443":      false,
		"198.51.100.1:443":   false,
		"[2606:4700::1]:443": false,
		// Malformed input must never be treated as an injection signature.
		"":                 false,
		"not-an-address":   false,
		"edge.example:443": false,
	} {
		if got := isBogonAddress(address); got != want {
			t.Errorf("isBogonAddress(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestBogonControlPreservesProtectorSemantics(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	t.Run("observes the resolved address without a protector", func(t *testing.T) {
		phases := &dialPhases{}
		conn, err := newNetworkDialer(time.Second, nil, phases, nil).DialContext(t.Context(), "tcp", listener.Addr().String())
		if err != nil {
			t.Fatalf("unprotected dial: %v", err)
		}
		_ = conn.Close()
		if !phases.bogonAddress.Load() {
			t.Fatal("a dial directed into reserved address space was not flagged")
		}
	})

	t.Run("keeps the protector veto and its bare sentinel", func(t *testing.T) {
		phases := &dialPhases{}
		protector := &recordingProtector{allow: false}
		conn, err := newNetworkDialer(time.Second, protector, phases, nil).DialContext(t.Context(), "tcp", listener.Addr().String())
		if conn != nil {
			_ = conn.Close()
			t.Fatal("socket connected after protection was denied")
		}
		if !errors.Is(err, ErrSocketProtectionFailed) {
			t.Fatalf("dial error = %v, want ErrSocketProtectionFailed", err)
		}
		if !phases.bogonAddress.Load() {
			t.Fatal("the address observation was skipped when protection failed")
		}
		protector.mu.Lock()
		defer protector.mu.Unlock()
		if len(protector.fds) != 1 {
			t.Fatalf("VpnService protector descriptors = %v, want exactly one", protector.fds)
		}
	})

	t.Run("keeps a panicking protector fatal to the dial", func(t *testing.T) {
		phases := &dialPhases{}
		control := bogonAwareControl(phases, func(context.Context, string, string, syscall.RawConn) error {
			panic("protector exploded")
		})
		panicked := func() (panicked bool) {
			defer func() { panicked = recover() != nil }()
			_ = control(t.Context(), "tcp", "10.10.34.36:443", &fakeRawConn{fd: 7})
			return false
		}()
		if !panicked {
			t.Fatal("the bogon wrapper swallowed a protector panic, letting an unprotected dial proceed")
		}
		if !phases.bogonAddress.Load() {
			t.Fatal("the address observation must happen before delegation")
		}
	})

	t.Run("flags a poisoned dial as DNS injection", func(t *testing.T) {
		phases := &dialPhases{}
		if err := bogonAwareControl(phases, nil)(t.Context(), "tcp", "10.10.34.36:443", &fakeRawConn{fd: 9}); err != nil {
			t.Fatalf("observation-only control returned %v", err)
		}
		if reason := classifyDialFailure(t.Context(), syscall.ETIMEDOUT, nil, phases); reason != ReasonDNSBogon {
			t.Fatalf("poisoned dial classified as %q, want %q", reason, ReasonDNSBogon)
		}
	})
}
