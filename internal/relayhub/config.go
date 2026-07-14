// Package relayhub holds the configuration for the relay hub binary, the
// publicly reachable component that terminates reverse tunnels from CGNAT
// volunteer-run relays and forwards client traffic to them.
package relayhub

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds the relay hub's runtime configuration.
type Config struct {
	// ControlAddr is the address volunteer-run relays dial to establish a tunnel.
	ControlAddr string
	// PublicHost is the hostname/IP advertised to clients for tunneled relays.
	PublicHost string
	// PublicBindHost is the interface per-tunnel public listeners bind to.
	// Empty means all interfaces.
	PublicBindHost string
	// PortRangeStart and PortRangeEnd bound the public TCP ports (inclusive).
	PortRangeStart int
	PortRangeEnd   int
	// BrokerURL is the broker base URL the hub registers relays against.
	BrokerURL string
	// Token is the shared bearer token used both to authenticate volunteer-class
	// relays and to authorize the hub's broker calls.
	Token string
	// TLSCertPath and TLSKeyPath enable TLS on the control channel when both set.
	TLSCertPath string
	TLSKeyPath  string
	// HeartbeatInterval is how often the hub re-heartbeats each live relay.
	HeartbeatInterval time.Duration
	// HTTPAddr is the address the hub's HTTP API listens on (e.g. ":9444"). It
	// serves the reachability prober and, when reflectors are configured, the NAT
	// punch coordinator. Empty disables the HTTP API (no probe, no punch).
	HTTPAddr string
	// ReflectorAddrs are the UDP reflector BIND addresses (host:port) — addresses
	// that exist on the host's NIC. On a host whose public IP is 1:1-NAT'd (AWS
	// EC2/Lightsail), bind the on-NIC private IP(s) or a wildcard, and set
	// ReflectorAdvertise to the public IP(s). Two distinct vantage points enable
	// correct RFC 5780 NAT classification; a single one degrades to "unknown"
	// (attempt-then-fallback). Empty disables punch.
	ReflectorAddrs []string
	// ReflectorAdvertise are the public reflector addresses announced to peers,
	// positionally matched to ReflectorAddrs. Empty means advertise the bound
	// addresses (correct only when the host owns its public IP directly).
	ReflectorAdvertise []string
	// PunchTTL is the punch time budget handed to both peers.
	PunchTTL time.Duration
}

// ApplyDefaults fills in zero-valued fields with sensible defaults.
func (c *Config) ApplyDefaults() {
	if c.ControlAddr == "" {
		c.ControlAddr = ":9443"
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.PunchEnabled() && c.PunchTTL == 0 {
		c.PunchTTL = 6 * time.Second
	}
}

// HTTPEnabled reports whether the hub should start its HTTP API (prober + punch).
func (c Config) HTTPEnabled() bool {
	return c.HTTPAddr != ""
}

// PunchEnabled reports whether the hub should run the punch coordinator. Punch
// needs both the HTTP API and at least one reflector; the reachability prober
// needs only the HTTP API.
func (c Config) PunchEnabled() bool {
	return c.HTTPAddr != "" && len(c.ReflectorAddrs) > 0
}

// Validate reports configuration errors.
func (c Config) Validate() error {
	if c.PublicHost == "" {
		return fmt.Errorf("public-host is required")
	}
	if c.BrokerURL == "" {
		return fmt.Errorf("broker URL is required")
	}
	if c.PortRangeStart < 1 || c.PortRangeEnd > 65535 || c.PortRangeStart > c.PortRangeEnd {
		return fmt.Errorf("invalid port range %d-%d", c.PortRangeStart, c.PortRangeEnd)
	}
	if c.HeartbeatInterval < 5*time.Second {
		return fmt.Errorf("heartbeat-interval must be at least 5s")
	}
	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return fmt.Errorf("tls-cert and tls-key must be set together")
	}
	if len(c.ReflectorAddrs) > 0 && c.HTTPAddr == "" {
		return fmt.Errorf("reflector-addrs requires http-addr (the punch coordinator is served on the hub HTTP API)")
	}
	if len(c.ReflectorAdvertise) > 0 {
		if len(c.ReflectorAddrs) == 0 {
			return fmt.Errorf("reflector-advertise requires reflector-addrs (the bind addresses)")
		}
		if len(c.ReflectorAdvertise) != len(c.ReflectorAddrs) {
			return fmt.Errorf("reflector-advertise must have the same number of addresses as reflector-addrs (%d vs %d)", len(c.ReflectorAdvertise), len(c.ReflectorAddrs))
		}
	}
	return nil
}

// ParseReflectorAddrs splits a comma-separated list of host:port reflector
// addresses, trimming whitespace and dropping empties.
func ParseReflectorAddrs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// TLSEnabled reports whether the control channel should use TLS.
func (c Config) TLSEnabled() bool {
	return c.TLSCertPath != "" && c.TLSKeyPath != ""
}

// ParsePortRange parses a "start-end" port range string.
func ParsePortRange(s string) (start, end int, err error) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port range must be in the form start-end")
	}
	start, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range start: %w", err)
	}
	end, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range end: %w", err)
	}
	return start, end, nil
}
