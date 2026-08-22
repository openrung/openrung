package relayruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openrung/internal/tunnel"
)

// detectAttempts is how many times the relay retries the probe before
// treating the hub HTTP API as unavailable.
const detectAttempts = 3

// DirectProbeOutcome classifies a direct-connect reachability attempt. Bind
// failures are kept separate from external reachability so callers never
// mistake a router/firewall failure for a local OS permission problem.
type DirectProbeOutcome string

const (
	DirectProbeReachable             DirectProbeOutcome = "reachable"
	DirectProbePermissionDenied      DirectProbeOutcome = "permission_denied"
	DirectProbePortInUse             DirectProbeOutcome = "port_in_use"
	DirectProbeExternallyUnreachable DirectProbeOutcome = "externally_unreachable"
	DirectProbeAPIUnavailable        DirectProbeOutcome = "probe_api_unavailable"
	DirectProbeBindFailed            DirectProbeOutcome = "bind_failed"
	DirectProbeInternalFailure       DirectProbeOutcome = "internal_failure"
)

// DirectProbeResult is the structured result of ProbeDirectReachability.
// Err is populated for local bind failures and probe-API failures; a completed
// negative callback is represented by DirectProbeExternallyUnreachable with a
// nil Err.
type DirectProbeResult struct {
	Outcome      DirectProbeOutcome
	ObservedHost string
	Err          error
}

// ProbeDirectReachability opens a temporary TCP listener on port, asks the hub
// to dial it back at the relay's observed public IP, and reports the outcome.
// The temporary listener answers each accepted connection with the nonce line
// so the hub can confirm it reached this relay. Direct mode is safe to
// advertise only when Outcome is DirectProbeReachable; any other Outcome with
// a nil Err means "probed, not reachable" (→ tunnel), and a non-nil Err means
// the probe itself could not run (hub HTTP API unreachable), which the caller
// treats as inconclusive.
func ProbeDirectReachability(ctx context.Context, hubHTTPBase, token, listenHost string, port int, httpClient *http.Client) DirectProbeResult {
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return DirectProbeResult{
			Outcome: DirectProbeInternalFailure,
			Err:     fmt.Errorf("generate probe nonce: %w", err),
		}
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Bind the temporary probe listener to the same interface(s) the real direct
	// listener will use, so the probe reflects true reachability (binding all
	// interfaces when the real listener only binds one could false-positive).
	bindAddr := ProbeBindAddr(listenHost, port)
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return DirectProbeResult{
			Outcome: classifyProbeBindError(err),
			Err:     fmt.Errorf("bind probe listener on %s: %w", bindAddr, err),
		}
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
			return DirectProbeResult{Outcome: DirectProbeAPIUnavailable, Err: ctx.Err()}
		}
		resp, callErr := doProbe(ctx, httpClient, url, token, payload)
		if callErr != nil {
			lastErr = callErr
			select {
			case <-ctx.Done():
				return DirectProbeResult{Outcome: DirectProbeAPIUnavailable, Err: ctx.Err()}
			case <-time.After(time.Second):
			}
			continue
		}
		outcome := DirectProbeExternallyUnreachable
		if resp.Reachable {
			outcome = DirectProbeReachable
		}
		return DirectProbeResult{Outcome: outcome, ObservedHost: resp.ObservedHost}
	}
	return DirectProbeResult{
		Outcome: DirectProbeAPIUnavailable,
		Err:     fmt.Errorf("hub probe endpoint unreachable: %w", lastErr),
	}
}

func classifyProbeBindError(err error) DirectProbeOutcome {
	if errors.Is(err, os.ErrPermission) || os.IsPermission(err) || platformProbePermissionDenied(err) {
		return DirectProbePermissionDenied
	}
	if errors.Is(err, syscall.EADDRINUSE) || platformProbePortInUse(err) {
		return DirectProbePortInUse
	}

	// Go exposes different address-in-use errno values on Unix and Windows.
	// net.Listen's stable cross-platform surface is the wrapped error text, so
	// recognize the messages emitted by both families after checking the
	// portable permission sentinel above.
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "address already in use") ||
		strings.Contains(message, "address is already in use") ||
		strings.Contains(message, "only one usage of each socket address") {
		return DirectProbePortInUse
	}
	return DirectProbeBindFailed
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
