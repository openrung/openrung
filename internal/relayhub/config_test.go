package relayhub

import (
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		ControlAddr:       ":9443",
		PublicHost:        "hub.example.com",
		PortRangeStart:    20000,
		PortRangeEnd:      20100,
		BrokerURL:         "http://broker.test",
		HeartbeatInterval: 30 * time.Second,
	}
}

func TestConfigValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected valid config: %v", err)
	}

	cases := map[string]func(*Config){
		"missing public host":  func(c *Config) { c.PublicHost = "" },
		"missing broker":       func(c *Config) { c.BrokerURL = "" },
		"inverted range":       func(c *Config) { c.PortRangeStart, c.PortRangeEnd = 30000, 20000 },
		"range out of bounds":  func(c *Config) { c.PortRangeEnd = 70000 },
		"heartbeat too small":  func(c *Config) { c.HeartbeatInterval = time.Second },
		"tls cert without key": func(c *Config) { c.TLSCertPath = "cert.pem" },
		"tls key without cert": func(c *Config) { c.TLSKeyPath = "key.pem" },
		"reflector without http": func(c *Config) {
			c.ReflectorAddrs = []string{":19302"}
		},
		"advertise without bind": func(c *Config) {
			c.HTTPAddr = ":9444"
			c.ReflectorAdvertise = []string{"203.0.113.1:19302"}
		},
		"advertise length mismatch": func(c *Config) {
			c.HTTPAddr = ":9444"
			c.ReflectorAddrs = []string{":19302"}
			c.ReflectorAdvertise = []string{"203.0.113.1:19302", "203.0.113.2:19302"}
		},
	}
	for name, mutate := range cases {
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}

	// A matched bind/advertise pair with the HTTP API enabled is valid.
	ok := validConfig()
	ok.HTTPAddr = ":9444"
	ok.ReflectorAddrs = []string{"172.26.4.229:19302"}
	ok.ReflectorAdvertise = []string{"43.202.39.181:19302"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("matched bind/advertise pair should validate: %v", err)
	}
	if !ok.PunchEnabled() {
		t.Fatal("punch should be enabled with http-addr + reflector")
	}
}

func TestConfigApplyDefaults(t *testing.T) {
	var cfg Config
	cfg.ApplyDefaults()
	if cfg.ControlAddr != ":9443" {
		t.Fatalf("ControlAddr = %q, want :9443", cfg.ControlAddr)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Fatalf("HeartbeatInterval = %s, want 30s", cfg.HeartbeatInterval)
	}
}

func TestConfigTLSEnabled(t *testing.T) {
	cfg := validConfig()
	if cfg.TLSEnabled() {
		t.Fatal("expected TLS disabled with no cert/key")
	}
	cfg.TLSCertPath = "cert.pem"
	cfg.TLSKeyPath = "key.pem"
	if !cfg.TLSEnabled() {
		t.Fatal("expected TLS enabled with cert and key")
	}
}

func TestParsePortRange(t *testing.T) {
	start, end, err := ParsePortRange("20000-20100")
	if err != nil {
		t.Fatalf("ParsePortRange: %v", err)
	}
	if start != 20000 || end != 20100 {
		t.Fatalf("ParsePortRange = %d-%d, want 20000-20100", start, end)
	}

	for _, bad := range []string{"", "20000", "abc-def", "20000-", "-20100"} {
		if _, _, err := ParsePortRange(bad); err == nil {
			t.Fatalf("ParsePortRange(%q) expected error", bad)
		}
	}
}
