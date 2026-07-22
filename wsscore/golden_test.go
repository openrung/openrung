package wsscore

import (
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
)

type goldenProtocol struct {
	Version                   int    `json:"version"`
	BridgePath                string `json:"bridge_path"`
	Subprotocol               string `json:"subprotocol"`
	TicketAuthorizationHeader string `json:"ticket_authorization_header"`
	TicketBearerPrefix        string `json:"ticket_bearer_prefix"`
	MaxFronts                 int    `json:"max_fronts"`
	MaxFrontIDBytes           int    `json:"max_front_id_bytes"`
	MaxFrontURLBytes          int    `json:"max_front_url_bytes"`
	MaxTicketBytes            int    `json:"max_ticket_bytes"`
}

type goldenLimits struct {
	DefaultHandshakeTimeout     string `json:"default_handshake_timeout"`
	MaxHandshakeTimeout         string `json:"max_handshake_timeout"`
	DefaultPingInterval         string `json:"default_ping_interval"`
	DefaultPingWriteTimeout     string `json:"default_ping_write_timeout"`
	MaxPingInterval             string `json:"max_ping_interval"`
	MaxPingWriteTimeout         string `json:"max_ping_write_timeout"`
	DefaultWebSocketReadMax     int64  `json:"default_websocket_read_max"`
	MaxWebSocketReadMax         int64  `json:"max_websocket_read_max"`
	DefaultMaxConcurrentStreams int    `json:"default_max_concurrent_streams"`
	MaxConcurrentStreams        int    `json:"max_concurrent_streams"`
	DefaultStreamIdleTimeout    string `json:"default_stream_idle_timeout"`
	DefaultNoStreamIdleTimeout  string `json:"default_no_stream_idle_timeout"`
	DefaultSessionLifetime      string `json:"default_session_lifetime"`
	MaxSessionLifetime          string `json:"max_session_lifetime"`
}

type goldenFrontJSON struct {
	Full          string `json:"full"`
	Zero          string `json:"zero"`
	NormalizedSet string `json:"normalized_set"`
}

type goldenYamux struct {
	AcceptBacklog          int    `json:"accept_backlog"`
	EnableKeepAlive        bool   `json:"enable_keepalive"`
	KeepAliveInterval      string `json:"keepalive_interval"`
	ConnectionWriteTimeout string `json:"connection_write_timeout"`
	MaxStreamWindowSize    uint32 `json:"max_stream_window_size"`
	StreamOpenTimeout      string `json:"stream_open_timeout"`
	StreamCloseTimeout     string `json:"stream_close_timeout"`
	DiscardLogs            bool   `json:"discard_logs"`
}

type goldenLifecycle struct {
	MaxConcurrentStreams int    `json:"max_concurrent_streams"`
	StreamIdleTimeout    string `json:"stream_idle_timeout"`
	NoStreamIdleTimeout  string `json:"no_stream_idle_timeout"`
	SessionLifetime      string `json:"session_lifetime"`
}

type goldenVectors struct {
	Protocol              goldenProtocol    `json:"protocol"`
	Limits                goldenLimits      `json:"limits"`
	FrontJSON             goldenFrontJSON   `json:"front_json"`
	FrontURLNormalization map[string]string `json:"front_url_normalization"`
	FrontIDNormalization  map[string]string `json:"front_id_normalization"`
	InvalidFrontURLs      []string          `json:"invalid_front_urls"`
	InvalidFrontIDs       []string          `json:"invalid_front_ids"`
	NormalizedFronts      []Front           `json:"normalized_fronts"`
	Yamux                 goldenYamux       `json:"yamux"`
	LifecycleDefaults     goldenLifecycle   `json:"lifecycle_defaults"`
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

	gotProtocol := goldenProtocol{
		Version: ProtocolVersion, BridgePath: BridgePath, Subprotocol: Subprotocol,
		TicketAuthorizationHeader: TicketAuthorizationHeader, TicketBearerPrefix: TicketBearerPrefix,
		MaxFronts: MaxFronts, MaxFrontIDBytes: MaxFrontIDBytes,
		MaxFrontURLBytes: MaxFrontURLBytes, MaxTicketBytes: MaxTicketBytes,
	}
	if gotProtocol != want.Protocol {
		t.Errorf("protocol constants = %+v, want %+v", gotProtocol, want.Protocol)
	}
	gotLimits := goldenLimits{
		DefaultHandshakeTimeout:     DefaultHandshakeTimeout.String(),
		MaxHandshakeTimeout:         MaxHandshakeTimeout.String(),
		DefaultPingInterval:         DefaultPingInterval.String(),
		DefaultPingWriteTimeout:     DefaultPingWriteTimeout.String(),
		MaxPingInterval:             MaxPingInterval.String(),
		MaxPingWriteTimeout:         MaxPingWriteTimeout.String(),
		DefaultWebSocketReadMax:     DefaultWebSocketReadMax,
		MaxWebSocketReadMax:         MaxWebSocketReadMax,
		DefaultMaxConcurrentStreams: DefaultMaxConcurrentStreams,
		MaxConcurrentStreams:        MaxConcurrentStreams,
		DefaultStreamIdleTimeout:    DefaultStreamIdleTimeout.String(),
		DefaultNoStreamIdleTimeout:  DefaultNoStreamIdleTimeout.String(),
		DefaultSessionLifetime:      DefaultSessionLifetime.String(),
		MaxSessionLifetime:          MaxSessionLifetime.String(),
	}
	if gotLimits != want.Limits {
		t.Errorf("bounded defaults and maxima = %+v, want %+v", gotLimits, want.Limits)
	}
	for input, expected := range want.FrontURLNormalization {
		got, err := NormalizeFrontURL(input)
		if err != nil || got != expected {
			t.Errorf("NormalizeFrontURL(%q) = %q, %v; want %q", input, got, err, expected)
		}
	}
	for input, expected := range want.FrontIDNormalization {
		got, err := NormalizeFrontID(input)
		if err != nil || got != expected {
			t.Errorf("NormalizeFrontID(%q) = %q, %v; want %q", input, got, err, expected)
		}
	}
	for _, input := range want.InvalidFrontURLs {
		if _, err := NormalizeFrontURL(input); err == nil {
			t.Errorf("NormalizeFrontURL(%q) unexpectedly succeeded", input)
		}
	}
	for _, input := range want.InvalidFrontIDs {
		if err := ValidateFrontID(input); err == nil {
			t.Errorf("ValidateFrontID(%q) unexpectedly succeeded", input)
		}
	}

	inputFronts := []Front{
		{ID: " Tehran-B ", URL: " WSS://D222222ABCDEF8.CLOUDFRONT.NET/api/v1/wss-bridge ", ProtocolVersion: ProtocolVersion},
		{ID: "tehran-a", URL: "wss://d111111abcdef8.cloudfront.net/api/v1/wss-bridge", ProtocolVersion: ProtocolVersion},
	}
	gotFronts, err := NormalizeFronts(inputFronts)
	if err != nil {
		t.Fatalf("NormalizeFronts: %v", err)
	}
	if !reflect.DeepEqual(gotFronts, want.NormalizedFronts) {
		t.Errorf("normalized fronts = %+v, want %+v", gotFronts, want.NormalizedFronts)
	}
	marshal := func(value any) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal golden wire value: %v", err)
		}
		return string(encoded)
	}
	gotFrontJSON := goldenFrontJSON{
		Full: marshal(Front{
			ID: "front-a", URL: "wss://cdn.example" + BridgePath,
			ProtocolVersion: ProtocolVersion,
		}),
		Zero:          marshal(Front{}),
		NormalizedSet: marshal(gotFronts),
	}
	if gotFrontJSON != want.FrontJSON {
		t.Errorf("front JSON = %+v, want %+v", gotFrontJSON, want.FrontJSON)
	}

	cfg := yamuxConfig()
	gotYamux := goldenYamux{
		AcceptBacklog:          cfg.AcceptBacklog,
		EnableKeepAlive:        cfg.EnableKeepAlive,
		KeepAliveInterval:      cfg.KeepAliveInterval.String(),
		ConnectionWriteTimeout: cfg.ConnectionWriteTimeout.String(),
		MaxStreamWindowSize:    cfg.MaxStreamWindowSize,
		StreamOpenTimeout:      cfg.StreamOpenTimeout.String(),
		StreamCloseTimeout:     cfg.StreamCloseTimeout.String(),
		DiscardLogs:            cfg.LogOutput == io.Discard && cfg.Logger == nil,
	}
	if gotYamux != want.Yamux {
		t.Errorf("yamux profile = %+v, want %+v", gotYamux, want.Yamux)
	}
	lifecycle, err := NormalizeLifecycleOptions(LifecycleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotLifecycle := goldenLifecycle{
		MaxConcurrentStreams: lifecycle.MaxConcurrentStreams,
		StreamIdleTimeout:    lifecycle.StreamIdleTimeout.String(),
		NoStreamIdleTimeout:  lifecycle.NoStreamIdleTimeout.String(),
		SessionLifetime:      lifecycle.SessionLifetime.String(),
	}
	if gotLifecycle != want.LifecycleDefaults {
		t.Errorf("lifecycle defaults = %+v, want %+v", gotLifecycle, want.LifecycleDefaults)
	}
}

func TestCanonicalValidationRejectsNormalizableValues(t *testing.T) {
	for _, value := range []string{
		" WSS://CDN.EXAMPLE/api/v1/wss-bridge ",
		"wss://CDN.EXAMPLE/api/v1/wss-bridge",
	} {
		if err := ValidateFrontURL(value); err == nil {
			t.Fatalf("ValidateFrontURL(%q) accepted non-canonical URL", value)
		}
	}
	if err := ValidateFrontURL("wss://cdn.example/api/v1/wss-bridge"); err != nil {
		t.Fatalf("canonical front rejected: %v", err)
	}
}

func TestNormalizeFrontsRejectsDuplicatesAndWrongVersion(t *testing.T) {
	valid := Front{ID: "front-a", URL: "wss://a.cdn.example/api/v1/wss-bridge", ProtocolVersion: ProtocolVersion}
	for name, fronts := range map[string][]Front{
		"duplicate ID":  {valid, {ID: valid.ID, URL: "wss://b.cdn.example/api/v1/wss-bridge", ProtocolVersion: ProtocolVersion}},
		"duplicate URL": {valid, {ID: "front-b", URL: valid.URL, ProtocolVersion: ProtocolVersion}},
		"wrong version": {{ID: valid.ID, URL: valid.URL, ProtocolVersion: ProtocolVersion + 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeFronts(fronts); err == nil {
				t.Fatal("unsafe front set accepted")
			}
		})
	}
}
