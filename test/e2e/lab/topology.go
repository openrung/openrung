// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The lab's address plan. Public addresses come from the RFC 5737
// documentation ranges, so nothing here can collide with a real network even
// though the lab container itself has no network at all.
const (
	// InetAddr is the stand-in for "the rest of the internet": DNS, a web
	// server that answers for every hostname, and the catch-all for every
	// address the lab does not assign.
	InetAddr   = "192.0.2.1"
	BrokerAddr = "198.51.100.10"
	HubAddr    = "198.51.100.30"
	// HubAltAddr is the relay hub's second public address. Its NAT-type
	// reflector listens on both, so a peer's mapping can be compared across
	// two destinations.
	HubAltAddr = "198.51.100.31"
	RelayAAddr = "198.51.100.41"
	RelayBAddr = "198.51.100.42"
	RelayFAddr = "198.51.100.43" // a Foundation relay with a WSS front

	// Public (WAN) addresses of the access routers.
	GwUserAddr   = "203.0.113.10" // the user's home router
	GwVolAddr    = "203.0.113.20" // the volunteer's home router
	CGNAddr      = "203.0.113.30" // a carrier-grade NAT in front of vol2
	GwMobileAddr = "203.0.113.40" // the user's second (mobile) uplink
	GwHostAddr   = "203.0.113.50" // the router in front of the container's own namespace

	// Private addresses behind those routers.
	UserAddr       = "192.168.10.10"
	UserMobileAddr = "10.20.0.10"
	VolAddr        = "192.168.20.10"
	Vol2Addr       = "192.168.30.10"

	// HostDev and HostAddr are the container's own network namespace's link
	// into the lab, behind gw-host (see attachHost). When the lab runs on the
	// host's network (make e2e-lab-host) that namespace is this machine's.
	HostDev  = "or-lab"
	HostAddr = "192.168.77.2"

	// coreAddr is the backbone router's address on every point-to-point link.
	coreAddr = "192.0.2.254"
)

// hostRouterCIDR is gw-host's side of the link to the container's namespace.
const hostRouterCIDR = "192.168.77.1/30"

// labPrefixes are the public ranges the container's namespace reaches through
// gw-host: the servers' and the routers' documentation ranges, so a route to
// them never shadows a real network. The internet stand-in's 192.0.2.0/24 is
// left out, since tests on the machine count on it going nowhere.
var labPrefixes = []string{"198.51.100.0/24", "203.0.113.0/24"}

// publicHost is a namespace attached straight to the backbone.
type publicHost struct {
	name, addr string
	// extra are further /32 addresses on the same link.
	extra  []string
	router bool
}

// privateLink joins a router's LAN side to the one host behind it.
type privateLink struct {
	name       string // link name used by the shaping API
	router     string
	routerDev  string
	routerCIDR string
	host       string
	hostDev    string
	hostCIDR   string
	// defaultRoute is false for a host's secondary uplink.
	defaultRoute bool
}

var publicHosts = []publicHost{
	{name: "inet", addr: InetAddr},
	{name: "broker", addr: BrokerAddr},
	{name: "hub", addr: HubAddr, extra: []string{HubAltAddr}},
	{name: "relay-a", addr: RelayAAddr},
	{name: "relay-b", addr: RelayBAddr},
	{name: FoundationRelay, addr: RelayFAddr},
	{name: "gw-user", addr: GwUserAddr, router: true},
	{name: "gw-mobile", addr: GwMobileAddr, router: true},
	{name: "gw-vol", addr: GwVolAddr, router: true},
	{name: "cgn", addr: CGNAddr, router: true},
	{name: "gw-host", addr: GwHostAddr, router: true},
}

var privateLinks = []privateLink{
	{name: "user", router: "gw-user", routerDev: "lan0", routerCIDR: "192.168.10.1/24", host: "user", hostDev: "eth0", hostCIDR: UserAddr + "/24", defaultRoute: true},
	{name: "user-mobile", router: "gw-mobile", routerDev: "lan0", routerCIDR: "10.20.0.1/24", host: "user", hostDev: "eth1", hostCIDR: UserMobileAddr + "/24"},
	{name: "vol", router: "gw-vol", routerDev: "lan0", routerCIDR: "192.168.20.1/24", host: "vol", hostDev: "eth0", hostCIDR: VolAddr + "/24", defaultRoute: true},
	{name: "gw-vol2", router: "cgn", routerDev: "lan0", routerCIDR: "100.64.0.1/24", host: "gw-vol2", hostDev: "eth0", hostCIDR: "100.64.0.10/24", defaultRoute: true},
	{name: "vol2", router: "gw-vol2", routerDev: "lan0", routerCIDR: "192.168.30.1/24", host: "vol2", hostDev: "eth0", hostCIDR: Vol2Addr + "/24", defaultRoute: true},
}

// natRouters are the nodes that translate their LAN behind their uplink.
var natRouters = []string{"gw-user", "gw-mobile", "gw-vol", "cgn", "gw-vol2", "gw-host"}

// End is one side of a link: an interface inside a node.
type End struct{ Node, Dev string }

// Nodes returns every namespace in the lab, sorted.
func Nodes() []string {
	seen := map[string]bool{"core": true}
	for _, h := range publicHosts {
		seen[h.name] = true
	}
	for _, l := range privateLinks {
		seen[l.router], seen[l.host] = true, true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Links maps a link name to its two ends. A public host's link to the
// backbone is named after the host; a private link is named after the host
// behind the router (with "user-mobile" for the user's second uplink).
func Links() map[string][2]End {
	links := make(map[string][2]End)
	for _, h := range publicHosts {
		links[h.name] = [2]End{{h.name, "eth0"}, {"core", coreDev(h.name)}}
	}
	for _, l := range privateLinks {
		links[l.name] = [2]End{{l.host, l.hostDev}, {l.router, l.routerDev}}
	}
	return links
}

// LinkNames returns the link names, sorted.
func LinkNames() []string {
	links := Links()
	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func coreDev(host string) string { return "c-" + host }

// buildTopology creates every namespace, link, route and NAT rule.
func buildTopology() error {
	if err := os.MkdirAll("/run/netns", 0o755); err != nil {
		return err
	}
	for _, node := range Nodes() {
		if err := run("ip", "netns", "add", node); err != nil {
			return err
		}
		if err := run("ip", "-n", node, "link", "set", "lo", "up"); err != nil {
			return err
		}
	}
	if err := sysctl("core", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	// Backbone: one point-to-point link per public host. The host owns a /32
	// and sends everything to the backbone router, like a routed uplink.
	for _, h := range publicHosts {
		dev := coreDev(h.name)
		steps := [][]string{
			{"ip", "-n", "core", "link", "add", dev, "type", "veth", "peer", "name", "eth0", "netns", h.name},
			{"ip", "-n", "core", "addr", "add", coreAddr + "/32", "dev", dev},
			{"ip", "-n", "core", "link", "set", dev, "up"},
			{"ip", "-n", "core", "route", "add", h.addr + "/32", "dev", dev},
			{"ip", "-n", h.name, "addr", "add", h.addr + "/32", "dev", "eth0"},
			{"ip", "-n", h.name, "link", "set", "eth0", "up"},
			{"ip", "-n", h.name, "route", "add", coreAddr + "/32", "dev", "eth0"},
			{"ip", "-n", h.name, "route", "add", "default", "via", coreAddr, "dev", "eth0"},
		}
		for _, addr := range h.extra {
			steps = append(steps,
				[]string{"ip", "-n", "core", "route", "add", addr + "/32", "dev", dev},
				[]string{"ip", "-n", h.name, "addr", "add", addr + "/32", "dev", "eth0"})
		}
		if err := runAll(steps); err != nil {
			return err
		}
	}
	// Any address the lab does not assign is "somewhere on the internet".
	if err := run("ip", "-n", "core", "route", "add", "default", "via", InetAddr, "dev", coreDev("inet")); err != nil {
		return err
	}

	for _, l := range privateLinks {
		steps := [][]string{
			{"ip", "-n", l.router, "link", "add", l.routerDev, "type", "veth", "peer", "name", l.hostDev, "netns", l.host},
			{"ip", "-n", l.router, "addr", "add", l.routerCIDR, "dev", l.routerDev},
			{"ip", "-n", l.router, "link", "set", l.routerDev, "up"},
			{"ip", "-n", l.host, "addr", "add", l.hostCIDR, "dev", l.hostDev},
			{"ip", "-n", l.host, "link", "set", l.hostDev, "up"},
		}
		if l.defaultRoute {
			steps = append(steps, []string{"ip", "-n", l.host, "route", "add", "default", "via", gatewayOf(l.routerCIDR), "dev", l.hostDev})
		}
		if err := runAll(steps); err != nil {
			return err
		}
	}

	if err := attachHost(); err != nil {
		return err
	}

	for _, router := range natRouters {
		if err := sysctl(router, "net.ipv4.ip_forward=1"); err != nil {
			return err
		}
		if err := SetNAT(router, NATCone); err != nil {
			return err
		}
		if err := refuseUnsolicited(router); err != nil {
			return err
		}
	}
	for _, node := range Nodes() {
		if err := installLabChain(node); err != nil {
			return err
		}
	}

	if err := redirectLoopback("user", 8080, BrokerAddr+":8080"); err != nil {
		return err
	}
	if err := buildWSSOrigin(); err != nil {
		return err
	}

	// The internet stand-in answers for every address and port routed to it.
	return In("inet", "iptables", "-t", "nat", "-A", "PREROUTING", "-j", "REDIRECT")
}

// attachHost puts the container's own network namespace behind gw-host, like
// a computer behind a home router, and routes the lab's public ranges there.
// Under `make e2e` that namespace has no other network and nothing uses the
// link. Under `make e2e-lab-host` the container shares this machine's
// namespace, so apps built and run on the machine (the desktop client) reach
// the lab through gw-host, where `lab net` can impair their network. The link
// and its routes vanish with gw-host's namespace when the lab stops.
func attachHost() error {
	steps := [][]string{
		{"ip", "link", "add", HostDev, "type", "veth", "peer", "name", "lan0", "netns", "gw-host"},
		{"ip", "-n", "gw-host", "addr", "add", hostRouterCIDR, "dev", "lan0"},
		{"ip", "-n", "gw-host", "link", "set", "lan0", "up"},
		{"ip", "addr", "add", HostAddr + "/30", "dev", HostDev},
		{"ip", "link", "set", HostDev, "up"},
	}
	for _, prefix := range labPrefixes {
		steps = append(steps, []string{"ip", "route", "add", prefix, "via", gatewayOf(hostRouterCIDR), "dev", HostDev})
	}
	if err := runAll(steps); err != nil {
		return fmt.Errorf("attach the container's namespace (is another lab already attached to this machine?): %w", err)
	}
	return nil
}

// refuseUnsolicited gives a NAT router the inbound firewall home routers
// have: a new connection arriving on its uplink for the router itself is
// refused in the firewall, TCP with a reset and anything else with
// port-unreachable, so it never enters the router's connection tracking.
func refuseUnsolicited(router string) error {
	uplink, err := uplinkOf(router)
	if err != nil {
		return err
	}
	unsolicited := []string{"iptables", "-A", "INPUT", "-i", uplink, "-m", "conntrack", "--ctstate", "NEW"}
	if err := In(router, append(unsolicited, "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset")...); err != nil {
		return err
	}
	return In(router, append(unsolicited, "-j", "REJECT")...)
}

// redirectLoopback makes 127.0.0.1:port inside a node reach a remote address
// across the simulated network. It is address translation, not a local
// proxy: a connection to the loopback port succeeds, stalls or is reset
// exactly as the remote address does from that node.
func redirectLoopback(node string, port int, to string) error {
	return runAll([][]string{
		{"ip", "netns", "exec", node, "sysctl", "-qw", "net.ipv4.conf.all.route_localnet=1"},
		{"ip", "netns", "exec", node, "iptables", "-t", "nat", "-A", "OUTPUT", "-d", "127.0.0.1", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "DNAT", "--to-destination", to},
		{"ip", "netns", "exec", node, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "127.0.0.1", "!", "-o", "lo", "-j", "MASQUERADE"},
	})
}

// destroyTopology removes every lab namespace. Deleting a namespace takes its
// veth pairs, routes, qdiscs and firewall rules with it.
func destroyTopology() {
	for _, node := range Nodes() {
		if _, err := os.Stat("/run/netns/" + node); err == nil {
			_ = run("ip", "netns", "delete", node)
		}
	}
}

func runAll(steps [][]string) error {
	for _, step := range steps {
		if err := run(step...); err != nil {
			return err
		}
	}
	return nil
}

func sysctl(node, setting string) error {
	return In(node, "sysctl", "-qw", setting)
}

// gatewayOf returns the address part of a router CIDR such as 192.168.10.1/24.
func gatewayOf(cidr string) string {
	addr, _, _ := strings.Cut(cidr, "/")
	return addr
}

// uplinkOf returns the default-route interface a NAT router translates behind.
func uplinkOf(router string) (string, error) {
	for _, r := range natRouters {
		if r == router {
			return "eth0", nil
		}
	}
	return "", fmt.Errorf("%q is not a NAT router (want one of %s)", router, strings.Join(natRouters, ", "))
}

func knownNode(node string) error {
	for _, n := range Nodes() {
		if n == node {
			return nil
		}
	}
	return fmt.Errorf("unknown node %q (want one of %s)", node, strings.Join(Nodes(), ", "))
}
