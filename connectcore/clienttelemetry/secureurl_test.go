package clienttelemetry

import "testing"

// TestTelemetryURLRejectsCleartext confirms pre-tunnel telemetry (which carries
// the persistent client identity) cannot be sent to a cleartext endpoint.
func TestTelemetryURLRejectsCleartext(t *testing.T) {
	if _, err := TelemetryURL("http://54.238.185.205:8080/"); err == nil {
		t.Fatal("TelemetryURL accepted a cleartext bare-IP broker URL")
	}
	if _, err := TelemetryURL("http://example.com/"); err == nil {
		t.Fatal("TelemetryURL accepted cleartext http to a real host")
	}
	if _, err := TelemetryURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("TelemetryURL rejected a loopback dev URL: %v", err)
	}
	if _, err := TelemetryURL("https://broker.openrung.org/"); err != nil {
		t.Fatalf("TelemetryURL rejected a valid HTTPS URL: %v", err)
	}
}
