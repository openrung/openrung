// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
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

func testUserKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestUserAuthSignVerifyRoundTrip(t *testing.T) {
	key := testUserKey(t)
	const challenge = "c2FtcGxlLWNoYWxsZW5nZS12YWx1ZQ"
	request, err := SignUserAuth(key, challenge)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParseUserPublicKey(request.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("parsed public key differs from the signing key")
	}
	if !VerifyUserAuth(pub, challenge, request.Signature) {
		t.Fatal("a freshly produced signature does not verify")
	}
	raw, _ := base64.StdEncoding.DecodeString(request.Signature)
	if len(raw) != 64 {
		t.Fatalf("signature is %d bytes, want the raw 64-byte r||s form", len(raw))
	}

	other := testUserKey(t)
	if VerifyUserAuth(&other.PublicKey, challenge, request.Signature) {
		t.Fatal("signature verified under a different key")
	}
	if VerifyUserAuth(pub, challenge+"x", request.Signature) {
		t.Fatal("signature verified for a different challenge")
	}
	raw[10] ^= 0x01
	if VerifyUserAuth(pub, challenge, base64.StdEncoding.EncodeToString(raw)) {
		t.Fatal("a tampered signature verified")
	}
	if VerifyUserAuth(pub, challenge, base64.StdEncoding.EncodeToString(raw[:63])) {
		t.Fatal("a truncated signature verified")
	}
}

func TestCanonicalUserPublicKeyIsEncodingIndependent(t *testing.T) {
	key := testUserKey(t)
	canonical, err := CanonicalUserPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	point, _ := base64.StdEncoding.DecodeString(canonical)
	if len(point) != 65 || point[0] != 0x04 {
		t.Fatalf("canonical key is not a 65-byte uncompressed point: %x", point)
	}
	// Re-parse through SPKI and compare: the canonical form must not depend
	// on how the key reached us.
	encoded, err := EncodeUserPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseUserPublicKey(" " + encoded + "\n")
	if err != nil {
		t.Fatal(err)
	}
	again, err := CanonicalUserPublicKey(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if again != canonical {
		t.Fatalf("canonical form drifted: %q vs %q", again, canonical)
	}
}

func TestParseUserPublicKeyAcceptsOnlyP256ECDSA(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string]string{
		"P-384":        base64.StdEncoding.EncodeToString(der),
		"not base64":   "%%%",
		"not DER":      base64.StdEncoding.EncodeToString([]byte("hello")),
		"empty":        "",
		"raw point":    "BAEC",
		"url base64":   strings.ReplaceAll(base64.StdEncoding.EncodeToString(der), "+", "-"),
		"leading junk": "x" + base64.StdEncoding.EncodeToString(der),
	} {
		if _, err := ParseUserPublicKey(encoded); err == nil {
			t.Errorf("%s: parsed as a user key, want an error", name)
		}
	}
	if _, err := EncodeUserPublicKey(&p384.PublicKey); err == nil {
		t.Error("EncodeUserPublicKey accepted a P-384 key")
	}
	if _, err := SignUserAuth(p384, "abc"); err == nil {
		t.Error("SignUserAuth accepted a P-384 key")
	}
}

func TestValidUserChallenge(t *testing.T) {
	for challenge, want := range map[string]bool{
		"":              false,
		"abc":           true,
		"with space":    false,
		"with\nnewline": false,
		"tab\there":     false,
		"\x7f":          false,
		"ünïcode":       false,
		strings.Repeat("a", MaxUserChallengeBytes):   true,
		strings.Repeat("a", MaxUserChallengeBytes+1): false,
	} {
		if got := ValidUserChallenge(challenge); got != want {
			t.Errorf("ValidUserChallenge(%q) = %v, want %v", challenge, got, want)
		}
	}
	if _, err := SignUserAuth(testUserKey(t), "has space"); err == nil {
		t.Error("SignUserAuth signed a challenge with whitespace")
	}
}

func TestUserAuthStatement(t *testing.T) {
	got := string(UserAuthStatement("CHAL", "KEY"))
	if got != "openrung-user-auth-v1\nCHAL\nKEY" {
		t.Fatalf("statement = %q", got)
	}
}

// userAuthTestServer is a loopback broker speaking the two user endpoints.
type userAuthTestServer struct {
	*httptest.Server
	challenges map[string]bool
	mu         chan struct{}
}

func newUserAuthTestServer(t *testing.T, onAuth func(w http.ResponseWriter, req UserAuthRequest)) *userAuthTestServer {
	t.Helper()
	ts := &userAuthTestServer{challenges: map[string]bool{}, mu: make(chan struct{}, 1)}
	ts.mu <- struct{}{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/challenge", func(w http.ResponseWriter, r *http.Request) {
		<-ts.mu
		defer func() { ts.mu <- struct{}{} }()
		challenge := fmt.Sprintf("challenge-%d", len(ts.challenges)+1)
		ts.challenges[challenge] = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(UserChallenge{Challenge: challenge, ExpiresAt: time.Now().Add(time.Minute)})
	})
	mux.HandleFunc("POST /api/v1/users/auth", func(w http.ResponseWriter, r *http.Request) {
		var req UserAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-OpenRung-Client-ID") != "" || r.Header.Get("X-OpenRung-Session-ID") != "" {
			http.Error(w, "identity headers must not accompany user auth", http.StatusBadRequest)
			return
		}
		onAuth(w, req)
	})
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestClientUserAuthHappyPath(t *testing.T) {
	key := testUserKey(t)
	ts := newUserAuthTestServer(t, func(w http.ResponseWriter, req UserAuthRequest) {
		pub, err := ParseUserPublicKey(req.PublicKey)
		if err != nil || !VerifyUserAuth(pub, req.Challenge, req.Signature) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(UserAuthResponse{
			UserID: "user_0123456789abcdef0123456789abcdef", Status: UserStatusEnabled,
			AccessToken: "tok", TokenExpiresAt: time.Now().Add(time.Hour),
		})
	})
	client := NewClient(ts.Client(), Options{AppVersion: "test"})
	ctx := context.Background()

	challenge, err := client.RequestUserChallenge(ctx, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	request, err := SignUserAuth(key, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.AuthenticateUser(ctx, ts.URL, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.UserID == "" || response.AccessToken != "tok" || response.Status != UserStatusEnabled {
		t.Fatalf("unexpected response %s", response)
	}
	if text := fmt.Sprintf("%v %+v %#v %s", response, response, response, response); strings.Contains(text, "tok") {
		t.Fatalf("formatted response leaks the token: %s", text)
	}
}

func TestClientUserAuthStatusErrorsAndValidation(t *testing.T) {
	key := testUserKey(t)
	var status int
	var retryAfter string
	var body string
	ts := newUserAuthTestServer(t, func(w http.ResponseWriter, _ UserAuthRequest) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	client := NewClient(ts.Client(), Options{})
	ctx := context.Background()
	request, err := SignUserAuth(key, "abc")
	if err != nil {
		t.Fatal(err)
	}

	status, retryAfter, body = http.StatusTooManyRequests, "120", `{"error":"budget"}`
	_, err = client.AuthenticateUser(ctx, ts.URL, request)
	var statusErr *UserAuthStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 429 || statusErr.RetryAfter != 2*time.Minute {
		t.Fatalf("429 → %v, want UserAuthStatusError{429, 2m}", err)
	}

	status, retryAfter, body = http.StatusForbidden, "", `{"user_id":"user_x","status":"disabled"}`
	_, err = client.AuthenticateUser(ctx, ts.URL, request)
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 403 || statusErr.Code != "" {
		t.Fatalf("403 → %v, want a code-less UserAuthStatusError (disabled user)", err)
	}

	// The datacenter-source refusal shares the 403 and is told apart by its
	// code; the 503 for missing classification data carries Retry-After.
	status, retryAfter, body = http.StatusForbidden, "", `{"error":"enrollment is not available from this network","code":"datacenter_source"}`
	_, err = client.AuthenticateUser(ctx, ts.URL, request)
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 403 || statusErr.Code != UserAuthCodeDatacenterSource {
		t.Fatalf("datacenter 403 → %v, want code %q", err, UserAuthCodeDatacenterSource)
	}
	if !strings.Contains(err.Error(), UserAuthCodeDatacenterSource) || strings.Contains(err.Error(), "network") {
		t.Fatalf("error text %q should name the code and not echo the body", err)
	}
	status, retryAfter, body = http.StatusServiceUnavailable, "300", `{"error":"retry later","code":"source_unclassified"}`
	_, err = client.AuthenticateUser(ctx, ts.URL, request)
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 503 || statusErr.Code != UserAuthCodeSourceUnclassified || statusErr.RetryAfter != 5*time.Minute {
		t.Fatalf("unclassified 503 → %+v", statusErr)
	}
	for name, bad := range map[string]string{
		"not json":   `datacenter_source`,
		"uppercase":  `{"code":"DATACENTER_SOURCE"}`,
		"spaces":     `{"code":"datacenter source"}`,
		"oversized":  `{"code":"` + strings.Repeat("a", 65) + `"}`,
		"wrong type": `{"code":7}`,
	} {
		status, retryAfter, body = http.StatusForbidden, "", bad
		_, err = client.AuthenticateUser(ctx, ts.URL, request)
		if !errors.As(err, &statusErr) || statusErr.Code != "" {
			t.Errorf("%s: code %q surfaced from a malformed body", name, statusErr.Code)
		}
	}

	for name, bad := range map[string]string{
		"no user id":     `{"status":"enabled","access_token":"t","token_expires_at":"2999-01-01T00:00:00Z"}`,
		"unknown status": `{"user_id":"u","status":"odd","access_token":"t","token_expires_at":"2999-01-01T00:00:00Z"}`,
		"no token":       `{"user_id":"u","status":"enabled","token_expires_at":"2999-01-01T00:00:00Z"}`,
		"expired token":  `{"user_id":"u","status":"enabled","access_token":"t","token_expires_at":"2000-01-01T00:00:00Z"}`,
		"token newline":  `{"user_id":"u","status":"enabled","access_token":"t\nx","token_expires_at":"2999-01-01T00:00:00Z"}`,
		"not json":       `nope`,
	} {
		status, body = http.StatusOK, bad
		if _, err := client.AuthenticateUser(ctx, ts.URL, request); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := client.AuthenticateUser(ctx, ts.URL, UserAuthRequest{PublicKey: "k", Challenge: "has space", Signature: "s"}); err == nil {
		t.Error("accepted a challenge with whitespace")
	}
	if _, err := client.AuthenticateUser(ctx, ts.URL, UserAuthRequest{Challenge: "abc"}); err == nil {
		t.Error("accepted a request without key and signature")
	}
}

func TestClientUserAuthRefusesEndpointUnboundFront(t *testing.T) {
	client := NewClient(nil, Options{})
	if _, err := client.RequestUserChallenge(context.Background(), AzureBrokerURL); err == nil || !strings.Contains(err.Error(), "exact broker endpoint") {
		t.Fatalf("challenge on the Azure no-SNI front: %v", err)
	}
	request, _ := SignUserAuth(testUserKey(t), "abc")
	if _, err := client.AuthenticateUser(context.Background(), AzureBrokerURL, request); err == nil || !strings.Contains(err.Error(), "exact broker endpoint") {
		t.Fatalf("auth on the Azure no-SNI front: %v", err)
	}
}

func TestClientUserChallengeValidation(t *testing.T) {
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/challenge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client := NewClient(ts.Client(), Options{})
	for name, bad := range map[string]string{
		"empty":      `{"challenge":"","expires_at":"2999-01-01T00:00:00Z"}`,
		"whitespace": `{"challenge":"a b","expires_at":"2999-01-01T00:00:00Z"}`,
		"expired":    `{"challenge":"abc","expires_at":"2000-01-01T00:00:00Z"}`,
		"no expiry":  `{"challenge":"abc"}`,
	} {
		body = bad
		if _, err := client.RequestUserChallenge(context.Background(), ts.URL); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUserURLs(t *testing.T) {
	challenge, err := UserChallengeURL("https://broker.example.org/")
	if err != nil || challenge != "https://broker.example.org/api/v1/users/challenge" {
		t.Fatalf("challenge URL = %q, %v", challenge, err)
	}
	auth, err := UserAuthURL("https://broker.example.org")
	if err != nil || auth != "https://broker.example.org/api/v1/users/auth" {
		t.Fatalf("auth URL = %q, %v", auth, err)
	}
}

// azureProbeTransport stands in for the network on the native Azure front:
// it records whether a request reached it and in which TLS mode, and answers
// a valid challenge.
type azureProbeTransport struct {
	calls   int
	sniMode bool
}

func (t *azureProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	t.sniMode = AzureSNIRequested(r.Context())
	body := `{"challenge":"abc","expires_at":"2999-01-01T00:00:00Z"}`
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// The native Azure front is endpoint-unbound only in its no-SNI fallback
// mode. With the hostname-verified SNI mode selected (as discovery does when
// that mode won), user authentication proceeds; without it, it is refused
// before any request leaves the client.
func TestClientUserAuthAzureFrontDependsOnSNIMode(t *testing.T) {
	probe := &azureProbeTransport{}
	client := NewClient(&http.Client{Transport: probe}, Options{})
	request, _ := SignUserAuth(testUserKey(t), "abc")

	ctx := WithAzureSNI(context.Background(), true)
	if _, err := client.RequestUserChallenge(ctx, AzureBrokerURL); err != nil {
		t.Fatalf("challenge on Azure with SNI: %v", err)
	}
	if probe.calls != 1 || !probe.sniMode {
		t.Fatalf("SNI-mode challenge reached the transport %d times (sni=%v), want once in SNI mode", probe.calls, probe.sniMode)
	}
	if _, err := client.AuthenticateUser(ctx, AzureBrokerURL, request); err == nil || probe.calls != 2 || !probe.sniMode {
		// The probe answers a challenge body, so decoding fails after the
		// request was made; what matters is that it was made, in SNI mode.
		t.Fatalf("auth on Azure with SNI: calls=%d sni=%v err=%v", probe.calls, probe.sniMode, err)
	}

	probe.calls = 0
	noSNI := WithAzureSNI(context.Background(), false)
	for name, call := range map[string]func() error{
		"challenge": func() error { _, err := client.RequestUserChallenge(noSNI, AzureBrokerURL); return err },
		"auth":      func() error { _, err := client.AuthenticateUser(noSNI, AzureBrokerURL, request); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "exact broker endpoint") {
			t.Errorf("%s on Azure without SNI: %v, want refusal", name, err)
		}
	}
	if probe.calls != 0 {
		t.Fatalf("no-SNI requests reached the transport %d times", probe.calls)
	}
	// Other fronts never consult the mode.
	if _, err := client.RequestUserChallenge(noSNI, CloudFrontBrokerURL); err != nil {
		t.Fatalf("CloudFront without the Azure flag: %v", err)
	}
}
