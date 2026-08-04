package brokerapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type vectorsFile struct {
	SignatureHeader string `json:"signature_header"`
	SpecVector      struct {
		SeedB64      string `json:"seed_b64"`
		PubKeyHex    string `json:"pubkey_hex"`
		KeyID        string `json:"key_id"`
		Body         string `json:"body"`
		SignatureB64 string `json:"signature_b64"`
		Header       string `json:"header"`
	} `json:"spec_vector"`
	PinnedKeys []struct {
		Role               string `json:"role"`
		PubKeyHex          string `json:"pubkey_hex"`
		KeyID              string `json:"key_id"`
		VectorMessage      string `json:"vector_message"`
		VectorSignatureB64 string `json:"vector_signature_b64"`
	} `json:"pinned_keys"`
}

func loadVectors(t *testing.T) vectorsFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/signing_vectors.json")
	if err != nil {
		t.Fatalf("read signing vectors: %v", err)
	}
	var vectors vectorsFile
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse signing vectors: %v", err)
	}
	return vectors
}

func vectorSigner(t *testing.T, vectors vectorsFile) ed25519.PrivateKey {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(vectors.SpecVector.SeedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("decode vector seed: %v", err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func signedHeader(key ed25519.PrivateKey, keyID string, body []byte) string {
	return "ed25519;" + keyID + ";" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
}

var vectorNow = time.Date(2026, 7, 10, 0, 5, 0, 0, time.UTC)

func TestSpecVectorVerifiesExactBytes(t *testing.T) {
	vectors := loadVectors(t)
	result, err := verifyRelayList(
		mustPinnedKeys(vectors.SpecVector.PubKeyHex),
		"https://broker.example/api/v1/relays?limit=1",
		1,
		vectors.SpecVector.Header,
		[]byte(vectors.SpecVector.Body),
		vectorNow,
	)
	if err != nil {
		t.Fatalf("verify spec vector: %v", err)
	}
	if result.KeyID != vectors.SpecVector.KeyID || !result.SignatureVerified {
		t.Fatalf("verification metadata = %+v", result)
	}
	if string(result.JSON()) != vectors.SpecVector.Body {
		t.Fatal("verified response bytes changed")
	}
	copyForCaller := result.JSON()
	copyForCaller[0] ^= 1
	if string(result.JSON()) != vectors.SpecVector.Body {
		t.Fatal("caller mutation changed retained verified response bytes")
	}

	mutated := []byte(vectors.SpecVector.Body)
	mutated[len(mutated)/2] ^= 1
	if _, err := verifyRelayList(
		mustPinnedKeys(vectors.SpecVector.PubKeyHex),
		"https://broker.example/api/v1/relays?limit=1",
		1,
		vectors.SpecVector.Header,
		mutated,
		vectorNow,
	); err == nil {
		t.Fatal("mutated exact body verified")
	}
}

func TestSigningVectorAndProductionPinsCIGuard(t *testing.T) {
	vectors := loadVectors(t)
	if vectors.SignatureHeader != RelaySignatureHeader {
		t.Fatalf("vector header = %q, code = %q", vectors.SignatureHeader, RelaySignatureHeader)
	}
	pub := vectorSigner(t, vectors).Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub) != vectors.SpecVector.PubKeyHex {
		t.Fatal("vector seed does not derive vector public key")
	}
	if signingKeyID(pub) != vectors.SpecVector.KeyID {
		t.Fatal("vector key ID derivation changed")
	}
	if len(vectors.PinnedKeys) != 2 || len(productionRelayKeys) != 2 {
		t.Fatalf("production pin count = %d/%d, want 2/2", len(vectors.PinnedKeys), len(productionRelayKeys))
	}
	for index, entry := range vectors.PinnedKeys {
		pin := productionRelayKeys[index]
		if hex.EncodeToString(pin.key) != entry.PubKeyHex || pin.id != entry.KeyID {
			t.Fatalf("%s pin does not match committed vector", entry.Role)
		}
		signature, err := base64.StdEncoding.DecodeString(entry.VectorSignatureB64)
		if err != nil || !ed25519.Verify(pin.key, []byte(entry.VectorMessage), signature) {
			t.Fatalf("%s promotion vector does not verify: %v", entry.Role, err)
		}
	}
}

func TestVerifyRelayListRejectsEveryEnvelopeFailure(t *testing.T) {
	vectors := loadVectors(t)
	keys := mustPinnedKeys(vectors.SpecVector.PubKeyHex)
	signer := vectorSigner(t, vectors)
	body := []byte(vectors.SpecVector.Body)

	signature, err := base64.StdEncoding.DecodeString(vectors.SpecVector.SignatureB64)
	if err != nil {
		t.Fatal(err)
	}
	flippedSignature := append([]byte(nil), signature...)
	flippedSignature[0] ^= 1
	mirrorBody := []byte(`{"not_after":"2026-07-10T00:30:00Z","channel":"mirror","limit":1,"relays":[]}`)
	// The broker's operational inventory channel is signed with the SAME key
	// as the API channel, so a validly signed inventory snapshot — untruncated
	// and in a different order than any client page — must be rejected here on
	// the channel field alone. It carries no limit either, so the echo check
	// must never be what saves us.
	inventoryBody := []byte(`{"not_after":"2026-07-10T00:30:00Z","channel":"inventory","relays":[]}`)
	noExpiryBody := []byte(`{"channel":"api","limit":1,"relays":[]}`)
	invalidJSON := []byte(`{"channel":`)
	badRelayBody := []byte(`{"not_after":"2026-07-10T00:30:00Z","channel":"api","limit":1,"relays":[{"public_port":"not-an-integer"}]}`)
	validRelayBody := []byte(`{"count":1,"server_time":"2026-07-10T00:00:00Z","not_after":"2026-07-10T00:30:00Z","channel":"api","limit":1,"relays":[{"id":"r","public_host":"192.0.2.1","public_port":443,"protocol":"vless-reality-vision","client_id":"c","reality_public_key":"key","short_id":"01","server_name":"example.com","flow":"xtls-rprx-vision","exit_mode":"direct","max_sessions":1,"max_mbps":10,"volunteer_version":"1.0.0","registered_at":"2026-07-10T00:00:00Z","last_heartbeat_at":"2026-07-10T00:00:00Z","expires_at":"2026-07-10T00:10:00Z","wss_fronts":[{"id":"front","url":"wss://relay.example/bridge","protocol_version":1}]}]}`)
	if err := validateRelayListWire(validRelayBody); err != nil {
		t.Fatalf("valid relay wire fixture: %v", err)
	}
	missingRelayFieldBody := []byte(strings.Replace(string(validRelayBody), `"client_id":"c",`, "", 1))
	unknownWSSFieldBody := []byte(strings.Replace(string(validRelayBody), `"protocol_version":1`, `"protocol_version":1,"unexpected":true`, 1))

	tests := []struct {
		name       string
		keys       []pinnedKey
		header     string
		body       []byte
		limit      int
		now        time.Time
		wantReason string
	}{
		{"unpinned", productionRelayKeys, vectors.SpecVector.Header, body, 1, vectorNow, "does not verify"},
		{"missing header", keys, "", body, 1, vectorNow, "has not enabled"},
		{"malformed header", keys, "ed25519;short", body, 1, vectorNow, "want 3"},
		{"algorithm", keys, "rsa;x;" + vectors.SpecVector.SignatureB64, body, 1, vectorNow, "unsupported"},
		{"base64", keys, "ed25519;x;!!!", body, 1, vectorNow, "standard base64"},
		{"short signature", keys, "ed25519;x;" + base64.StdEncoding.EncodeToString(signature[:63]), body, 1, vectorNow, "63 bytes"},
		{"flipped signature", keys, "ed25519;x;" + base64.StdEncoding.EncodeToString(flippedSignature), body, 1, vectorNow, "does not verify"},
		{"expired", keys, vectors.SpecVector.Header, body, 1, time.Date(2026, 7, 10, 0, 35, 1, 0, time.UTC), "expired"},
		{"limit", keys, vectors.SpecVector.Header, body, 2, vectorNow, "echoed limit"},
		{"channel", keys, signedHeader(signer, vectors.SpecVector.KeyID, mirrorBody), mirrorBody, 1, vectorNow, "channel"},
		{"inventory channel", keys, signedHeader(signer, vectors.SpecVector.KeyID, inventoryBody), inventoryBody, 1, vectorNow, "channel"},
		{"expiry", keys, signedHeader(signer, vectors.SpecVector.KeyID, noExpiryBody), noExpiryBody, 1, vectorNow, "no not_after"},
		{"json", keys, signedHeader(signer, vectors.SpecVector.KeyID, invalidJSON), invalidJSON, 1, vectorNow, "not valid JSON"},
		{"relay wire schema", keys, signedHeader(signer, vectors.SpecVector.KeyID, badRelayBody), badRelayBody, 1, vectorNow, "wire schema"},
		{"missing relay field", keys, signedHeader(signer, vectors.SpecVector.KeyID, missingRelayFieldBody), missingRelayFieldBody, 1, vectorNow, "wire schema"},
		{"unknown WSS field", keys, signedHeader(signer, vectors.SpecVector.KeyID, unknownWSSFieldBody), unknownWSSFieldBody, 1, vectorNow, "wire schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifyRelayList(
				test.keys,
				"https://broker.example/api/v1/relays",
				test.limit,
				test.header,
				test.body,
				test.now,
			)
			if err == nil || !strings.Contains(err.Error(), "unsigned/invalid relay list") {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("error %q does not contain %q", err, test.wantReason)
			}
		})
	}
}

func TestVerifyRelayListKeyIDIsAdvisory(t *testing.T) {
	vectors := loadVectors(t)
	decoy, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := mustPinnedKeys(hex.EncodeToString(decoy), vectors.SpecVector.PubKeyHex)
	result, err := verifyRelayList(
		keys,
		"https://broker.example/api/v1/relays",
		1,
		"ed25519;deadbeefdeadbeef;"+vectors.SpecVector.SignatureB64,
		[]byte(vectors.SpecVector.Body),
		vectorNow,
	)
	if err != nil {
		t.Fatalf("advisory unknown key ID blocked fallback: %v", err)
	}
	if result.KeyID != vectors.SpecVector.KeyID {
		t.Fatalf("verified key ID = %q", result.KeyID)
	}
}

func TestVerifyRelayListLoopbackOnlyExemption(t *testing.T) {
	body := []byte(`{"count":0,"server_time":"2026-07-24T00:00:00Z","relays":[]}`)
	result, err := verifyRelayList(nil, "http://127.0.0.1:8080/api/v1/relays", 5, "", body, time.Now())
	if err != nil {
		t.Fatalf("loopback unsigned JSON: %v", err)
	}
	if result.SignatureVerified || string(result.JSON()) != string(body) {
		t.Fatalf("loopback result = %+v", result)
	}
	if _, err := verifyRelayList(nil, "https://broker.example/api/v1/relays", 5, "", body, time.Now()); err == nil {
		t.Fatal("non-loopback unsigned JSON was accepted")
	}
	if _, err := verifyRelayList(nil, "http://localhost.example/api/v1/relays", 5, "", body, time.Now()); err == nil {
		t.Fatal("localhost suffix received loopback exemption")
	}
}
