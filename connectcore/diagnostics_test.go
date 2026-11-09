// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

const (
	diagTestUUID     = "3f1c2b7a-9d4e-4c1a-8b2f-6e5d4c3b2a19"
	diagTestRealPK   = "Zx9mQ2vT7kLp4RsW1nYb8cHd3fGj6uKe0aVoXiNqBtM"
	diagTestShortID  = "a1b2c3d4"
	diagTestShortID6 = "0f9e8d"
	diagTestToken    = "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ1c2VyMTIzIn0.c2lnbmF0dXJlQmFzZTY0VXJs"
	diagTestRelayIP  = "198.51.100.10"
	diagTestRelayV6  = "2001:db8:10::1"
	diagTestClientV4 = "203.0.113.77"
	diagTestClientV6 = "2001:db8:ffff::42"
	diagTestRelayID  = "relay_0123456789abcdef0123456789abcdef"
)

func diagTestRelay(id, host string) brokerapi.RelayDescriptor {
	r := usableRelay(id, "JP", "Tokyo", "Japan")
	r.PublicHost = host
	r.ClientID = diagTestUUID
	r.RealityPublicKey = diagTestRealPK
	r.ShortID = diagTestShortID
	return r
}

func TestDiagnosticRedaction(t *testing.T) {
	var d diagnosticLog
	d.learnRelays([]brokerapi.RelayDescriptor{
		diagTestRelay(diagTestRelayID, diagTestRelayIP),
		diagTestRelay("relay_v6", diagTestRelayV6),
	})
	d.learnSecret(diagTestShortID6, "abc") // too short to learn: must not clip words

	d.mu.Lock()
	r := newRedactor(d.secrets, d.allowed)
	d.mu.Unlock()

	cases := []struct {
		name, in, want string
	}{
		{"relay endpoint kept", "trying relay " + diagTestRelayID + " at " + diagTestRelayIP + ":443",
			"trying relay " + diagTestRelayID + " at " + diagTestRelayIP + ":443"},
		{"relay v6 kept", "dial tcp [" + diagTestRelayV6 + "]:443: i/o timeout",
			"dial tcp [" + diagTestRelayV6 + "]:443: i/o timeout"},
		{"client v4 with port", "read tcp " + diagTestClientV4 + ":50412->" + diagTestRelayIP + ":443: reset",
			"read tcp [ip]:50412->" + diagTestRelayIP + ":443: reset"},
		{"client v6", "your address " + diagTestClientV6 + ".", "your address [ip]."},
		{"v6 ending in compression with port", "read udp [2001:db8:ffff::]:50000: refused", "read udp [[ip]]:50000: refused"},
		{"v6 ending in compression before a period", "from 2001:db8:ffff::.", "from [ip]."},
		{"private lan", "local 192.168.1.23", "local [ip]"},
		{"loopback kept", "bridge 127.0.0.1:1080", "bridge 127.0.0.1:1080"},
		{"uuid", "client " + diagTestUUID + " refused", "client [redacted] refused"},
		{"unknown uuid", "id 00000000-1111-4222-8333-444444444444", "id [uuid]"},
		{"reality key", "pbk=" + diagTestRealPK, "pbk=[redacted]"},
		{"unknown base64 key", "key Qm9vbXRvd25Mb3VuZ2VCeXRlczEyMzQ1Njc4OTA end", "key [token] end"},
		{"short id", "sid " + diagTestShortID + ",", "sid [redacted],"},
		{"six char short id", "sid=" + diagTestShortID6, "sid=[redacted]"},
		{"short id not inside words", "decade facade", "decade facade"},
		{"token", "bearer " + diagTestToken, "bearer [token]"},
		{"unknown hex", "key 9f86d081884c7d65", "key [hex]"},
		{"hostname label kept", "front d1a2b3c4d5e6f7.cloudfront.net", "front d1a2b3c4d5e6f7.cloudfront.net"},
		{"numbers kept", "after 12345678ms at 15:04:05.000 v0.11.0", "after 12345678ms at 15:04:05.000 v0.11.0"},
		{"paths kept", "GET /api/v1/relays", "GET /api/v1/relays"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.redact(tc.in); got != tc.want {
				t.Fatalf("redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDiagnosticLogIsBounded(t *testing.T) {
	var d diagnosticLog
	for i := 0; i < diagnosticEventCapacity+40; i++ {
		d.note(fmt.Sprintf("line %d", i))
	}
	if len(d.events) != diagnosticEventCapacity {
		t.Fatalf("events = %d, want %d", len(d.events), diagnosticEventCapacity)
	}
	if got, want := d.events[len(d.events)-1].Message, fmt.Sprintf("line %d", diagnosticEventCapacity+39); got != want {
		t.Fatalf("newest = %q, want %q", got, want)
	}
	if got := d.events[0].Message; got != "line 40" {
		t.Fatalf("oldest = %q, want line 40", got)
	}

	d.note(strings.Repeat("x", diagnosticLineLimit*2))
	if n := len(d.events[len(d.events)-1].Message); n > diagnosticLineLimit+len("…") {
		t.Fatalf("long line kept %d bytes", n)
	}

	req := httptest.NewRequest(http.MethodGet, "https://broker.example/api/v1/relays", nil)
	for i := 0; i < diagnosticBrokerCapacity+5; i++ {
		d.beginBroker(req)
	}
	if len(d.brokers) != diagnosticBrokerCapacity {
		t.Fatalf("broker attempts = %d, want %d", len(d.brokers), diagnosticBrokerCapacity)
	}

	for i := 0; i < diagnosticSecretCapacity+10; i++ {
		d.learnSecret(fmt.Sprintf("secret-%06d", i))
	}
	if len(d.secrets) != diagnosticSecretCapacity || len(d.secretQ) != diagnosticSecretCapacity {
		t.Fatalf("secrets = %d/%d, want %d", len(d.secrets), len(d.secretQ), diagnosticSecretCapacity)
	}
}

func lastAttempt(t *testing.T, d *diagnosticLog) DiagnosticBrokerAttempt {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.brokers) == 0 {
		t.Fatal("no broker attempt recorded")
	}
	return d.brokers[len(d.brokers)-1].a
}

func TestDiagnosticTransportRecordsPhases(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"relays":[]}`)
	}))
	defer srv.Close()

	var d diagnosticLog
	client := &http.Client{Transport: &diagnosticTransport{base: srv.Client().Transport, log: &d}}
	resp, err := client.Get(srv.URL + "/api/v1/relays?limit=5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	a := lastAttempt(t, &d)
	if a.Request != "GET /api/v1/relays" || a.FrontKind != "custom" {
		t.Fatalf("request/front = %q/%q", a.Request, a.FrontKind)
	}
	for name, v := range map[string]*int64{
		"connect": a.ConnectDoneMS, "tls": a.TLSDoneMS, "conn": a.ConnReadyMS,
		"written": a.RequestWrittenMS, "first_byte": a.FirstByteMS, "body_done": a.BodyDoneMS, "done": a.DoneMS,
	} {
		if v == nil {
			t.Fatalf("%s phase not recorded: %+v", name, a)
		}
	}
	if a.StatusCode != http.StatusOK || a.BodyBytes != int64(len(`{"relays":[]}`)) || a.Error != "" || a.TLSVersion == "" {
		t.Fatalf("attempt = %+v", a)
	}
	if a.RemoteAddr == "" {
		t.Fatalf("remote address missing: %+v", a)
	}
}

// A front that completes TLS and accepts the request but never answers must
// show exactly that: request written, no first byte, and the timeout.
func TestDiagnosticTransportLocatesStalledResponse(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	var d diagnosticLog
	client := &http.Client{Transport: &diagnosticTransport{base: srv.Client().Transport, log: &d}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/relays", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("stalled request succeeded")
	}

	a := lastAttempt(t, &d)
	if a.TLSDoneMS == nil || a.RequestWrittenMS == nil {
		t.Fatalf("handshake/write not recorded: %+v", a)
	}
	if a.FirstByteMS != nil || a.StatusCode != 0 || a.BodyDoneMS != nil {
		t.Fatalf("stalled request shows a response: %+v", a)
	}
	if a.DoneMS == nil || !strings.Contains(a.Error, "deadline") {
		t.Fatalf("error = %q done = %v", a.Error, a.DoneMS)
	}
}

func TestDiagnosticTransportRecordsConnectFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()
	transport := srv.Client().Transport
	srv.Close()

	var d diagnosticLog
	client := &http.Client{Transport: &diagnosticTransport{base: transport, log: &d}}
	if _, err := client.Get("https://" + addr + "/api/v1/relays"); err == nil {
		t.Fatal("request to closed server succeeded")
	}
	a := lastAttempt(t, &d)
	if a.Connects == 0 || a.ConnectError == "" || a.TLSDoneMS != nil || a.RequestWrittenMS != nil || a.Error == "" {
		t.Fatalf("attempt = %+v", a)
	}
}

// brokerapi handshakes inside its custom TLS dial, where net/http reports no
// handshake failure; the transport still attributes the failure to TLS.
func TestDiagnosticTransportRecordsCustomDialTLSFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	var d diagnosticLog
	client := &http.Client{Transport: &diagnosticTransport{base: brokerapi.NewHTTPClient(0).Transport, log: &d}}
	if _, err := client.Get(srv.URL + "/api/v1/relays"); err == nil {
		t.Fatal("request to an untrusted certificate succeeded")
	}
	a := lastAttempt(t, &d)
	if a.ConnectDoneMS == nil || a.ConnectError != "" {
		t.Fatalf("TCP connect not recorded: %+v", a)
	}
	if a.TLSError == "" || !strings.Contains(a.TLSError, "certificate") || a.TLSDoneMS != nil || a.ConnReadyMS != nil {
		t.Fatalf("TLS failure not recorded: %+v", a)
	}
}

func TestTracedBrokerClientKeepsTheSharedTransport(t *testing.T) {
	s := New()
	traced := s.tracedBrokerClient()
	dt, ok := traced.Transport.(*diagnosticTransport)
	if !ok {
		t.Fatalf("transport = %T", traced.Transport)
	}
	if dt.base != brokerapi.NewHTTPClient(0).Transport {
		t.Fatal("traced client does not wrap brokerapi's shared default transport")
	}
	if dt.log != &s.diagnostics {
		t.Fatal("traced client records into another log")
	}
}

func TestBuiltInFrontKinds(t *testing.T) {
	for want, front := range map[string]string{
		"cloudfront": brokerapi.CloudFrontBrokerURL,
		"azure":      brokerapi.AzureBrokerURL,
		"cloudflare": brokerapi.DefaultBrokerURL,
		"custom":     "https://broker.example/",
	} {
		req := httptest.NewRequest(http.MethodGet, front+"api/v1/relays", nil)
		if got := brokerFrontKind(req.URL.Hostname()); got != want {
			t.Fatalf("front kind of %s = %q, want %q", front, got, want)
		}
	}
}

func TestEngineLogFeedsDiagnosticsButTunnelOutputDoesNot(t *testing.T) {
	sink := &testSink{}
	s := New()
	s.Sink = sink
	s.appendLog("trying relay a at " + diagTestRelayIP + ":443")
	_, _ = s.logWriter().Write([]byte("outbound connection to example.com:443\n"))

	text := s.DiagnosticsText()
	if !strings.Contains(text, "trying relay a") {
		t.Fatalf("engine line missing from diagnostics:\n%s", text)
	}
	if strings.Contains(text, "example.com") {
		t.Fatalf("tunnel output leaked into diagnostics:\n%s", text)
	}
	sink.mu.Lock()
	got := strings.Join(sink.logs, "\n")
	sink.mu.Unlock()
	if !strings.Contains(got, "example.com") {
		t.Fatalf("tunnel output no longer reaches the sink: %v", got)
	}
}

// A failed connect leaves a report that walks the ladder with relay
// addresses but without the relays' credentials or the client's address.
func TestDiagnosticsAfterFailedConnectAreRedacted(t *testing.T) {
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{
		diagTestRelay("a", diagTestRelayIP),
		diagTestRelay("b", "198.51.100.11"),
	}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.dialRelay = func(ctx context.Context, host string, port int) (int64, error) {
		return 0, fmt.Errorf("dial tcp 192.168.1.23:50000->%s:%d: connection refused (client %s pbk %s)", host, port, diagTestUUID, diagTestRealPK)
	}
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	text := s.DiagnosticsText()
	jsonText := s.DiagnosticsJSON()
	for _, out := range []string{text, jsonText} {
		for _, secret := range []string{diagTestUUID, diagTestRealPK, "192.168.1.23"} {
			if strings.Contains(out, secret) {
				t.Fatalf("diagnostics contain %q:\n%s", secret, out)
			}
		}
		for _, want := range []string{"trying relay a at " + diagTestRelayIP + ":443", "relay b failed", "All relay connection attempts failed"} {
			if !strings.Contains(out, want) {
				t.Fatalf("diagnostics missing %q:\n%s", want, out)
			}
		}
	}

	var report DiagnosticReport
	if err := json.Unmarshal([]byte(jsonText), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != string(StatusFailed) || report.EngineVersion == "" || report.Mode != "proxy" || len(report.Events) == 0 {
		t.Fatalf("report header = %+v", report)
	}
	if !strings.Contains(report.LastError, "All relay connection attempts failed") || strings.Contains(report.LastError, diagTestUUID) {
		t.Fatalf("last error = %q", report.LastError)
	}
}

type diagTestCrashError struct{ quote string }

func (e *diagTestCrashError) Error() string {
	return "sing-box exited: " + e.quote + " (exit status 1)"
}

func (e *diagTestCrashError) TelemetrySafe() string { return "sing-box exited: exit status 1" }

// A tunnel crash quotes the core's own output, which can name a destination
// the user reached; the report carries the telemetry-safe rendering instead.
func TestDiagnosticsReplaceQuotedTunnelCrashOutput(t *testing.T) {
	const quote = "FATAL outbound/vless[proxy]: connection to secret-destination.example:443 failed"
	sink := newTelemetrySink(t)
	fixtures := []brokerapi.RelayDescriptor{diagTestRelay("a", diagTestRelayIP)}
	s, _ := newLadderService(t, func() []brokerapi.RelayDescriptor { return fixtures })
	s.TunnelRuntime = runFuncRuntime(func(context.Context, []byte) error {
		return &diagTestCrashError{quote: quote}
	})
	// Hold readiness so the crash, not the readiness probe, decides the rung.
	s.tunnelReady = func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.Connect(sink.srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, StatusFailed)
	waitIdle(t, s)

	for _, out := range []string{s.DiagnosticsText(), s.DiagnosticsJSON()} {
		if strings.Contains(out, "secret-destination") {
			t.Fatalf("tunnel crash output leaked into diagnostics:\n%s", out)
		}
		if !strings.Contains(out, "sing-box exited: exit status 1") {
			t.Fatalf("diagnostics lost the crash itself:\n%s", out)
		}
	}
}

func TestDiagnosticLogSubstitutesTunnelErrorsBeforeTruncating(t *testing.T) {
	var d diagnosticLog
	crash := fmt.Errorf("relay a failed: %w", &diagTestCrashError{quote: strings.Repeat("x", diagnosticLineLimit) + " host.example"})
	d.learnTunnelError(crash)
	d.note(crash.Error())
	if got := d.events[0].Message; got != "relay a failed: sing-box exited: exit status 1" {
		t.Fatalf("event = %q", got)
	}
	// The forwarded exit report keeps the Done contract: one value, then closed.
	done := make(chan error, 1)
	done <- &diagTestCrashError{quote: "other.example"}
	close(done)
	out := d.watchTunnelExit(done)
	if err := <-out; err == nil {
		t.Fatal("exit report lost")
	}
	if _, open := <-out; open {
		t.Fatal("forwarded channel not closed")
	}
	d.note("x sing-box exited: other.example (exit status 1)")
	if got := d.events[1].Message; strings.Contains(got, "other.example") {
		t.Fatalf("event = %q", got)
	}
}
