package relayruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrung/internal/tunnel"
)

// detectAttempts is how many times the relay retries the probe before
// treating the hub HTTP API as unavailable.
const detectAttempts = 3

// DetectDirectReachable opens a temporary TCP listener on port, asks the hub to
// dial it back at the relay's observed public IP, and reports whether that
// inbound connection succeeded. The temporary listener answers each accepted
// connection with the nonce line so the hub can confirm it reached this
// relay. reachable=false with err=nil means "probed, not reachable" (→
// tunnel); a non-nil err means the probe itself could not run (hub HTTP API
// unreachable), which the caller treats as inconclusive.
func DetectDirectReachable(ctx context.Context, hubHTTPBase, token, listenHost string, port int, httpClient *http.Client) (reachable bool, observedHost string, err error) {
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return false, "", fmt.Errorf("generate probe nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Bind the temporary probe listener to the same interface(s) the real direct
	// listener will use, so the probe reflects true reachability (binding all
	// interfaces when the real listener only binds one could false-positive).
	bindAddr := ProbeBindAddr(listenHost, port)
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return false, "", fmt.Errorf("bind probe listener on %s: %w", bindAddr, err)
	}
	defer ln.Close()

	// Serve the nonce line to whoever connects (the hub's probe dial).
	line := []byte(tunnel.ProbeLinePrefix + nonce + "\n")
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write(line)
			}(conn)
		}
	}()

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(hubHTTPBase, "/") + tunnel.PathProbe
	payload, _ := json.Marshal(tunnel.ProbeRequest{Port: port, Nonce: nonce})

	var lastErr error
	for attempt := 0; attempt < detectAttempts; attempt++ {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		resp, callErr := doProbe(ctx, httpClient, url, token, payload)
		if callErr != nil {
			lastErr = callErr
			select {
			case <-ctx.Done():
				return false, "", ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		return resp.Reachable, resp.ObservedHost, nil
	}
	return false, "", fmt.Errorf("hub probe endpoint unreachable: %w", lastErr)
}

func doProbe(ctx context.Context, httpClient *http.Client, url, token string, payload []byte) (tunnel.ProbeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return tunnel.ProbeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return tunnel.ProbeResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tunnel.ProbeResponse{}, fmt.Errorf("probe status %d", resp.StatusCode)
	}
	var out tunnel.ProbeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil {
		return tunnel.ProbeResponse{}, err
	}
	return out, nil
}

// DeriveHubHTTPBase turns a hub control address (host:port) into the hub HTTP API
// base URL, defaulting to <scheme>://<host>:9444 when no explicit URL is given.
// The scheme follows the control-channel TLS setting: a TLS hub also serves its
// HTTP API over TLS.
func DeriveHubHTTPBase(explicit, hubAddr string, useTLS bool) string {
	if explicit != "" {
		return explicit
	}
	host := hubAddr
	if h, _, err := net.SplitHostPort(hubAddr); err == nil {
		host = h
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, "9444")
}

// ProbeBindAddr returns the address the temporary probe listener should bind so
// that it matches the interfaces the real direct listener will serve on.
func ProbeBindAddr(listenHost string, port int) string {
	switch strings.ToLower(strings.TrimSpace(listenHost)) {
	case "", "::", "dual", "both":
		return ":" + strconv.Itoa(port) // all interfaces (dual-stack)
	default:
		return net.JoinHostPort(listenHost, strconv.Itoa(port))
	}
}
