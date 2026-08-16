package engine

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe io.Writer for capturing engine/observer output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runConnLogSession runs a direct session with the given connection-log
// writer, opens one client connection against the observer, and returns once
// the engine has counted it.
func runConnLogSession(t *testing.T, output *syncBuffer, engineLog *syncBuffer) {
	t.Helper()
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	cfg := Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}
	if output != nil {
		cfg.ConnectionLogOutput = output
	}
	eng := New(cfg, Events{Log: engineLog})
	brokerClient := eng.cfg.brokerClient()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, brokerClient, eng.cfg, "log-relay", testIdentity, "127.0.0.1", directOnlyListenHost)
	}()
	defer func() {
		cancel()
		<-done
	}()

	eventually(t, 5*time.Second, "session online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})

	// One observed connection; xray is disabled, so the forward target refuses
	// and the observer immediately reports the disconnect too.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(eng.cfg.ListenPort)))
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer conn.Close()
	eventually(t, 5*time.Second, "connection counted", func() bool {
		return eng.Status().TotalConnections >= 1
	})
}

func TestConnectionLogOutputReceivesObserverLines(t *testing.T) {
	output := &syncBuffer{}
	engineLog := &syncBuffer{}
	runConnLogSession(t, output, engineLog)

	eventually(t, 5*time.Second, "connection log lines", func() bool {
		s := output.String()
		return strings.Contains(s, "client connected") && strings.Contains(s, "client disconnected")
	})
	// The console lines are opt-in only: they carry client addresses and must
	// not leak into the engine's UI log stream.
	if s := engineLog.String(); strings.Contains(s, "client connected") {
		t.Fatalf("engine log contains per-connection lines: %q", s)
	}
}

func TestConnectionLinesStayOutOfLogsByDefault(t *testing.T) {
	engineLog := &syncBuffer{}
	runConnLogSession(t, nil, engineLog)

	if s := engineLog.String(); strings.Contains(s, "client connected") || strings.Contains(s, "client disconnected") {
		t.Fatalf("per-connection lines reached the engine log without opt-in: %q", s)
	}
}
