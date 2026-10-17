// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const accessTestNow = "2026-10-17T12:00:00Z"

// accessBody renders an access-channel body with one relay whose client_id
// is the lease credential.
func accessBody(channel, notAfter, leaseExpiresAt string) []byte {
	expiry := ""
	if leaseExpiresAt != "" {
		expiry = `,"lease_expires_at":"` + leaseExpiresAt + `"`
	}
	return []byte(`{"count":1,"server_time":"` + accessTestNow + `","not_after":"` + notAfter + `","key_id":"k","channel":"` + channel + `"` + expiry +
		`,"relays":[{"id":"r1","public_host":"1.2.3.4","public_port":443,"node_class":"foundation","protocol":"vless-reality-vision","client_id":"lease-uuid","reality_public_key":"key","short_id":"01","server_name":"example.com","flow":"xtls-rprx-vision","exit_mode":"direct","max_sessions":1,"max_mbps":10,"relay_version":"1.0.0","volunteer_version":"1.0.0","registered_at":"` + accessTestNow + `","last_heartbeat_at":"` + accessTestNow + `","expires_at":"2026-10-17T12:03:00Z"}]}`)
}

func TestVerifyAccessResponseAcceptsOnlyTheAccessChannel(t *testing.T) {
	vectors := loadVectors(t)
	keys := mustPinnedKeys(vectors.SpecVector.PubKeyHex)
	signer := vectorSigner(t, vectors)
	now, _ := time.Parse(time.RFC3339, accessTestNow)
	const endpoint = "https://broker.example/api/v1/access"

	body := accessBody(ChannelAccess, "2026-10-17T12:30:00Z", "2026-10-17T13:00:00Z")
	response, err := verifyAccessResponse(keys, endpoint, signedHeader(signer, vectors.SpecVector.KeyID, body), body, now)
	if err != nil {
		t.Fatalf("verify access body: %v", err)
	}
	if response.Channel != ChannelAccess || len(response.Relays) != 1 || response.Relays[0].ClientID != "lease-uuid" ||
		!response.LeaseExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("decoded access response = %+v", response)
	}

	// The same signed bytes never verify on the public relay-list channel,
	// and a public list never verifies here.
	if _, err := verifyRelayList(keys, "https://broker.example/api/v1/relays?limit=5", 5, signedHeader(signer, vectors.SpecVector.KeyID, body), body, now); err == nil {
		t.Fatal("an access body verified as a public relay list")
	}
	public := accessBody(ChannelAPI, "2026-10-17T12:30:00Z", "2026-10-17T13:00:00Z")
	for name, bad := range map[string][]byte{
		"api channel":         public,
		"inventory channel":   accessBody(ChannelInventory, "2026-10-17T12:30:00Z", "2026-10-17T13:00:00Z"),
		"expired not_after":   accessBody(ChannelAccess, "2026-10-17T11:00:00Z", "2026-10-17T13:00:00Z"),
		"no lease expiry":     accessBody(ChannelAccess, "2026-10-17T12:30:00Z", ""),
		"expiry before clock": accessBody(ChannelAccess, "2026-10-17T12:30:00Z", "2026-10-17T11:00:00Z"),
	} {
		if _, err := verifyAccessResponse(keys, endpoint, signedHeader(signer, vectors.SpecVector.KeyID, bad), bad, now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if _, err := verifyAccessResponse(keys, endpoint, "", body, now); err == nil {
		t.Error("unsigned access body verified on a non-loopback endpoint")
	}
	tampered := []byte(strings.Replace(string(body), "lease-uuid", "other-uuid", 1))
	if _, err := verifyAccessResponse(keys, endpoint, signedHeader(signer, vectors.SpecVector.KeyID, body), tampered, now); err == nil {
		t.Error("tampered credential verified")
	}

	// An empty answer (no eligible relay) needs no lease expiry.
	empty := []byte(`{"count":0,"server_time":"` + accessTestNow + `","not_after":"2026-10-17T12:30:00Z","key_id":"k","channel":"access","relays":[]}`)
	if response, err := verifyAccessResponse(keys, endpoint, signedHeader(signer, vectors.SpecVector.KeyID, empty), empty, now); err != nil || len(response.Relays) != 0 {
		t.Fatalf("empty access body: %+v %v", response, err)
	}
}

func TestClientRequestAccess(t *testing.T) {
	var (
		status     = http.StatusOK
		retryAfter string
		gotBody    map[string]any
		gotAuth    string
		gotHeaders http.Header
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/access", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody = nil
		_ = json.Unmarshal(raw, &gotBody)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"no"}`))
			return
		}
		_, _ = w.Write(accessBody(ChannelAccess, "2999-01-01T00:00:00Z", "2026-10-17T13:00:00Z"))
	})
	mux.HandleFunc("POST /redirect/api/v1/access", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/access", http.StatusTemporaryRedirect)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client := NewClient(ts.Client(), Options{AppVersion: "test"})
	ctx := context.Background()

	request := AccessRequest{Limit: 1, RelayID: "r1", AccessToken: "secret-token"}
	response, err := client.RequestAccess(ctx, ts.URL, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Relays) != 1 || response.Relays[0].ClientID != "lease-uuid" || response.LeaseExpiresAt.IsZero() {
		t.Fatalf("response = %+v", response)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if fmt.Sprint(gotBody) != fmt.Sprint(map[string]any{"limit": float64(1), "relay_id": "r1"}) {
		t.Fatalf("body = %v (the token must never be in the body)", gotBody)
	}
	if gotHeaders.Get("X-OpenRung-Client-ID") != "" || gotHeaders.Get("X-OpenRung-Session-ID") != "" {
		t.Fatal("access request carried the telemetry identity")
	}
	if text := fmt.Sprintf("%v %+v %#v %s", request, request, request, request); strings.Contains(text, "secret-token") {
		t.Fatalf("formatted request leaks the token: %s", text)
	}

	for _, tc := range []struct {
		status     int
		retryAfter string
		wantRetry  time.Duration
	}{
		{http.StatusUnauthorized, "", 0},
		{http.StatusForbidden, "", 0},
		{http.StatusNotFound, "", 0},
		{http.StatusServiceUnavailable, "2", 2 * time.Second},
	} {
		status, retryAfter = tc.status, tc.retryAfter
		_, err := client.RequestAccess(ctx, ts.URL, request)
		var statusErr *AccessStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != tc.status || statusErr.RetryAfter != tc.wantRetry {
			t.Errorf("%d → %v, want AccessStatusError{%d, %s}", tc.status, err, tc.status, tc.wantRetry)
		}
	}
	status, retryAfter = http.StatusOK, ""

	if _, err := client.RequestAccess(ctx, ts.URL+"/redirect", request); err == nil {
		t.Error("followed a redirect with the bearer token")
	}
	for name, bad := range map[string]AccessRequest{
		"no token":     {Limit: 1},
		"token space":  {AccessToken: "a b"},
		"limit high":   {Limit: MaxAccessRelays + 1, AccessToken: "t"},
		"limit low":    {Limit: -1, AccessToken: "t"},
		"both targets": {RelayID: "r1", Country: "JP", AccessToken: "t"},
	} {
		if _, err := client.RequestAccess(ctx, ts.URL, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClientRequestAccessRefusesEndpointUnboundFront(t *testing.T) {
	client := NewClient(nil, Options{})
	request := AccessRequest{AccessToken: "t"}
	if _, err := client.RequestAccess(context.Background(), AzureBrokerURL, request); err == nil || !strings.Contains(err.Error(), "exact broker endpoint") {
		t.Fatalf("access on the Azure no-SNI front: %v", err)
	}
}

func TestSignUserAuthWithExternalKey(t *testing.T) {
	key := testUserKey(t)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(digest []byte) ([]byte, error) {
		r, s, err := ecdsa.Sign(rand.Reader, key, digest)
		if err != nil {
			return nil, err
		}
		raw := make([]byte, 64)
		r.FillBytes(raw[:32])
		s.FillBytes(raw[32:])
		return raw, nil
	}
	request, err := SignUserAuthWith(spki, "abc", sign)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParseUserPublicKey(request.PublicKey)
	if err != nil || !VerifyUserAuth(pub, "abc", request.Signature) {
		t.Fatalf("external-key request does not verify: %v", err)
	}

	other := testUserKey(t)
	wrongKey := func(digest []byte) ([]byte, error) {
		r, s, _ := ecdsa.Sign(rand.Reader, other, digest)
		raw := make([]byte, 64)
		r.FillBytes(raw[:32])
		s.FillBytes(raw[32:])
		return raw, nil
	}
	if _, err := SignUserAuthWith(spki, "abc", wrongKey); err == nil {
		t.Error("a signature from another key was accepted")
	}
	short := func([]byte) ([]byte, error) { return make([]byte, 63), nil }
	if _, err := SignUserAuthWith(spki, "abc", short); err == nil {
		t.Error("a short signature was accepted")
	}
	if _, err := SignUserAuthWith(spki, "has space", sign); err == nil {
		t.Error("an invalid challenge was signed")
	}
	digest := sha256.Sum256([]byte("x"))
	if _, err := SignUserAuthWith([]byte("not der"), "abc", func([]byte) ([]byte, error) { return digest[:], nil }); err == nil {
		t.Error("a malformed public key was accepted")
	}
}

func TestAccessURL(t *testing.T) {
	got, err := AccessURL("https://broker.example/base/")
	if err != nil || got != "https://broker.example/base/api/v1/access" {
		t.Fatalf("AccessURL = %q %v", got, err)
	}
}
