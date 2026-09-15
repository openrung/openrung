package brokerapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAzureSNIVerifiesEndpointHostname(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted bool
	}{
		{azureBrokerHost, true}, {azureEdgeNoSNICertificateSAN, false}, {"other.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, roots := testCloudFrontChain(t, tc.name)
			dial, _, _ := testTLSSocketDialer(t, testBrokerServerConfig(cert))
			d := testBrokerECHDialer(dial, roots, newECHConfigState(embeddedCloudflareECHConfigList))
			d.azureSNI = true
			conn, err := d.dialTLSContext(t.Context(), "tcp", azureBrokerHost+":443")
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%v error=%v", tc.accepted, err)
			}
			if conn != nil {
				defer closeTLSConn(conn)
				if got := conn.(*tls.Conn).ConnectionState().ServerName; got != azureBrokerHost {
					t.Fatalf("SNI=%q", got)
				}
			}
		})
	}
}

func TestAzureModesNeverShareConnections(t *testing.T) {
	// A certificate valid under both rules deliberately permits either handshake:
	// only isolated pools prevent an SNI request reusing a prior no-SNI socket.
	cert, roots := testCloudFrontChain(t, azureBrokerHost, azureEdgeNoSNICertificateSAN)
	var mu sync.Mutex
	var names []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		names = append(names, r.TLS.ServerName)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ForceAttemptHTTP2 = false
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	var dials int
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	tr := newTransport(base, newECHConfigState(embeddedCloudflareECHConfigList), time.Second)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, normal := range []bool{false, true, false, true} {
		ctx := t.Context()
		if normal {
			ctx = WithAzureSNI(ctx, true)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", AzureBrokerURL, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(names, []string{"", azureBrokerHost, "", azureBrokerHost}) || dials != 2 {
		t.Fatalf("SNI names=%v dials=%d", names, dials)
	}
}

func TestAzureDiscoveryModesAndSignatureFailure(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	api := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mode := ""
		if r.URL.Hostname() == azureBrokerHost {
			if AzureSNIRequested(r.Context()) {
				mode = " SNI"
			} else {
				mode = " no-SNI"
			}
		}
		mu.Lock()
		calls = append(calls, r.URL.Hostname()+mode)
		mu.Unlock()
		return response(r, 200, `{"count":0,"relays":[]}`), nil
	})}, Options{})
	_, err := api.FirstReachable(t.Context(), BrokerCandidates(""), ListOptions{Stagger: time.Millisecond})
	if err == nil {
		t.Fatal("unsigned response accepted")
	}
	want := []string{cloudFrontBrokerHost, azureBrokerHost + " SNI", cloudflareBrokerHost, azureBrokerHost + " no-SNI"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("attempts=%v want=%v", calls, want)
	}
}

func TestAzureDiscoveryPreservesWinningMode(t *testing.T) {
	// Sequential test: temporarily trust the published test-vector key so the
	// real verifier and race can exercise successful Azure responses offline.
	vectors := loadVectors(t)
	oldKeys := productionRelayKeys
	productionRelayKeys = mustPinnedKeys(vectors.SpecVector.PubKeyHex)
	defer func() { productionRelayKeys = oldKeys }()
	key := vectorSigner(t, vectors)
	body, err := json.Marshal(map[string]any{"channel": ChannelAPI, "limit": 5, "count": 0, "relays": []any{}, "server_time": time.Now().UTC(), "not_after": time.Now().Add(time.Minute).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("CloudFront success avoids Azure SNI", func(t *testing.T) {
		var calls atomic.Int32
		api := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.URL.Hostname() != cloudFrontBrokerHost {
				t.Errorf("unexpected fallback request to %s", r.URL.Hostname())
			}
			res := response(r, 200, string(body))
			res.Header.Set(RelaySignatureHeader, signedHeader(key, vectors.SpecVector.KeyID, body))
			return res, nil
		})}, Options{})
		got, err := api.FirstReachable(t.Context(), BrokerCandidates(""), ListOptions{})
		if err != nil || got.BrokerURL != CloudFrontBrokerURL || got.AzureSNI || !got.RelayList.SignatureVerified || calls.Load() != 1 {
			t.Fatalf("result=%+v calls=%d error=%v", got, calls.Load(), err)
		}
	})
	for _, normalWins := range []bool{true, false} {
		var calls atomic.Int32
		api := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			res := response(r, 200, string(body))
			if r.URL.Hostname() == azureBrokerHost && AzureSNIRequested(r.Context()) == normalWins {
				res.Header.Set(RelaySignatureHeader, signedHeader(key, vectors.SpecVector.KeyID, body))
			}
			return res, nil
		})}, Options{})
		got, err := api.FirstReachable(t.Context(), BrokerCandidates(""), ListOptions{Stagger: time.Millisecond})
		if err != nil || got.BrokerURL != AzureBrokerURL || got.AzureSNI != normalWins || !got.RelayList.SignatureVerified {
			t.Fatalf("normalWins=%v result=%+v error=%v", normalWins, got, err)
		}
		if !normalWins && calls.Load() != 4 {
			t.Fatalf("weak fallback skipped stronger attempts: %d", calls.Load())
		}
	}
}
