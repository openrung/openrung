// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// labChain holds every filtering rule the lab adds to a node, so clearing a
// node's impairments is a single flush.
const labChain = "LAB"

// ErrNoNetem reports that the kernel has no netem qdisc, so added delay and
// jitter cannot be simulated. Rate limits and loss still work without it.
var ErrNoNetem = errors.New("kernel has no netem qdisc: delay and jitter are unavailable")

// NATMode is how a router maps private flows onto its public address.
type NATMode string

const (
	// NATCone keeps one public port per private socket whatever the
	// destination (endpoint-independent mapping), and only lets replies back
	// in. This is the common home-router behaviour and the default.
	NATCone NATMode = "cone"
	// NATSymmetric picks a fresh public port for every destination, which
	// defeats hole punching.
	NATSymmetric NATMode = "symmetric"
)

// SetNAT switches a router's NAT behaviour. A change of mode drops the
// router's existing mappings, so flows through it break and must be
// re-established; asking for the current mode does nothing.
func SetNAT(router string, mode NATMode) error {
	_, err := setNAT(router, mode)
	return err
}

// setNAT is SetNAT that also reports whether existing mappings were dropped.
func setNAT(router string, mode NATMode) (changed bool, err error) {
	uplink, err := uplinkOf(router)
	if err != nil {
		return false, err
	}
	rule := []string{"POSTROUTING", "-o", uplink, "-j", "MASQUERADE"}
	switch mode {
	case NATCone:
	case NATSymmetric:
		rule = append(rule, "--random-fully")
	default:
		return false, fmt.Errorf("unknown NAT mode %q (want %s or %s)", mode, NATCone, NATSymmetric)
	}
	current, err := OutputIn(router, "iptables", "-t", "nat", "-S", "POSTROUTING")
	if err != nil {
		return false, err
	}
	if !strings.Contains(current, "MASQUERADE") {
		return false, In(router, append([]string{"iptables", "-t", "nat", "-A"}, rule...)...)
	}
	if strings.Contains(current, "--random-fully") == (mode == NATSymmetric) {
		return false, nil
	}
	// Replace in place so no packet leaves untranslated in between.
	replace := append([]string{"iptables", "-t", "nat", "-R", "POSTROUTING", "1"}, rule[1:]...)
	if err := In(router, replace...); err != nil {
		return false, err
	}
	return true, In(router, "conntrack", "-F")
}

// PortForward publishes a port of the host behind a router on the router's
// public address, like a manual port-forward on a home router. ClearNode
// removes it again.
func PortForward(router string, port int, to string) error {
	uplink, err := uplinkOf(router)
	if err != nil {
		return err
	}
	return In(router, "iptables", "-t", "nat", "-A", "PREROUTING", "-i", uplink, "-p", "tcp",
		"--dport", strconv.Itoa(port), "-j", "DNAT", "--to-destination", to)
}

// Shape describes the quality of a link. Zero fields leave that property
// untouched, and the zero Shape removes shaping.
type Shape struct {
	Delay  time.Duration // added one-way latency
	Jitter time.Duration // variation around Delay
	Loss   float64       // percent of packets dropped, 0-100
	Rate   string        // tc rate such as "512kbit" or "5mbit"
}

func (s Shape) String() string {
	var parts []string
	if s.Delay > 0 {
		parts = append(parts, "delay "+s.Delay.String())
	}
	if s.Jitter > 0 {
		parts = append(parts, "jitter "+s.Jitter.String())
	}
	if s.Loss > 0 {
		parts = append(parts, fmt.Sprintf("loss %g%%", s.Loss))
	}
	if s.Rate != "" {
		parts = append(parts, "rate "+s.Rate)
	}
	if len(parts) == 0 {
		return "unshaped"
	}
	return strings.Join(parts, ", ")
}

// Profiles are named link qualities for common access networks. The numbers
// are round, illustrative values, not measurements.
var Profiles = map[string]Shape{
	"broadband":   {Delay: 10 * time.Millisecond, Rate: "50mbit"},
	"dsl":         {Delay: 25 * time.Millisecond, Rate: "8mbit"},
	"mobile-4g":   {Delay: 40 * time.Millisecond, Jitter: 15 * time.Millisecond, Loss: 0.5, Rate: "20mbit"},
	"mobile-3g":   {Delay: 150 * time.Millisecond, Jitter: 50 * time.Millisecond, Loss: 1.5, Rate: "1500kbit"},
	"mobile-edge": {Delay: 400 * time.Millisecond, Jitter: 100 * time.Millisecond, Loss: 2, Rate: "200kbit"},
	"satellite":   {Delay: 300 * time.Millisecond, Jitter: 20 * time.Millisecond, Loss: 0.5, Rate: "10mbit"},
	"lossy-wifi":  {Delay: 5 * time.Millisecond, Jitter: 20 * time.Millisecond, Loss: 8},
	"congested":   {Delay: 100 * time.Millisecond, Jitter: 80 * time.Millisecond, Loss: 4, Rate: "1mbit"},
}

// ProfileNames returns the profile names, sorted.
func ProfileNames() []string {
	names := make([]string, 0, len(Profiles))
	for name := range Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ShapeLink applies a shape to both directions of a link, so Delay is added
// each way and a round trip pays it twice.
func ShapeLink(link string, shape Shape) error {
	ends, ok := Links()[link]
	if !ok {
		return fmt.Errorf("unknown link %q (want one of %s)", link, strings.Join(LinkNames(), ", "))
	}
	if (shape.Delay > 0 || shape.Jitter > 0) && !HasNetem() {
		return ErrNoNetem
	}
	for _, end := range ends {
		if err := shapeEnd(end, shape); err != nil {
			return err
		}
	}
	return nil
}

func shapeEnd(end End, shape Shape) error {
	// Start from a clean interface: drop the old qdisc and any loss rule.
	_ = In(end.Node, "tc", "qdisc", "del", "dev", end.Dev, "root")
	if err := deleteRules(end.Node, lossRulePrefix(end.Dev)); err != nil {
		return err
	}
	if shape == (Shape{}) {
		return nil
	}
	if HasNetem() {
		args := []string{"tc", "qdisc", "add", "dev", end.Dev, "root", "netem"}
		if shape.Delay > 0 || shape.Jitter > 0 {
			args = append(args, "delay", tcDuration(shape.Delay))
			if shape.Jitter > 0 {
				args = append(args, tcDuration(shape.Jitter))
			}
		}
		if shape.Loss > 0 {
			args = append(args, "loss", fmt.Sprintf("%g%%", shape.Loss))
		}
		if shape.Rate != "" {
			args = append(args, "rate", shape.Rate)
		}
		return In(end.Node, args...)
	}
	// Without netem, rate comes from a token bucket on the way out and loss
	// from a random-drop firewall rule on the way in. Each end of the link
	// drops what it receives, so both directions lose packets, and a node's
	// own sockets never see a local send error.
	if shape.Rate != "" {
		if err := In(end.Node, "tc", "qdisc", "add", "dev", end.Dev, "root", "tbf", "rate", shape.Rate, "burst", "32kbit", "latency", "400ms"); err != nil {
			return err
		}
	}
	if shape.Loss > 0 {
		rule := append(lossRulePrefix(end.Dev), "--mode", "random", "--probability", fmt.Sprintf("%.6f", shape.Loss/100), "-j", "DROP")
		return In(end.Node, append([]string{"iptables", "-A", labChain}, rule...)...)
	}
	return nil
}

func lossRulePrefix(dev string) []string { return []string{"-i", dev, "-m", "statistic"} }

func tcDuration(d time.Duration) string {
	return strconv.FormatInt(d.Microseconds(), 10) + "us"
}

var netem struct {
	once sync.Once
	ok   bool
}

// HasIPv6 reports whether the lab's nodes have a working IPv6 stack.
func HasIPv6() bool {
	disabled, err := OutputIn("user", "cat", "/proc/sys/net/ipv6/conf/all/disable_ipv6")
	return err == nil && disabled == "0"
}

// HasNetem reports whether the kernel can add delay and jitter.
func HasNetem() bool {
	netem.once.Do(func() {
		// Probe on the backbone's loopback, which carries no lab traffic.
		if err := In("core", "tc", "qdisc", "add", "dev", "lo", "root", "netem", "delay", "1ms"); err == nil {
			netem.ok = true
			_ = In("core", "tc", "qdisc", "del", "dev", "lo", "root")
		}
	})
	return netem.ok
}

// Action is what a filtering rule does with a matching packet.
type Action string

const (
	// Drop discards silently: the sender sees a timeout.
	Drop Action = "drop"
	// Reset answers TCP with a RST: the sender sees "connection reset".
	Reset Action = "reset"
	// Unreachable answers with ICMP host-unreachable.
	Unreachable Action = "unreachable"
)

// Match selects packets. Empty fields match everything.
type Match struct {
	Src   string // address or CIDR
	Dst   string // address or CIDR
	Proto string // "tcp" or "udp"
	Port  int    // destination port; needs Proto
}

func (m Match) args() ([]string, error) {
	var args []string
	if m.Src != "" {
		args = append(args, "-s", m.Src)
	}
	if m.Dst != "" {
		args = append(args, "-d", m.Dst)
	}
	if m.Proto != "" {
		if m.Proto != "tcp" && m.Proto != "udp" {
			return nil, fmt.Errorf("unknown protocol %q (want tcp or udp)", m.Proto)
		}
		args = append(args, "-p", m.Proto)
	}
	if m.Port != 0 {
		if m.Proto == "" {
			return nil, errors.New("a port needs a protocol")
		}
		args = append(args, "--dport", strconv.Itoa(m.Port))
	}
	return args, nil
}

func (a Action) args(m Match) ([]string, error) {
	switch a {
	case Drop:
		return []string{"-j", "DROP"}, nil
	case Reset:
		if m.Proto != "tcp" {
			return nil, errors.New("reset needs protocol tcp")
		}
		return []string{"-j", "REJECT", "--reject-with", "tcp-reset"}, nil
	case Unreachable:
		return []string{"-j", "REJECT", "--reject-with", "icmp-host-unreachable"}, nil
	}
	return nil, fmt.Errorf("unknown action %q (want %s, %s or %s)", a, Drop, Reset, Unreachable)
}

// Block makes a node refuse matching packets, whether it forwards them (a
// router) or sends and receives them itself. The returned function removes
// the rule again.
func Block(node string, m Match, action Action) (undo func() error, err error) {
	if err := knownNode(node); err != nil {
		return nil, err
	}
	spec, err := m.args()
	if err != nil {
		return nil, err
	}
	verdict, err := action.args(m)
	if err != nil {
		return nil, err
	}
	spec = append(spec, verdict...)
	if err := In(node, append([]string{"iptables", "-A", labChain}, spec...)...); err != nil {
		return nil, err
	}
	// Established flows keep their conntrack entry, but the rule sits in
	// front of it, so they stall too; that is what a new block looks like.
	return func() error {
		return In(node, append([]string{"iptables", "-D", labChain}, spec...)...)
	}, nil
}

// AllowOnly turns a node into a restrictive firewall: only the listed
// destinations pass (plus replies), everything else gets action. Use it for
// networks that permit web ports and nothing else.
func AllowOnly(node string, allowed []Match, action Action) error {
	if err := knownNode(node); err != nil {
		return err
	}
	rules := [][]string{{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}}
	for _, m := range allowed {
		spec, err := m.args()
		if err != nil {
			return err
		}
		rules = append(rules, append(spec, "-j", "RETURN"))
	}
	if action == Reset {
		rules = append(rules, []string{"-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset"})
		action = Drop
	}
	verdict, err := action.args(Match{})
	if err != nil {
		return err
	}
	rules = append(rules, verdict)
	for _, rule := range rules {
		if err := In(node, append([]string{"iptables", "-A", labChain}, rule...)...); err != nil {
			return err
		}
	}
	return nil
}

// SetLinkDown cuts a link in both directions without removing its routes,
// like a cable pulled or a radio out of range. SetLinkDown(link, false)
// restores it.
func SetLinkDown(link string, down bool) error {
	ends, ok := Links()[link]
	if !ok {
		return fmt.Errorf("unknown link %q (want one of %s)", link, strings.Join(LinkNames(), ", "))
	}
	end := ends[0]
	for _, dir := range []string{"-i", "-o"} {
		spec := []string{dir, end.Dev, "-j", "DROP"}
		if err := deleteRules(end.Node, spec); err != nil {
			return err
		}
		if !down {
			continue
		}
		if err := In(end.Node, append([]string{"iptables", "-I", labChain}, spec...)...); err != nil {
			return err
		}
	}
	return nil
}

// ClearNode removes every filtering rule, qdisc and port forward the lab
// added to a node.
func ClearNode(node string) error {
	if err := knownNode(node); err != nil {
		return err
	}
	if err := In(node, "iptables", "-F", labChain); err != nil {
		return err
	}
	if _, err := uplinkOf(node); err == nil {
		// On a NAT router the only inbound translations are port forwards.
		if err := In(node, "iptables", "-t", "nat", "-F", "PREROUTING"); err != nil {
			return err
		}
	}
	for _, ends := range Links() {
		for _, end := range ends {
			if end.Node == node {
				_ = In(node, "tc", "qdisc", "del", "dev", end.Dev, "root")
			}
		}
	}
	return nil
}

// ClearNetwork returns the whole lab to its pristine network: no shaping, no
// blocks, no port forwards, cone NAT everywhere.
func ClearNetwork() error {
	_, err := clearNetwork()
	return err
}

// clearNetwork is ClearNetwork that also reports the routers whose NAT mode
// it had to change, and so whose mappings it dropped.
func clearNetwork() (remapped []string, err error) {
	for _, node := range Nodes() {
		if err := ClearNode(node); err != nil {
			return nil, err
		}
	}
	for _, router := range natRouters {
		changed, err := setNAT(router, NATCone)
		if err != nil {
			return nil, err
		}
		if changed {
			remapped = append(remapped, router)
		}
	}
	return remapped, nil
}

// DescribeNetwork lists the active impairments per node, for `lab status`.
func DescribeNetwork() (string, error) {
	var b strings.Builder
	for _, node := range Nodes() {
		rules, err := OutputIn(node, "iptables", "-S", labChain)
		if err != nil {
			return "", err
		}
		var lines []string
		for _, line := range strings.Split(rules, "\n") {
			if strings.HasPrefix(line, "-A ") {
				lines = append(lines, "rule  "+strings.TrimPrefix(line, "-A "+labChain+" "))
			}
		}
		qdiscs, err := OutputIn(node, "tc", "qdisc", "show")
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(qdiscs, "\n") {
			if strings.Contains(line, "netem") || strings.Contains(line, "tbf") {
				lines = append(lines, "qdisc "+strings.TrimSpace(line))
			}
		}
		if _, err := uplinkOf(node); err == nil {
			nat, err := OutputIn(node, "iptables", "-t", "nat", "-S")
			if err != nil {
				return "", err
			}
			if strings.Contains(nat, "--random-fully") {
				lines = append(lines, "nat   symmetric")
			}
			for _, line := range strings.Split(nat, "\n") {
				if strings.HasPrefix(line, "-A PREROUTING ") {
					lines = append(lines, "fwd   "+strings.TrimPrefix(line, "-A PREROUTING "))
				}
			}
		}
		for _, line := range lines {
			fmt.Fprintf(&b, "%-10s %s\n", node, line)
		}
	}
	if b.Len() == 0 {
		return "no impairments\n", nil
	}
	return b.String(), nil
}

// installLabChain creates the lab chain in a node and sends every packet the
// node receives, forwards or sends through it.
func installLabChain(node string) error {
	if err := In(node, "iptables", "-N", labChain); err != nil {
		return err
	}
	for _, hook := range []string{"INPUT", "FORWARD", "OUTPUT"} {
		// First in each hook, so an impairment also silences what the node
		// would otherwise answer itself.
		if err := In(node, "iptables", "-I", hook, "1", "-j", labChain); err != nil {
			return err
		}
	}
	return nil
}

// deleteRules removes every lab-chain rule in a node whose spec starts with
// the given arguments.
func deleteRules(node string, prefix []string) error {
	rules, err := OutputIn(node, "iptables", "-S", labChain)
	if err != nil {
		return err
	}
	want := "-A " + labChain + " " + strings.Join(prefix, " ")
	for _, line := range strings.Split(rules, "\n") {
		if !strings.HasPrefix(line, want) {
			continue
		}
		spec := strings.Fields(strings.TrimPrefix(line, "-A "))
		if err := In(node, append([]string{"iptables", "-D"}, spec...)...); err != nil {
			return err
		}
	}
	return nil
}
