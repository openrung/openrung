package connectcore

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/openrung/openrung/brokerapi"
)

type discoveryOrderTransport func(*http.Request) (*http.Response, error)

func (f discoveryOrderTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise each engine entry point with the real shared fetcher. Candidate
// spellings and persisted built-ins are covered by brokerapi and the contract
// vectors; platform differences only affect headers (tested separately below).
// Unsigned 200 responses must fail closed through Azure's separate phase.
func TestSharedDiscoveryOrderAndRecovery(t *testing.T) {
	for _, tc := range []struct{ name, path, primary string }{
		{"connect", "connect", brokerapi.DefaultBrokerURL},
		{"directory", "directory", ""},
		{"reconnect_default", "reconnect", brokerapi.DefaultBrokerURL},
		{"reconnect_custom_after_fallback", "reconnect", "https://custom.example/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New()
			s.SocketProtector = &recordingProtector{}
			var mu sync.Mutex
			var calls []string
			s.brokerHTTPClient().Transport = discoveryOrderTransport(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				mode := ""
				if r.URL.Host == "cdn-edge-cxdnhsg2aadmaubj.z02.azurefd.net" && brokerapi.AzureSNIRequested(r.Context()) {
					mode = " (SNI)"
				}
				calls = append(calls, "https://"+r.URL.Host+"/"+mode)
				mu.Unlock()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"count":0,"relays":[]}`)), Request: r}, nil
			})
			t.Cleanup(func() { s.protectedHTTP.mu.Lock(); defer s.protectedHTTP.mu.Unlock(); s.protectedHTTP.releaseLocked() })
			var err error
			switch tc.path {
			case "connect":
				_, err = s.relayFetcher()(t.Context(), tc.primary, 5, "", "")
			case "directory":
				_, err = s.directory.fetcher(t.Context(), tc.primary, s.identityForDirectory())
			case "reconnect":
				// The previous attempt fell back to Cloudflare. Recovery
				// must still honor the original primary, including custom.
				conn := &connection{discoveryPrimary: tc.primary, brokerURL: brokerapi.DefaultBrokerURL}
				_, _, stage, fetchErr := s.reladder(t.Context(), conn, 0, RelayTarget{}, "failed-relay")
				err = fetchErr
				if stage != "broker_fetch" {
					t.Fatalf("stage = %q", stage)
				}
			}
			if err == nil {
				t.Fatal("unsigned relay list accepted")
			}
			var verification *brokerapi.RelayListVerificationError
			if !errors.As(err, &verification) {
				t.Fatalf("error = %T %v, want signature rejection", err, err)
			}
			want := []string{brokerapi.CloudFrontBrokerURL, brokerapi.AzureBrokerURL + " (SNI)", brokerapi.DefaultBrokerURL, brokerapi.AzureBrokerURL}
			if tc.primary == "https://custom.example/" {
				want = append([]string{tc.primary}, want...)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, want) {
				t.Fatalf("requests = %v, want %v", calls, want)
			}
		})
	}
}

func TestDiscoveryPlatformHeaders(t *testing.T) {
	for _, tc := range []struct {
		platform brokerapi.Platform
		header   string
	}{
		{brokerapi.PlatformDesktop, "X-OpenRung-Desktop"},
		{brokerapi.PlatformAndroid, "X-OpenRung-Android-API"},
		{brokerapi.PlatformIOS, "X-OpenRung-iOS-Version"},
	} {
		t.Run(string(tc.platform), func(t *testing.T) {
			s := New()
			s.Platform = tc.platform
			if tc.platform != brokerapi.PlatformDesktop {
				s.Mobile = &MobileHost{PlatformVersion: "test-platform-version"}
			}
			s.SocketProtector = &recordingProtector{}
			s.brokerHTTPClient().Transport = discoveryOrderTransport(func(r *http.Request) (*http.Response, error) {
				if got := r.Header.Get(tc.header); got == "" || (tc.platform != brokerapi.PlatformDesktop && got != "test-platform-version") {
					t.Errorf("%s = %q", tc.header, got)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`)), Request: r}, nil
			})
			t.Cleanup(func() { s.protectedHTTP.mu.Lock(); defer s.protectedHTTP.mu.Unlock(); s.protectedHTTP.releaseLocked() })
			// A successful loopback override needs no signing key and no stagger.
			if _, err := s.relayFetcher()(t.Context(), "http://127.0.0.1:8080/", 5, "", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
