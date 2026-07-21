package relay

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func wssTestFronts(t *testing.T) []WSSFrontDescriptor {
	t.Helper()
	fronts, err := NormalizeWSSFronts([]WSSFrontDescriptor{
		{ID: " Tehran-B ", URL: " WSS://D222222ABCDEF8.CLOUDFRONT.NET/api/v1/wss-bridge ", ProtocolVersion: WSSProtocolVersion},
		{ID: "tehran-a", URL: "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
	})
	if err != nil {
		t.Fatalf("normalize test fronts: %v", err)
	}
	return fronts
}

func signedWSSTestRequest(t *testing.T, now time.Time) RegisterRequest {
	t.Helper()
	priv := identityTestKey(t)
	req := identityTestRequest()
	req.NodeClass = NodeClassFoundation
	req.Transport = TransportDirect
	req.WSSFronts = wssTestFronts(t)
	expiresAt := now.Add(WSSCapabilityProofTTL)
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = SignIdentity(priv, req, expiresAt)
	var err error
	req.WSSCapabilityProof, req.WSSCapabilityExpiresAt, err = SignWSSCapability(priv, req, expiresAt)
	if err != nil {
		t.Fatalf("sign WSS capability: %v", err)
	}
	return req
}

func TestNormalizeWSSFrontsCanonicalizesAndSorts(t *testing.T) {
	fronts := wssTestFronts(t)
	want := []WSSFrontDescriptor{
		{ID: "tehran-a", URL: "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		{ID: "tehran-b", URL: "wss://d222222abcdef8.cloudfront.net/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
	}
	if !slices.Equal(fronts, want) {
		t.Fatalf("normalized fronts = %#v, want %#v", fronts, want)
	}

	copyOfInput := []WSSFrontDescriptor{{ID: "front-a", URL: "wss://cdn.example/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion}}
	if _, err := NormalizeWSSFronts(copyOfInput); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if copyOfInput[0].ID != "front-a" {
		t.Fatal("NormalizeWSSFronts mutated its input")
	}
}

func TestNormalizeWSSFrontsRejectsNonCDNOrAmbiguousURLs(t *testing.T) {
	valid := WSSFrontDescriptor{ID: "front-a", URL: "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion}
	tests := map[string]WSSFrontDescriptor{
		"missing ID":         {URL: valid.URL, ProtocolVersion: WSSProtocolVersion},
		"invalid ID":         {ID: "front/a", URL: valid.URL, ProtocolVersion: WSSProtocolVersion},
		"wrong version":      {ID: valid.ID, URL: valid.URL, ProtocolVersion: WSSProtocolVersion + 1},
		"plaintext":          {ID: valid.ID, URL: "ws://cdn.example/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"https":              {ID: valid.ID, URL: "https://cdn.example/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"raw IPv4":           {ID: valid.ID, URL: "wss://192.0.2.10/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"raw IPv6":           {ID: valid.ID, URL: "wss://[2001:db8::1]/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"single label":       {ID: valid.ID, URL: "wss://localhost/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"explicit 443":       {ID: valid.ID, URL: "wss://cdn.example:443/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"empty port":         {ID: valid.ID, URL: "wss://cdn.example:/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"custom port":        {ID: valid.ID, URL: "wss://cdn.example:8443/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"userinfo":           {ID: valid.ID, URL: "wss://user@cdn.example/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion},
		"wrong path":         {ID: valid.ID, URL: "wss://cdn.example/another-relay", ProtocolVersion: WSSProtocolVersion},
		"escaped path":       {ID: valid.ID, URL: "wss://cdn.example/api/v1/wss%2Dbridge", ProtocolVersion: WSSProtocolVersion},
		"query":              {ID: valid.ID, URL: valid.URL + "?ticket=secret", ProtocolVersion: WSSProtocolVersion},
		"empty query":        {ID: valid.ID, URL: valid.URL + "?", ProtocolVersion: WSSProtocolVersion},
		"fragment":           {ID: valid.ID, URL: valid.URL + "#relay", ProtocolVersion: WSSProtocolVersion},
		"control whitespace": {ID: valid.ID, URL: valid.URL + "\n", ProtocolVersion: WSSProtocolVersion},
	}
	for name, front := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeWSSFronts([]WSSFrontDescriptor{front}); !errors.Is(err, ErrWSSFrontInvalid) {
				t.Fatalf("NormalizeWSSFronts() error = %v, want ErrWSSFrontInvalid", err)
			}
		})
	}

	for name, fronts := range map[string][]WSSFrontDescriptor{
		"duplicate ID":  {valid, {ID: valid.ID, URL: "wss://other.example/api/v1/wss-bridge", ProtocolVersion: WSSProtocolVersion}},
		"duplicate URL": {valid, {ID: "front-b", URL: valid.URL, ProtocolVersion: WSSProtocolVersion}},
		"too many":      {valid, valid, valid, valid, valid},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeWSSFronts(fronts); !errors.Is(err, ErrWSSFrontInvalid) {
				t.Fatalf("NormalizeWSSFronts() error = %v, want ErrWSSFrontInvalid", err)
			}
		})
	}
}

func TestWSSCapabilityRoundTripAndCanonicalStatement(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	req := signedWSSTestRequest(t, now)
	if err := VerifyWSSCapability(req, now); err != nil {
		t.Fatalf("VerifyWSSCapability: %v", err)
	}

	statement, err := WSSCapabilityStatement(req, req.WSSCapabilityExpiresAt)
	if err != nil {
		t.Fatalf("WSSCapabilityStatement: %v", err)
	}
	const wantStatement = `openrung-relay-wss-capability-v1
2026-07-22T12:00:00Z
eyJpZGVudGl0eV9zdGF0ZW1lbnRfc2hhMjU2IjoiUXpKRUQrYU5DbmJ0WnVWamMyMFUxS1V1UHM4aVZUajNxYmxiNGF1d1BqNCIsImZyb250cyI6W3siaWQiOiJ0ZWhyYW4tYSIsInVybCI6IndzczovL2QxMTExMTFhYmNkZWY4LmNsb3VkZnJvbnQubmV0L2FwaS92MS93c3MtYnJpZGdlIiwicHJvdG9jb2xfdmVyc2lvbiI6MX0seyJpZCI6InRlaHJhbi1iIiwidXJsIjoid3NzOi8vZDIyMjIyMmFiY2RlZjguY2xvdWRmcm9udC5uZXQvYXBpL3YxL3dzcy1icmlkZ2UiLCJwcm90b2NvbF92ZXJzaW9uIjoxfV19`
	const wantProof = "KFJP8ZdRiSZT9rbqPxtVIRYbWI3MBDfEMhSjmb/8pMGxPsbIjdq+/MEK86ZHXwhHDONFznv7joTKJYcJ0YI/Bg=="
	if string(statement) != wantStatement || req.WSSCapabilityProof != wantProof {
		t.Fatalf("WSS capability golden changed:\n statement=%q\n proof=%q", statement, req.WSSCapabilityProof)
	}
	parts := strings.Split(string(statement), "\n")
	if len(parts) != 3 || parts[0] != WSSCapabilitySpecV1 || parts[1] != req.IdentityExpiresAt {
		t.Fatalf("unexpected canonical statement framing: %q", statement)
	}
	if _, err := base64.RawStdEncoding.DecodeString(parts[2]); err != nil {
		t.Fatalf("statement payload is not canonical raw-base64: %v", err)
	}
}

func TestWSSCapabilityDoesNotAlterIdentitySpecV1Bytes(t *testing.T) {
	req := identityTestRequest()
	expires := "2026-07-21T12:00:00Z"
	want := append([]byte(nil), IdentityStatement(req, expires)...)
	req.WSSFronts = wssTestFronts(t)
	req.WSSCapabilityProof = "new-field-must-stay-outside-v1"
	req.WSSCapabilityExpiresAt = expires
	if got := IdentityStatement(req, expires); !slices.Equal(got, want) {
		t.Fatalf("WSS fields changed IdentitySpecV1 bytes:\n got %q\nwant %q", got, want)
	}
}

func TestWSSCapabilityJSONKeepsProofPrivateToRegistration(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	req := signedWSSTestRequest(t, now)
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	var decoded RegisterRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal registration: %v", err)
	}
	if !slices.Equal(decoded.WSSFronts, req.WSSFronts) || decoded.WSSCapabilityProof != req.WSSCapabilityProof {
		t.Fatalf("registration WSS fields did not round trip: %+v", decoded)
	}

	descriptorJSON, err := json.Marshal(Descriptor{ID: "relay-a", WSSFronts: slices.Clone(req.WSSFronts)})
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if strings.Contains(string(descriptorJSON), "wss_capability_proof") || !strings.Contains(string(descriptorJSON), "wss_fronts") {
		t.Fatalf("public descriptor proof/front shape is wrong: %s", descriptorJSON)
	}
}

func TestVerifyWSSCapabilityBindsIdentityAndExactFronts(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	priv := identityTestKey(t)
	base := signedWSSTestRequest(t, now)

	mutations := map[string]func(*RegisterRequest){
		"front ID":          func(req *RegisterRequest) { req.WSSFronts[0].ID = "other-front" },
		"front URL":         func(req *RegisterRequest) { req.WSSFronts[0].URL = "wss://other.example/api/v1/wss-bridge" },
		"front version":     func(req *RegisterRequest) { req.WSSFronts[0].ProtocolVersion++ },
		"front removed":     func(req *RegisterRequest) { req.WSSFronts = req.WSSFronts[:1] },
		"front reordered":   func(req *RegisterRequest) { req.WSSFronts[0], req.WSSFronts[1] = req.WSSFronts[1], req.WSSFronts[0] },
		"capability expiry": func(req *RegisterRequest) { req.WSSCapabilityExpiresAt = now.Add(time.Hour).Format(time.RFC3339) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			req := base
			req.WSSFronts = slices.Clone(base.WSSFronts)
			mutate(&req)
			if err := VerifyWSSCapability(req, now); !errors.Is(err, ErrWSSCapabilityInvalid) && !errors.Is(err, ErrWSSCapabilityIncomplete) {
				t.Fatalf("VerifyWSSCapability() error = %v, want invalid capability", err)
			}
		})
	}

	// Re-signing a changed ordinary identity statement is insufficient: the old
	// capability proof remains bound to the original statement digest.
	changedIdentity := base
	changedIdentity.PublicHost = "relay-new.example"
	expiresAt, err := time.Parse(time.RFC3339, changedIdentity.IdentityExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	changedIdentity.IdentityPublicKey, changedIdentity.IdentityProof, changedIdentity.IdentityExpiresAt = SignIdentity(priv, changedIdentity, expiresAt)
	if _, err := VerifyIdentity(changedIdentity, now); err != nil {
		t.Fatalf("changed identity precondition did not verify: %v", err)
	}
	if err := VerifyWSSCapability(changedIdentity, now); !errors.Is(err, ErrWSSCapabilityInvalid) {
		t.Fatalf("changed identity accepted old WSS proof: %v", err)
	}
}

func TestVerifyWSSCapabilityRequiresEligibleStableRelay(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	base := signedWSSTestRequest(t, now)
	for name, mutate := range map[string]func(*RegisterRequest){
		"volunteer":      func(req *RegisterRequest) { req.NodeClass = NodeClassVolunteer },
		"tunnel":         func(req *RegisterRequest) { req.Transport = TransportTunnel },
		"dedicated exit": func(req *RegisterRequest) { req.ExitMode = ExitModeDedicated },
		"port":           func(req *RegisterRequest) { req.PublicPort = 8443 },
		"no identity": func(req *RegisterRequest) {
			req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt = "", "", ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			mutate(&req)
			if err := VerifyWSSCapability(req, now); !errors.Is(err, ErrWSSCapabilityInvalid) {
				t.Fatalf("VerifyWSSCapability() error = %v, want ErrWSSCapabilityInvalid", err)
			}
		})
	}
}

func TestVerifyWSSCapabilityRejectsPartialExpiredAndWrongSigner(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	base := signedWSSTestRequest(t, now)

	partial := base
	partial.WSSCapabilityProof = ""
	if err := VerifyWSSCapability(partial, now); !errors.Is(err, ErrWSSCapabilityIncomplete) {
		t.Fatalf("partial capability error = %v, want ErrWSSCapabilityIncomplete", err)
	}
	if err := VerifyWSSCapability(base, now.Add(WSSCapabilityProofTTL)); !errors.Is(err, ErrWSSCapabilityExpired) {
		t.Fatalf("expired capability error = %v, want ErrWSSCapabilityExpired", err)
	}

	wrongSeed := make([]byte, ed25519.SeedSize)
	wrongSeed[0] = 1
	wrongKey := ed25519.NewKeyFromSeed(wrongSeed)
	expiresAt, err := time.Parse(time.RFC3339, base.IdentityExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := SignWSSCapability(wrongKey, base, expiresAt); !errors.Is(err, ErrWSSCapabilityInvalid) {
		t.Fatalf("wrong signer error = %v, want ErrWSSCapabilityInvalid", err)
	}
}
