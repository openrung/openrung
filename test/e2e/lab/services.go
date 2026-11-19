// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Fixed lab credentials. The lab has no network, so these protect nothing;
// they exist so the stack runs with authentication switched on.
const (
	VolunteerToken = "lab-volunteer-token"

	// BrokerURL is how lab services reach the broker.
	BrokerURL = "http://" + BrokerAddr + ":8080"
	// ClientBrokerURL is the broker as the client is told to reach it. The
	// stock client accepts a plain-HTTP, lab-signed broker only on loopback,
	// so the user's node redirects this loopback port to BrokerAddr.
	ClientBrokerURL = "http://127.0.0.1:8080"

	// HubControlAddr is where volunteer relays dial the relay hub.
	HubControlAddr = HubAddr + ":9443"

	// Short leases and heartbeats so a scenario sees a relay disappear from
	// the broker in seconds rather than minutes.
	leaseTTL          = "20s"
	heartbeatInterval = "5s"
)

// seed derives a stable 32-byte secret for a named purpose, base64-encoded
// the way the broker and relays expect.
func seed(purpose string) string {
	sum := sha256.Sum256([]byte("openrung lab " + purpose))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// InternetService is the web side of the internet stand-in.
func InternetService() Service {
	return Service{Name: "internet", Node: "inet", Argv: []string{"lab", "internet", CAPath}}
}

// DNSService resolves every hostname to the internet stand-in. Queries are
// logged, so a scenario can check which names a component looked up.
func DNSService() Service {
	return Service{Name: "dns", Node: "inet", Argv: []string{
		"dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--user=root", "--no-resolv", "--no-hosts",
		// Every name has the stand-in's IPv4 address and no IPv6 address.
		"--address=/#/" + InetAddr, "--local=/#/", "--log-queries", "--log-facility=-",
	}}
}

// BrokerService is the discovery broker with authentication on, internet
// lookups off, and limits loose enough for a test loop from one address.
func BrokerService() Service {
	env := []string{
		"OPENRUNG_RELAY_SIGNING_KEY=" + seed("broker relay signing key"),
		"OPENRUNG_VOLUNTEER_TOKEN=" + VolunteerToken,
		"OPENRUNG_FOUNDATION_TOKEN=" + FoundationToken,
		"OPENRUNG_WSS_TICKET_SIGNING_SEED=" + wssTicketSeed,
		"OPENRUNG_GEOIP_ENDPOINT=off",
		"OPENRUNG_MAX_NEW_RELAY_IDS_PER_IP_PER_DAY=off",
		"OPENRUNG_TELEMETRY_FILE=" + TelemetryPath,
	}
	for _, limit := range []string{"RELAY_LIST", "WSS_TICKET", "TELEMETRY", "USER_AUTH", "ACCESS", "RELAY_REGISTRATION", "SPEED_TEST"} {
		env = append(env, "OPENRUNG_RATE_LIMIT_"+limit+"=1000:1000")
	}
	return Service{Name: "broker", Node: "broker", Env: env,
		Argv: []string{"broker", "-addr", ":8080", "-lease-ttl", leaseTTL}}
}

// TelemetryPath is the broker's telemetry log.
const TelemetryPath = RunDir + "/state/broker/telemetry.jsonl"

// HubService is the relay hub that publishes volunteers behind NAT.
func HubService() Service {
	return Service{Name: "hub", Node: "hub", Env: []string{
		"OPENRUNG_HUB_PUBLIC_HOST=" + HubAddr,
		"OPENRUNG_BROKER_URL=" + BrokerURL,
		"OPENRUNG_VOLUNTEER_TOKEN=" + VolunteerToken,
		"OPENRUNG_HUB_HTTP_ADDR=:9444",
		"OPENRUNG_HUB_HEARTBEAT_INTERVAL=" + heartbeatInterval,
	}, Argv: []string{"relayhub"}}
}

// DirectRelayService is a relay clients dial directly at publicAddr, started
// through the relay image's entrypoint. name doubles as its label.
func DirectRelayService(name, node, publicAddr string) Service {
	return Service{Name: name, Node: node, Argv: []string{"entrypoint.sh"}, Env: append(relayEnv(name),
		"OPENRUNG_MODE=direct",
		"OPENRUNG_BROKER_URL="+BrokerURL,
		"OPENRUNG_PUBLIC_HOST="+publicAddr,
		"OPENRUNG_LISTEN_HOST=0.0.0.0",
		"OPENRUNG_HEARTBEAT_INTERVAL="+heartbeatInterval,
	)}
}

// TunnelRelayService is a volunteer relay behind NAT that publishes itself
// through the relay hub. name doubles as its label and node.
func TunnelRelayService(name string) Service {
	return Service{Name: name, Node: name, Argv: []string{"entrypoint.sh"}, Env: append(relayEnv(name),
		"OPENRUNG_MODE=tunnel",
		"OPENRUNG_HUB_ADDR="+HubControlAddr,
		"OPENRUNG_HUB_TLS=false",
	)}
}

func relayEnv(name string) []string {
	return []string{
		"OPENRUNG_VOLUNTEER_TOKEN=" + VolunteerToken,
		"OPENRUNG_LABEL=" + name,
		// A fixed identity keeps the relay's ID stable across restarts.
		"OPENRUNG_IDENTITY_SEED=" + seed("relay identity "+name),
		"OPENRUNG_CONFIG_OUT=" + StateDir(name) + "/xray.json",
		"OPENRUNG_XRAY_PATH=/usr/local/bin/xray",
		"XRAY_LOCATION_ASSET=/usr/local/bin",
	}
}

// DefaultRelays are the relay labels a fresh lab lists at the broker: two
// with public addresses and one volunteer behind a home router.
var DefaultRelays = []string{"relay-a", "relay-b", "vol"}

// DefaultServices returns the services of a fresh lab in start order.
func DefaultServices() []Service {
	return []Service{
		InternetService(),
		DNSService(),
		BrokerService(),
		HubService(),
		DirectRelayService("relay-a", "relay-a", RelayAAddr),
		DirectRelayService("relay-b", "relay-b", RelayBAddr),
		TunnelRelayService("vol"),
	}
}

// Vol2Service is a second volunteer, behind a home router that itself sits
// behind a carrier-grade NAT.
func Vol2Service() Service { return TunnelRelayService("vol2") }

// VolDirectService is the first volunteer's machine serving clients directly
// on its home router's public address, which needs PortForward("gw-vol", 443,
// VolAddr+":443").
func VolDirectService() Service { return DirectRelayService("vol-direct", "vol", GwVolAddr) }

// OptionalServices are defined but not started by Up.
func OptionalServices() []Service {
	return []Service{Vol2Service(), VolDirectService(), FoundationRelayService(), WSSSidecarService()}
}

// ExitAddr is the address the internet sees for traffic leaving through the
// relay with the given label.
func ExitAddr(label string) string {
	return map[string]string{
		"relay-a": RelayAAddr, "relay-b": RelayBAddr, FoundationRelay: RelayFAddr,
		"vol": GwVolAddr, "vol-direct": GwVolAddr, "vol2": CGNAddr,
	}[label]
}

// ServiceByName returns the definition of a default or optional service.
func ServiceByName(name string) (Service, bool) {
	for _, s := range append(DefaultServices(), OptionalServices()...) {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// Relay is the part of a broker relay descriptor the lab reads.
type Relay struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	PublicHost string `json:"public_host"`
	PublicPort int    `json:"public_port"`
	Transport  string `json:"transport"`
}

// ListedRelays asks the broker which relays it currently hands out.
func ListedRelays() ([]Relay, error) {
	body, err := OutputIn("broker", "curl", "-fsS", "--max-time", "5", "http://127.0.0.1:8080/api/v1/relays?limit=20")
	if err != nil {
		return nil, err
	}
	var list struct {
		Relays []Relay `json:"relays"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		return nil, fmt.Errorf("decode relay list: %w: %s", err, body)
	}
	return list.Relays, nil
}

// WaitListed waits until the broker lists exactly the relays with the given
// labels.
func WaitListed(timeout time.Duration, labels ...string) error {
	return Eventually(timeout, func() error {
		relays, err := ListedRelays()
		if err != nil {
			return err
		}
		listed := make(map[string]bool)
		for _, relay := range relays {
			listed[relay.Label] = true
		}
		var got []string
		for label := range listed {
			got = append(got, label)
		}
		if len(listed) != len(labels) {
			return fmt.Errorf("broker lists relays %v, want %v", got, labels)
		}
		for _, label := range labels {
			if !listed[label] {
				return fmt.Errorf("broker lists relays %v, want %v", got, labels)
			}
		}
		return nil
	})
}

// Up builds the lab from scratch: the network, the internet stand-in and the
// default services, and waits until the broker lists the default relays.
func Up() error {
	Down()
	for _, dir := range []string{RunDir, LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// One resolver for every node: the internet stand-in.
	if err := writeFile("/etc/resolv.conf", "nameserver "+InetAddr+"\n"); err != nil {
		return err
	}
	if err := buildTopology(); err != nil {
		return fmt.Errorf("build topology: %w", err)
	}
	for _, s := range DefaultServices() {
		if err := Start(s); err != nil {
			return err
		}
		if s.Name == "broker" {
			if err := waitBroker(); err != nil {
				return err
			}
		}
	}
	if err := Eventually(10*time.Second, func() error {
		_, err := os.Stat(CAPath)
		return err
	}); err != nil {
		return fmt.Errorf("internet stand-in did not write its CA: %w", err)
	}
	if err := WaitListed(60*time.Second, DefaultRelays...); err != nil {
		return fmt.Errorf("relays did not register: %w", err)
	}
	return nil
}

func waitBroker() error {
	err := Eventually(20*time.Second, func() error {
		return In("broker", "curl", "-fsS", "--max-time", "2", "-o", "/dev/null", "http://127.0.0.1:8080/healthz")
	})
	if err != nil {
		return fmt.Errorf("broker did not become healthy: %w", err)
	}
	return nil
}

// Restore returns a running lab to its fresh state: no impairments, optional
// services stopped, default services running and their relays listed.
func Restore() error {
	if err := ClearNetwork(); err != nil {
		return err
	}
	for _, s := range OptionalServices() {
		if err := Stop(s.Name); err != nil {
			return err
		}
	}
	for _, s := range DefaultServices() {
		if Running(s.Name) {
			continue
		}
		if err := Start(s); err != nil {
			return err
		}
		if s.Name == "broker" {
			if err := waitBroker(); err != nil {
				return err
			}
		}
	}
	return WaitListed(90*time.Second, DefaultRelays...)
}

// Down stops every service and removes the network. Logs are kept.
func Down() {
	StopAll()
	destroyTopology()
	_ = os.RemoveAll(RunDir)
}

// Status describes the lab for a human: nodes, services and impairments.
func Status() (string, error) {
	var b strings.Builder
	b.WriteString("services: " + strings.Join(RunningServices(), ", ") + "\n")
	if relays, err := ListedRelays(); err == nil {
		b.WriteString("broker lists:\n")
		for _, relay := range relays {
			fmt.Fprintf(&b, "  %-8s %s:%d %s %s\n", relay.Label, relay.PublicHost, relay.PublicPort, relay.Transport, relay.ID)
		}
	} else {
		fmt.Fprintf(&b, "broker: %v\n", err)
	}
	network, err := DescribeNetwork()
	if err != nil {
		return "", err
	}
	b.WriteString("network:\n" + network)
	return b.String(), nil
}
