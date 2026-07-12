package punchcore

// TestGoldenVectors is the permanent wire-format regression net for the
// punchcore extraction. testdata/golden.json was captured from the code of
// BOTH pre-extraction implementations (openrung internal/punch and the Android
// punchbridge) — every shared vector was verified byte-identical between them,
// and the policy-bearing SanitizePeers output is recorded per preset. Any
// mismatch here is a wire-protocol or policy change and must be treated as one.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"testing"
)

func goldenToken() []byte {
	t := make([]byte, 32)
	for i := range t {
		t[i] = byte(i)
	}
	return t
}

func goldenNonce() []byte {
	n := make([]byte, 16)
	for i := range n {
		n[i] = 0xA0 + byte(i)
	}
	return n
}

func goldenSanitizeInput() []Endpoint {
	in := []Endpoint{
		{IP: "203.0.113.5", Port: 4433, Kind: "srflx"},
		{IP: "203.0.113.5", Port: 4433, Kind: "srflx"},  // dup
		{IP: "203.0.113.5", Port: 4434, Kind: "srflx"},  // same IP, new port
		{IP: "198.51.100.7", Port: 1000, Kind: "srflx"}, // second srflx IP
		{IP: "10.1.2.3", Port: 5000, Kind: "srflx"},     // private srflx
		{IP: "127.0.0.1", Port: 6000, Kind: "srflx"},    // loopback srflx
		{IP: "100.64.0.9", Port: 6001, Kind: "srflx"},   // CGNAT srflx
		{IP: "192.168.1.50", Port: 7000, Kind: "host"},
		{IP: "8.8.8.8", Port: 7001, Kind: "host"}, // routable host -> drop
		{IP: "224.0.0.1", Port: 7002, Kind: "host"},
		{IP: "0.0.0.0", Port: 7003, Kind: "host"},
		{IP: "192.168.1.51", Port: 0, Kind: "host"},
		{IP: "not-an-ip", Port: 7004, Kind: "host"},
		{IP: "192.168.1.52", Port: 70000, Kind: "host"},
		{IP: "192.168.1.53", Port: 7005, Kind: "bogus"},
		{IP: "fe80::1", Port: 7006, Kind: "host"},
		{IP: "fd00::2", Port: 7007, Kind: "host"},
		{IP: "2001:db8::9", Port: 7008, Kind: "srflx"}, // v6 srflx (third IP)
	}
	// bulk candidates to exercise per-kind caps (indexes matter for order)
	for i := 0; i < 12; i++ {
		in = append(in, Endpoint{IP: fmt.Sprintf("192.168.9.%d", i+1), Port: 8000 + i, Kind: "host"})
	}
	for i := 0; i < 12; i++ {
		in = append(in, Endpoint{IP: "203.0.113.5", Port: 9000 + i, Kind: "srflx"})
	}
	return in
}

type goldenReplyParse struct {
	OK    bool   `json:"ok"`
	Nonce string `json:"nonce,omitempty"`
	IP    string `json:"ip,omitempty"`
	Port  int    `json:"port,omitempty"`
}

type goldenRequestParse struct {
	Nonce string `json:"nonce"`
	OK    bool   `json:"ok"`
}

type goldenVectors struct {
	ComputeToken          string                      `json:"compute_token"`
	DecodeErrors          map[string]string           `json:"decode_errors"`
	DecodeRoundtrip       string                      `json:"decode_roundtrip"`
	EncodeToken           string                      `json:"encode_token"`
	JSON                  map[string]string           `json:"json"`
	NonceKey              string                      `json:"nonce_key"`
	ParseProbe            map[string]int              `json:"parse_probe"`
	ProbePackets          map[string]string           `json:"probe_packets"`
	ReflectReplyBuild     map[string]string           `json:"reflect_reply_build"`
	ReflectReplyParse     map[string]goldenReplyParse `json:"reflect_reply_parse"`
	ReflectRequest        string                      `json:"reflect_request"`
	ReflectRequestParse   goldenRequestParse          `json:"reflect_request_parse"`
	ReflectRequestShortOK bool                        `json:"reflect_request_short_ok"`
	SanitizeDesktop       []Endpoint                  `json:"sanitize_desktop"`
	SanitizeMobile        []Endpoint                  `json:"sanitize_mobile"`
	UDPAddrErrors         map[string]string           `json:"udpaddr_errors"`
}

// checkStringMap compares a recomputed map against the golden one key-by-key so
// a failure names the exact vector that regressed.
func checkStringMap(t *testing.T, section string, got, want map[string]string) {
	t.Helper()
	for key, w := range want {
		if g, ok := got[key]; !ok {
			t.Errorf("%s[%q]: not recomputed", section, key)
		} else if g != w {
			t.Errorf("%s[%q] = %q, want %q", section, key, g, w)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s[%q]: recomputed but absent from golden file", section, key)
		}
	}
}

func TestGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var want goldenVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}

	token := goldenToken()
	nonce := goldenNonce()

	// --- probe/ack packet framing ---
	probes := map[string]string{}
	for _, sid := range []string{"", "s", "sess-1234567890"} {
		probes["probe:"+sid] = hex.EncodeToString(buildProbePacket(probeMagic, sid, token))
		probes["ack:"+sid] = hex.EncodeToString(buildProbePacket(probeAckMagic, sid, token))
	}
	long := ""
	for i := 0; i < 128; i++ {
		long += "x"
	}
	probes["probe:long128"] = hex.EncodeToString(buildProbePacket(probeMagic, long, token))
	checkStringMap(t, "probe_packets", probes, want.ProbePackets)

	// --- parseProbePacket classification ---
	sid := "sess-1234567890"
	valid := buildProbePacket(probeMagic, sid, token)
	validAck := buildProbePacket(probeAckMagic, sid, token)
	wrongTok := goldenToken()
	wrongTok[0] ^= 0xFF
	trailing := append(append([]byte{}, valid...), 0xDE, 0xAD)
	parse := map[string]int{
		"valid_probe":     int(parseProbePacket(valid, sid, token)),
		"valid_ack":       int(parseProbePacket(validAck, sid, token)),
		"trailing_junk":   int(parseProbePacket(trailing, sid, token)),
		"wrong_token":     int(parseProbePacket(buildProbePacket(probeMagic, sid, wrongTok), sid, token)),
		"wrong_session":   int(parseProbePacket(buildProbePacket(probeMagic, "other", token), sid, token)),
		"truncated":       int(parseProbePacket(valid[:len(valid)-1], sid, token)),
		"empty":           int(parseProbePacket(nil, sid, token)),
		"wrong_magic":     int(parseProbePacket(append([]byte("ORWRONG"), valid[6:]...), sid, token)),
		"ack_as_probe_id": int(parseProbePacket(validAck, "other", token)),
	}
	for key, w := range want.ParseProbe {
		if g, ok := parse[key]; !ok {
			t.Errorf("parse_probe[%q]: not recomputed", key)
		} else if g != w {
			t.Errorf("parse_probe[%q] = %d, want %d", key, g, w)
		}
	}

	// --- reflector wire: request build + parse (incl. short-request rejection) ---
	if got := hex.EncodeToString(buildReflectRequest(nonce)); got != want.ReflectRequest {
		t.Errorf("reflect_request = %q, want %q", got, want.ReflectRequest)
	}
	rq, rqOK := parseReflectRequest(buildReflectRequest(nonce))
	if hex.EncodeToString(rq) != want.ReflectRequestParse.Nonce || rqOK != want.ReflectRequestParse.OK {
		t.Errorf("reflect_request_parse = (%q, %v), want (%q, %v)",
			hex.EncodeToString(rq), rqOK, want.ReflectRequestParse.Nonce, want.ReflectRequestParse.OK)
	}
	if _, shortOK := parseReflectRequest(buildReflectRequest(nonce)[:63]); shortOK != want.ReflectRequestShortOK {
		t.Errorf("reflect_request_short_ok = %v, want %v", shortOK, want.ReflectRequestShortOK)
	}

	// --- reflector wire: reply build v4+v6 + parse matrix ---
	v4 := buildReflectReply(nonce, &net.UDPAddr{IP: net.ParseIP("203.0.113.5"), Port: 44444})
	v6 := buildReflectReply(nonce, &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1234})
	checkStringMap(t, "reflect_reply_build", map[string]string{
		"v4": hex.EncodeToString(v4),
		"v6": hex.EncodeToString(v6),
	}, want.ReflectReplyBuild)

	parseReply := func(b []byte) goldenReplyParse {
		n, addr, ok := parseReflectReply(b)
		out := goldenReplyParse{OK: ok}
		if ok {
			out.Nonce = hex.EncodeToString(n)
			out.IP = addr.IP.String()
			out.Port = addr.Port
		}
		return out
	}
	gotReplies := map[string]goldenReplyParse{
		"v4":        parseReply(v4),
		"v6":        parseReply(v6),
		"truncated": parseReply(v4[:len(v4)-1]),
		"badmagic":  parseReply(append([]byte("ORPUNCHRX"), v4[9:]...)),
	}
	for key, w := range want.ReflectReplyParse {
		if g, ok := gotReplies[key]; !ok {
			t.Errorf("reflect_reply_parse[%q]: not recomputed", key)
		} else if g != w {
			t.Errorf("reflect_reply_parse[%q] = %+v, want %+v", key, g, w)
		}
	}

	// --- JSON encodings of every wire struct (full + zero values) ---
	ep := Endpoint{IP: "203.0.113.5", Port: 4433, Kind: "srflx"}
	jsonOut := map[string]string{}
	enc := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		jsonOut[name] = string(b)
	}
	enc("endpoint", ep)
	enc("config_full", PunchConfig{ReflectorAddrs: []string{"203.0.113.5:9000", "198.51.100.7:9000"}, ALPN: "openrung-punch/1", TTLMillis: 6000})
	enc("config_zero", PunchConfig{})
	enc("request_full", PunchRequest{RelayID: "relay-1", ClientNonce: "a0a1", ClientReflexive: []Endpoint{ep}, ClientLocal: []Endpoint{{IP: "192.168.1.2", Port: 4433, Kind: "host"}}, ClientClass: "eim", QUICALPN: "openrung-punch/1", ProtoVersion: 1})
	enc("request_zero", PunchRequest{})
	enc("response_full", PunchResponse{OK: true, Error: "e", SessionID: "sess", VolunteerReflexive: []Endpoint{ep}, VolunteerLocal: []Endpoint{ep}, VolunteerClass: "eim", PunchToken: "aa", CertFingerprint: "bb", TTLMillis: 6000})
	enc("response_zero", PunchResponse{})
	enc("directive_full", PunchDirective{SessionID: "sess", RelayID: "relay-1", ClientReflexive: []Endpoint{ep}, ClientLocal: []Endpoint{ep}, ClientClass: "eim", PunchToken: "aa", ReflectorAddrs: []string{"203.0.113.5:9000"}, TTLMillis: 6000, QUICALPN: "openrung-punch/1", ProtoVersion: 1})
	enc("directive_zero", PunchDirective{})
	enc("ack_full", PunchAck{SessionID: "sess", OK: true, VolunteerReflexive: []Endpoint{ep}, VolunteerLocal: []Endpoint{ep}, VolunteerClass: "eim", CertFingerprint: "bb", Error: "e"})
	enc("ack_zero", PunchAck{})
	enc("result_full", PunchResult{SessionID: "sess", OK: true, Reason: "punch", RTTMillis: 123, NATClass: "eim"})
	enc("result_zero", PunchResult{})
	checkStringMap(t, "json", jsonOut, want.JSON)

	// --- token derivation + encoding ---
	if got := hex.EncodeToString(ComputeToken([]byte("hub-secret-0123"), "sess-abc", "relay-xyz", hex.EncodeToString(nonce))); got != want.ComputeToken {
		t.Errorf("compute_token = %q, want %q", got, want.ComputeToken)
	}
	if got := EncodeToken(token); got != want.EncodeToken {
		t.Errorf("encode_token = %q, want %q", got, want.EncodeToken)
	}
	dec, err := DecodeToken(EncodeToken(token))
	if err != nil {
		t.Fatalf("decode roundtrip: %v", err)
	}
	if got := hex.EncodeToString(dec); got != want.DecodeRoundtrip {
		t.Errorf("decode_roundtrip = %q, want %q", got, want.DecodeRoundtrip)
	}
	_, badHexErr := DecodeToken("zz")
	_, shortErr := DecodeToken("aabb")
	if badHexErr == nil || shortErr == nil {
		t.Fatalf("decode errors = (%v, %v), want both non-nil", badHexErr, shortErr)
	}
	checkStringMap(t, "decode_errors", map[string]string{
		"badhex": badHexErr.Error(),
		"short":  shortErr.Error(),
	}, want.DecodeErrors)

	// --- nonce key (hub correlation) ---
	nk, err := NonceKey(hex.EncodeToString(nonce))
	if err != nil {
		t.Fatalf("nonce key: %v", err)
	}
	if got := hex.EncodeToString([]byte(nk)); got != want.NonceKey {
		t.Errorf("nonce_key = %q, want %q", got, want.NonceKey)
	}

	// --- SanitizePeers under BOTH presets (policy-bearing) ---
	if got := DesktopPolicy().SanitizePeers(goldenSanitizeInput()); !reflect.DeepEqual(got, want.SanitizeDesktop) {
		t.Errorf("DesktopPolicy().SanitizePeers = %+v,\nwant %+v", got, want.SanitizeDesktop)
	}
	if got := MobilePolicy().SanitizePeers(goldenSanitizeInput()); !reflect.DeepEqual(got, want.SanitizeMobile) {
		t.Errorf("MobilePolicy().SanitizePeers = %+v,\nwant %+v", got, want.SanitizeMobile)
	}

	// --- Endpoint.UDPAddr error strings ---
	udpErrs := map[string]string{}
	if _, err := (Endpoint{IP: "bad", Port: 1}).UDPAddr(); err != nil {
		udpErrs["badip"] = err.Error()
	}
	if _, err := (Endpoint{IP: "1.2.3.4", Port: 0}).UDPAddr(); err != nil {
		udpErrs["badport"] = err.Error()
	}
	if _, err := (Endpoint{IP: "1.2.3.4", Port: 70000}).UDPAddr(); err != nil {
		udpErrs["hugeport"] = err.Error()
	}
	checkStringMap(t, "udpaddr_errors", udpErrs, want.UDPAddrErrors)
}
