// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package proxymode

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// NewDNS returns the macOS resolver controller, which drives `networksetup`.
//
// It sets each enabled network service's DNS servers, the same per-service
// configuration a user edits in System Settings. configd treats user-set
// servers as not bound to the service's interface, so the system resolver
// sends its queries by the routing table, which in TUN mode leads into the
// tunnel. Changing a service's DNS needs administrator rights, which TUN mode
// already has.
func NewDNS() DNSController {
	return &darwinDNSController{
		run: func(args ...string) ([]byte, error) {
			return exec.Command("networksetup", args...).Output()
		},
	}
}

type darwinDNSController struct {
	// run executes networksetup; injectable so tests need no real system.
	run func(args ...string) ([]byte, error)
}

func (c *darwinDNSController) Supported() bool { return true }

// Snapshot captures the enabled services that prev does not hold yet, so a
// service that appeared since the last Set (a USB adapter plugged in during a
// session) is recorded before Set first changes it.
func (c *darwinDNSController) Snapshot(prev Snapshot) (Snapshot, error) {
	if prev.Platform != "" && prev.Platform != "darwin" {
		return Snapshot{}, fmt.Errorf("cannot extend %q snapshot on darwin", prev.Platform)
	}
	services, err := listServices(c.run)
	if err != nil {
		return Snapshot{}, err
	}
	snap := prev
	snap.Platform = "darwin"
	snap.DNS = append([]ServiceDNSState{}, prev.DNS...)
	for _, name := range services {
		if snapshotHolds(snap, name) {
			continue
		}
		out, err := c.run("-getdnsservers", name)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read DNS servers of %q: %w", name, err)
		}
		servers, err := parseDNSServers(out)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read DNS servers of %q: %w", name, err)
		}
		snap.DNS = append(snap.DNS, ServiceDNSState{Name: name, Servers: servers})
	}
	return snap, nil
}

// Set changes only services that are both in snap and enabled now: a service
// absent from snap has no captured setting to restore, and one disabled or
// deleted since the snapshot carries no traffic.
func (c *darwinDNSController) Set(server string, snap Snapshot) error {
	if _, err := netip.ParseAddr(server); err != nil {
		return fmt.Errorf("DNS server is not an IP address: %q", server)
	}
	services, err := listServices(c.run)
	if err != nil {
		return err
	}
	for _, name := range services {
		if !snapshotHolds(snap, name) {
			continue
		}
		if _, err := c.run("-setdnsservers", name, server); err != nil {
			return fmt.Errorf("set DNS servers of %q: %w", name, err)
		}
	}
	return nil
}

// Restore puts every captured service that still exists back, continuing past
// failures so one broken service does not strand the others; the joined error
// keeps the caller's snapshot for a retry. A service deleted since the
// snapshot has nothing left to restore and is skipped, so it cannot hold the
// snapshot pending forever. A snapshot without DNS state (a proxy snapshot
// sharing the persisted slot) restores nothing.
func (c *darwinDNSController) Restore(snap Snapshot) error {
	if snap.Platform != "" && snap.Platform != "darwin" {
		return fmt.Errorf("cannot restore %q DNS snapshot on darwin", snap.Platform)
	}
	if len(snap.DNS) == 0 {
		return nil
	}
	existing, err := listAllServices(c.run)
	if err != nil {
		return err
	}
	var errs []error
	for _, svc := range snap.DNS {
		if !existing[svc.Name] {
			continue
		}
		args := []string{"-setdnsservers", svc.Name}
		if len(svc.Servers) == 0 {
			args = append(args, "Empty") // networksetup's "none configured"
		} else {
			args = append(args, svc.Servers...)
		}
		if _, err := c.run(args...); err != nil {
			errs = append(errs, fmt.Errorf("restore DNS servers of %q: %w", svc.Name, err))
		}
	}
	return errors.Join(errs...)
}

func snapshotHolds(snap Snapshot, name string) bool {
	for _, svc := range snap.DNS {
		if svc.Name == name {
			return true
		}
	}
	return false
}

// listAllServices returns every network service, enabled or not (networksetup
// marks disabled ones with a leading "*").
func listAllServices(run func(args ...string) ([]byte, error)) (map[string]bool, error) {
	out, err := run("-listallnetworkservices")
	if err != nil {
		return nil, fmt.Errorf("list network services: %w", err)
	}
	all := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.Contains(line, "asterisk") {
			continue // header line
		}
		all[strings.TrimPrefix(line, "*")] = true
	}
	return all, scanner.Err()
}

// parseDNSServers parses `networksetup -getdnsservers <service>` output: one
// address per line, or a single sentence when none are configured. Any other
// line is refused rather than guessed at, so a Restore never writes back
// something that was not an address.
func parseDNSServers(out []byte) ([]string, error) {
	text := strings.TrimSpace(string(out))
	if strings.HasPrefix(text, "There aren't any DNS Servers set") {
		return nil, nil
	}
	var servers []string
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, err := netip.ParseAddr(line); err != nil {
			return nil, fmt.Errorf("unexpected networksetup output %q", line)
		}
		servers = append(servers, line)
	}
	return servers, scanner.Err()
}
