package client

import "testing"

func TestEnforceSecureBrokerURL(t *testing.T) {
	allowed := []string{
		"https://broker.openrung.org/",
		"https://1.2.3.4:8443",
		"http://localhost:8080",
		"http://127.0.0.1:8080/",
		"http://[::1]:8080",
	}
	for _, u := range allowed {
		if _, err := EnforceSecureBrokerURL(u); err != nil {
			t.Errorf("EnforceSecureBrokerURL(%q) rejected a safe URL: %v", u, err)
		}
	}

	rejected := []string{
		"http://broker.openrung.org/", // cleartext to a real host
		"http://54.238.185.205:8080/", // cleartext bare IP (the removed fallback)
		"http://192.168.1.1/",         // cleartext to a LAN host
		"ftp://broker.openrung.org/",  // wrong scheme
		"broker.openrung.org",         // no scheme
		"",                            // empty
	}
	for _, u := range rejected {
		if _, err := EnforceSecureBrokerURL(u); err == nil {
			t.Errorf("EnforceSecureBrokerURL(%q) accepted a cleartext/invalid URL", u)
		}
	}
}

// TestRelayListURLRejectsCleartext confirms the enforcement flows through the
// public builder that both the CLI and desktop discovery actually call.
func TestRelayListURLRejectsCleartext(t *testing.T) {
	if _, err := RelayListURL("http://54.238.185.205:8080/", 5); err == nil {
		t.Fatal("RelayListURL accepted a cleartext bare-IP broker URL")
	}
	if _, err := RelayListURL("http://localhost:8080", 5); err != nil {
		t.Fatalf("RelayListURL rejected a loopback dev URL: %v", err)
	}
}
