//go:build darwin

package proxymode

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// New returns the macOS proxy controller, which drives `networksetup`.
func New() Controller {
	return &darwinController{
		run: func(args ...string) ([]byte, error) {
			return exec.Command("networksetup", args...).Output()
		},
	}
}

type darwinController struct {
	// run executes networksetup; injectable so tests need no real system.
	run func(args ...string) ([]byte, error)
}

func (c *darwinController) Supported() bool { return true }

func (c *darwinController) Snapshot() (Snapshot, error) {
	services, err := c.listServices()
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Platform: "darwin"}
	for _, name := range services {
		web, err := c.getProxy("-getwebproxy", name)
		if err != nil {
			return Snapshot{}, err
		}
		secure, err := c.getProxy("-getsecurewebproxy", name)
		if err != nil {
			return Snapshot{}, err
		}
		snap.Services = append(snap.Services, ServiceProxyState{
			Name:          name,
			WebEnabled:    web.enabled,
			WebHost:       web.host,
			WebPort:       web.port,
			SecureEnabled: secure.enabled,
			SecureHost:    secure.host,
			SecurePort:    secure.port,
		})
	}
	return snap, nil
}

func (c *darwinController) Set(host string, port int) error {
	services, err := c.listServices()
	if err != nil {
		return err
	}
	portStr := strconv.Itoa(port)
	for _, name := range services {
		// -setwebproxy / -setsecurewebproxy both set the endpoint and enable it.
		if _, err := c.run("-setwebproxy", name, host, portStr); err != nil {
			return fmt.Errorf("set web proxy on %q: %w", name, err)
		}
		if _, err := c.run("-setsecurewebproxy", name, host, portStr); err != nil {
			return fmt.Errorf("set secure proxy on %q: %w", name, err)
		}
	}
	return nil
}

func (c *darwinController) Restore(snap Snapshot) error {
	if snap.Platform != "" && snap.Platform != "darwin" {
		return fmt.Errorf("cannot restore %q proxy snapshot on darwin", snap.Platform)
	}
	for _, svc := range snap.Services {
		if err := c.restoreOne("-setwebproxy", "-setwebproxystate", svc.Name, svc.WebEnabled, svc.WebHost, svc.WebPort); err != nil {
			return err
		}
		if err := c.restoreOne("-setsecurewebproxy", "-setsecurewebproxystate", svc.Name, svc.SecureEnabled, svc.SecureHost, svc.SecurePort); err != nil {
			return err
		}
	}
	return nil
}

func (c *darwinController) restoreOne(setCmd, stateCmd, name string, enabled bool, host string, port int) error {
	if enabled && host != "" {
		if _, err := c.run(setCmd, name, host, strconv.Itoa(port)); err != nil {
			return fmt.Errorf("restore proxy on %q: %w", name, err)
		}
		return nil
	}
	// Was off (or had no endpoint): turn it back off, undoing our Set.
	if _, err := c.run(stateCmd, name, "off"); err != nil {
		return fmt.Errorf("disable proxy on %q: %w", name, err)
	}
	return nil
}

// listServices returns the enabled network services, skipping the informational
// header line and disabled services (which networksetup prefixes with "*").
func (c *darwinController) listServices() ([]string, error) {
	out, err := c.run("-listallnetworkservices")
	if err != nil {
		return nil, fmt.Errorf("list network services: %w", err)
	}
	var services []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.Contains(line, "asterisk") {
			continue // header line
		}
		if strings.HasPrefix(line, "*") {
			continue // disabled service
		}
		services = append(services, line)
	}
	return services, scanner.Err()
}

type proxyReading struct {
	enabled bool
	host    string
	port    int
}

// getProxy parses `networksetup -getwebproxy <service>` output:
//
//	Enabled: Yes
//	Server: 1.2.3.4
//	Port: 8080
//	Authenticated Proxy Enabled: 0
func (c *darwinController) getProxy(subcommand, name string) (proxyReading, error) {
	out, err := c.run(subcommand, name)
	if err != nil {
		return proxyReading{}, fmt.Errorf("%s %q: %w", subcommand, name, err)
	}
	var reading proxyReading
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Enabled":
			reading.enabled = strings.EqualFold(value, "Yes")
		case "Server":
			reading.host = value
		case "Port":
			reading.port, _ = strconv.Atoi(value)
		}
	}
	return reading, scanner.Err()
}
