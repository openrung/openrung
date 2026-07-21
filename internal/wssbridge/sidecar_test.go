package wssbridge

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testOriginOld = "old-origin-token-that-is-at-least-32-bytes"
	testOriginNew = "new-origin-token-that-is-at-least-32-bytes"
	testOriginB   = "front-b-origin-token-at-least-32-bytes"
)

type sidecarFixture struct {
	signer   *TicketSigner
	verifier *TicketVerifier
	stats    *SidecarStats
	handler  http.Handler
	now      time.Time
	nextJTI  atomic.Int64
}

type blockingReplayStore struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (s *blockingReplayStore) Consume(ctx context.Context, _ string, _ time.Time) (bool, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func newSidecarFixture(t *testing.T, mutate func(*SidecarOptions)) *sidecarFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	signer, key := testSigner(t, now)
	verifier, err := NewTicketVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): key.Public().(ed25519.PublicKey),
	}, "relay-a", TicketOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats := &SidecarStats{}
	opts := SidecarOptions{
		RelayID: "relay-a", Verifier: verifier, Stats: stats,
		ReplayStore: NewMemoryReplayStore(100_000),
		FrontOriginTokens: map[string][]string{
			"front-a": {testOriginOld, testOriginNew},
			"front-b": {testOriginB},
		},
		PingInterval: -1,
	}
	if mutate != nil {
		mutate(&opts)
	}
	handler, err := NewSidecarHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &sidecarFixture{signer: signer, verifier: verifier, stats: stats, handler: handler, now: now}
}

func (f *sidecarFixture) ticket(t *testing.T, relayID, frontID string) string {
	t.Helper()
	jti := fmt.Sprintf("sidecar-ticket-%08d", f.nextJTI.Add(1))
	token, err := f.signer.Sign(validClaims(f.now, relayID, frontID, jti))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type testEdge struct {
	URL      string
	server   *http.Server
	listener net.Listener
}

func (e *testEdge) Close() {
	_ = e.server.Close()
	_ = e.listener.Close()
}

func sidecarEdge(t *testing.T, handler http.Handler, originToken, viewer string) *testEdge {
	t.Helper()
	edgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(OriginTokenHeader, originToken)
		r.Header.Set(DefaultViewerAddressHeader, viewer)
		handler.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: edgeHandler}
	edge := &testEdge{URL: "http://" + listener.Addr().String(), server: server, listener: listener}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(edge.Close)
	return edge
}

func dialSidecarClient(t *testing.T, edge *testEdge, ticket string) (*Client, error) {
	t.Helper()
	return DialClient(t.Context(), ClientOptions{
		URL:    strings.Replace(edge.URL, "http://", "ws://", 1) + BridgePath,
		Ticket: ticket, PingInterval: -1,
	})
}

func waitSnapshot(t *testing.T, stats *SidecarStats, check func(SidecarSnapshot) bool) SidecarSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := stats.Snapshot()
		if check(snapshot) {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stats condition not reached: %+v", stats.Snapshot())
	return SidecarSnapshot{}
}

func TestSidecarRejectsWrongRelayFrontAndTicketReplay(t *testing.T) {
	fixture := newSidecarFixture(t, nil)
	edgeA := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.10:443")
	edgeB := sidecarEdge(t, fixture.handler, testOriginB, "198.51.100.10:443")

	if client, err := dialSidecarClient(t, edgeA, fixture.ticket(t, "relay-b", "front-a")); err == nil {
		_ = client.Close()
		t.Fatal("ticket for another relay was accepted")
	}
	if client, err := dialSidecarClient(t, edgeB, fixture.ticket(t, "relay-a", "front-a")); err == nil {
		_ = client.Close()
		t.Fatal("ticket for another front was accepted")
	}
	replayTicket := fixture.ticket(t, "relay-a", "front-a")
	client, err := dialSidecarClient(t, edgeA, replayTicket)
	if err != nil {
		t.Fatalf("first ticket use: %v", err)
	}
	_ = client.Close()
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 0 })
	if client, err := dialSidecarClient(t, edgeA, replayTicket); err == nil {
		_ = client.Close()
		t.Fatal("replayed ticket was accepted")
	}
	snapshot := fixture.stats.Snapshot()
	if snapshot.TicketRejections != 1 || snapshot.FrontRejections != 1 || snapshot.ReplayRejections != 1 {
		t.Fatalf("rejection counters = %+v", snapshot)
	}
}

func TestSidecarOriginTokenRotationAndAuthBeforeViewerAddress(t *testing.T) {
	fixture := newSidecarFixture(t, nil)
	for _, origin := range []string{testOriginOld, testOriginNew} {
		edge := sidecarEdge(t, fixture.handler, origin, "198.51.100.11:443")
		client, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
		if err != nil {
			t.Fatalf("rotating token rejected: %v", err)
		}
		_ = client.Close()
	}
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 0 })

	req := httptest.NewRequest(http.MethodGet, BridgePath, nil)
	req.Header.Set(OriginTokenHeader, strings.Repeat("z", 32))
	req.Header.Set(DefaultViewerAddressHeader, "not-an-address")
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong origin status = %d", response.Code)
	}
	snapshot := fixture.stats.Snapshot()
	if snapshot.OriginAuthRejections != 1 || snapshot.ViewerAddressRejections != 0 {
		t.Fatalf("origin/viewer counters = %+v", snapshot)
	}
}

func TestSidecarConfigRejectsSharedTokensAndNonLoopbackTargets(t *testing.T) {
	fixture := newSidecarFixture(t, nil)
	for _, target := range []string{"relay.example:443", "192.0.2.1:443", "localhost:443", "127.0.0.1", "127.0.0.1:0"} {
		opts := SidecarOptions{
			RelayID: "relay-a", Verifier: fixture.verifier,
			FrontOriginTokens: map[string][]string{"front-a": {testOriginOld}},
			FixedTarget:       target,
		}
		if _, err := NewSidecarHandler(opts); err == nil {
			t.Fatalf("unsafe fixed target %q accepted", target)
		}
	}
	if _, err := NewSidecarHandler(SidecarOptions{
		RelayID: "relay-a", Verifier: fixture.verifier,
		FrontOriginTokens: map[string][]string{
			"front-a": {testOriginOld}, "front-b": {testOriginOld},
		},
	}); err == nil {
		t.Fatal("one origin token was allowed to authenticate two fronts")
	}
}

func startEchoTarget(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener
}

func TestSidecarOpaqueBridgeAlwaysDialsOneFixedTargetAndCleansUp(t *testing.T) {
	target := startEchoTarget(t)
	var dialMu sync.Mutex
	var dialed []string
	fixture := newSidecarFixture(t, func(opts *SidecarOptions) {
		opts.FixedTarget = target.Addr().String()
		opts.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dialMu.Lock()
			dialed = append(dialed, network+" "+address)
			dialMu.Unlock()
			if network != "tcp" || address != target.Addr().String() {
				return nil, errors.New("unexpected dial authority")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		opts.NoStreamIdleTimeout = time.Minute
		opts.StreamIdleTimeout = time.Minute
	})
	edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.12:443")
	client, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- client.Serve(serveCtx) }()
	host, port := client.Endpoint()
	local, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("opaque-reality-bytes")
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(local, received); err != nil {
		t.Fatal(err)
	}
	if string(received) != string(payload) {
		t.Fatalf("received %q", received)
	}
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentStreams == 1 })
	_ = local.Close()
	cancelServe()
	_ = client.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client Serve did not stop")
	}
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool {
		return s.CurrentSessions == 0 && s.CurrentStreams == 0
	})
	dialMu.Lock()
	defer dialMu.Unlock()
	if len(dialed) != 1 || dialed[0] != "tcp "+target.Addr().String() {
		t.Fatalf("dial attempts = %v", dialed)
	}
}

func TestSidecarTicketStreamBudgetIsLifetimeBound(t *testing.T) {
	target := startEchoTarget(t)
	var dialCount atomic.Int32
	fixture := newSidecarFixture(t, func(opts *SidecarOptions) {
		opts.FixedTarget = target.Addr().String()
		opts.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCount.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		opts.NoStreamIdleTimeout = time.Minute
	})
	claims := validClaims(fixture.now, "relay-a", "front-a", "one-stream-ticket-00000001")
	claims.MaxStreams = 1
	ticket, err := fixture.signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.12:443")
	client, err := dialSidecarClient(t, edge, ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- client.Serve(serveCtx) }()

	host, port := client.Endpoint()
	endpoint := net.JoinHostPort(host, fmt.Sprint(port))
	first, err := net.Dial("tcp", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("first"))
	if _, err := io.ReadFull(first, got); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentStreams == 0 })

	second, err := net.Dial("tcp", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.SetDeadline(time.Now().Add(time.Second))
	_, _ = second.Write([]byte("second"))
	buffer := make([]byte, 1)
	if _, err := second.Read(buffer); err == nil {
		t.Fatal("stream beyond ticket lifetime budget remained open")
	}
	_ = second.Close()
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.StreamLimitRejections == 1 })
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("ticket with max_streams=1 caused %d loopback dials", got)
	}

	cancelServe()
	_ = client.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client Serve did not stop")
	}
}

func TestSidecarPerSourceSessionLimitIsReleased(t *testing.T) {
	fixture := newSidecarFixture(t, func(opts *SidecarOptions) { opts.MaxSessionsPerSource = 1 })
	edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.13:443")
	first, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
	if err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 1 })
	if second, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a")); err == nil {
		_ = second.Close()
		t.Fatal("second session behind same source passed the configured limit")
	}
	if got := fixture.stats.Snapshot().SourceSessionRejections; got != 1 {
		t.Fatalf("source session rejections = %d", got)
	}
	_ = first.Close()
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 0 })
	third, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
	if err != nil {
		t.Fatalf("source capacity was not released: %v", err)
	}
	_ = third.Close()
}

func TestSidecarGlobalHandshakeRateLimitIsBounded(t *testing.T) {
	fixture := newSidecarFixture(t, func(opts *SidecarOptions) {
		opts.GlobalHandshakeRatePerSecond = 0.000001
		opts.GlobalHandshakeBurst = 1
	})
	edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.13:443")
	first, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 0 })

	if second, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a")); err == nil {
		_ = second.Close()
		t.Fatal("second handshake passed the configured global rate limit")
	}
	if got := fixture.stats.Snapshot().GlobalHandshakeRateRejections; got != 1 {
		t.Fatalf("global handshake rate rejections = %d", got)
	}
}

func TestSidecarPendingHandshakeLimitBoundsReplayStalls(t *testing.T) {
	replay := &blockingReplayStore{started: make(chan struct{}), release: make(chan struct{})}
	fixture := newSidecarFixture(t, func(opts *SidecarOptions) {
		opts.ReplayStore = replay
		opts.MaxPendingHandshakes = 1
	})
	edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.15:443")
	type dialResult struct {
		client *Client
		err    error
	}
	firstResult := make(chan dialResult, 1)
	firstTicket := fixture.ticket(t, "relay-a", "front-a")
	go func() {
		client, err := DialClient(context.Background(), ClientOptions{
			URL:    strings.Replace(edge.URL, "http://", "ws://", 1) + BridgePath,
			Ticket: firstTicket, PingInterval: -1,
		})
		firstResult <- dialResult{client: client, err: err}
	}()
	waitWSS := func(signal <-chan struct{}) {
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal("first replay check did not start")
		}
	}
	waitWSS(replay.started)

	started := time.Now()
	second, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
	if err == nil {
		_ = second.Close()
		t.Fatal("handshake beyond pending limit succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("excess handshake was not rejected promptly: %s", elapsed)
	}
	if got := fixture.stats.Snapshot().HandshakeConcurrencyRejections; got != 1 {
		t.Fatalf("handshake concurrency rejections = %d", got)
	}
	if got := replay.calls.Load(); got != 1 {
		t.Fatalf("pending limit allowed %d replay operations", got)
	}

	close(replay.release)
	select {
	case result := <-firstResult:
		if result.err != nil {
			t.Fatalf("first handshake after replay release: %v", result.err)
		}
		_ = result.client.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("first handshake did not finish")
	}
}

func TestSidecarIdleAndLifetimeControlsCloseAndReleaseSessions(t *testing.T) {
	for name, configure := range map[string]func(*SidecarOptions){
		"idle": func(opts *SidecarOptions) {
			opts.NoStreamIdleTimeout = 50 * time.Millisecond
			opts.SessionLifetime = time.Minute
		},
		"lifetime": func(opts *SidecarOptions) {
			opts.NoStreamIdleTimeout = time.Minute
			opts.SessionLifetime = 50 * time.Millisecond
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSidecarFixture(t, configure)
			edge := sidecarEdge(t, fixture.handler, testOriginOld, "198.51.100.14:443")
			client, err := dialSidecarClient(t, edge, fixture.ticket(t, "relay-a", "front-a"))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool { return s.CurrentSessions == 1 })
			snapshot := waitSnapshot(t, fixture.stats, func(s SidecarSnapshot) bool {
				return s.CurrentSessions == 0
			})
			if name == "idle" && snapshot.IdleSessionCloses != 1 {
				t.Fatalf("idle close counters = %+v", snapshot)
			}
			if name == "lifetime" && snapshot.LifetimeSessionCloses != 1 {
				t.Fatalf("lifetime close counters = %+v", snapshot)
			}
		})
	}
}

func TestSourceUsageStreamLimit(t *testing.T) {
	usage := newSourceUsage(100, 2)
	if !usage.acquireStream("source") || !usage.acquireStream("source") || usage.acquireStream("source") {
		t.Fatal("stream source limit was not enforced")
	}
	usage.releaseStream("source")
	if !usage.acquireStream("source") {
		t.Fatal("stream source capacity was not released")
	}
}
