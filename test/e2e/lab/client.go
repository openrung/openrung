// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is a running headless client.
type Client struct {
	Name      string
	Node      string
	ProxyPort int
	logStart  int64
}

// clientProxyPort is the local proxy port the lab pins the client to.
const clientProxyPort = 1080

// StartClient runs `openrung-client connect -headless` as a fresh
// installation in the user's node, against the lab broker, with any extra
// `connect` flags. The client trusts the lab CA, so its HTTPS connectivity
// checks reach the internet stand-in.
func StartClient(args ...string) (*Client, error) {
	client := CurrentClient()
	if err := os.RemoveAll(StateDir(client.Name)); err != nil {
		return nil, err
	}
	client.logStart = LogSize(client.Name)
	argv := append([]string{"openrung-client", "connect", "-headless", "-broker", ClientBrokerURL}, args...)
	env := []string{
		"OPENRUNG_PROXY_PORT=" + strconv.Itoa(client.ProxyPort),
		"SSL_CERT_FILE=" + CAPath,
	}
	if err := Start(Service{Name: client.Name, Node: client.Node, Argv: argv, Env: env}); err != nil {
		return nil, err
	}
	return client, nil
}

// CurrentClient is a handle on the client started by an earlier command,
// for fetching through its proxy.
func CurrentClient() *Client {
	return &Client{Name: "client", Node: "user", ProxyPort: clientProxyPort}
}

var connectedVia = regexp.MustCompile(`(?m)^status: connected(?: via (.+))?$`)

// WaitConnected waits for the client to report a working connection and
// returns the label of the relay it connected through.
func (c *Client) WaitConnected(timeout time.Duration) (string, error) {
	var via string
	var failed bool
	err := Eventually(timeout, func() error {
		log := c.Log()
		if match := connectedVia.FindStringSubmatch(log); match != nil {
			via = match[1]
			return nil
		}
		if !Running(c.Name) || strings.Contains(log, "\nstatus: failed") {
			failed = true
			return nil
		}
		return fmt.Errorf("client is not connected yet")
	})
	if failed {
		return "", fmt.Errorf("client failed to connect: %s", tail(c.Log(), 1))
	}
	return via, err
}

// Via returns the label of the relay the client most recently reported being
// connected through, or "" if it has not connected.
func (c *Client) Via() string {
	matches := connectedVia.FindAllStringSubmatch(c.Log(), -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// WaitLog waits for text to appear in this run's client log.
func (c *Client) WaitLog(text string, timeout time.Duration) error {
	return WaitLog(c.Name, c.logStart, text, timeout)
}

// WaitExit waits for the client process to end on its own.
func (c *Client) WaitExit(timeout time.Duration) error {
	return Eventually(timeout, func() error {
		if Running(c.Name) {
			return fmt.Errorf("client is still running")
		}
		return nil
	})
}

// Log returns everything this run of the client has printed.
func (c *Client) Log() string {
	log, _ := ReadLog(c.Name, c.logStart)
	return log
}

// Fetch requests a URL through the client's local proxy and returns the
// body. Hostnames are resolved on the far side of the tunnel.
func (c *Client) Fetch(url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	return OutputInContext(ctx, c.Node, "curl", "-fsS", "--max-time", seconds(timeout),
		"--cacert", CAPath, "-x", fmt.Sprintf("socks5h://127.0.0.1:%d", c.ProxyPort), url)
}

// Download fetches a URL through the client's local proxy, discards the body
// and returns its size in bytes.
func (c *Client) Download(url string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	size, err := OutputInContext(ctx, c.Node, "curl", "-fsS", "--max-time", seconds(timeout),
		"--cacert", CAPath, "-x", fmt.Sprintf("socks5h://127.0.0.1:%d", c.ProxyPort),
		"-o", "/dev/null", "-w", "%{size_download}", url)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(size, 10, 64)
}

// ExitIP returns the address the internet sees this client's traffic coming
// from: the relay's public address when the tunnel carries it.
func (c *Client) ExitIP(timeout time.Duration) (string, error) {
	return c.Fetch("http://exit.example/ip", timeout)
}

// Stop disconnects the client.
func (c *Client) Stop() error { return Stop(c.Name) }

// FetchDirect requests a URL from a node without any proxy.
func FetchDirect(node, url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	return OutputInContext(ctx, node, "curl", "-fsS", "--max-time", seconds(timeout), "--cacert", CAPath, url)
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64)
}

// tail returns the last n lines of text.
func tail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
