package punchcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HubClient talks to the relay hub's punch coordinator over HTTP. The base URL is
// the hub's punch listener (e.g. https://hub:9444), which is distinct from the
// broker and not Cloudflare-fronted.
type HubClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func (c HubClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// HardenedHTTPClient returns the hardened default used by the mobile client when
// no coordinator pin applies: 10s timeout, redirects refused, keep-alives disabled.
func HardenedHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

// FetchConfig retrieves the reflector addresses and punch parameters.
func (c HubClient) FetchConfig(ctx context.Context) (PunchConfig, error) {
	url := strings.TrimRight(c.BaseURL, "/") + PathPunchConfig
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PunchConfig{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return PunchConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PunchConfig{}, fmt.Errorf("punch config: unexpected status %d", resp.StatusCode)
	}
	var cfg PunchConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&cfg); err != nil {
		return PunchConfig{}, fmt.Errorf("decode punch config: %w", err)
	}
	return cfg, nil
}

// RequestPunch asks the hub to coordinate a punch session with the volunteer.
func (c HubClient) RequestPunch(ctx context.Context, reqBody PunchRequest) (PunchResponse, error) {
	var out PunchResponse
	if err := c.postJSON(ctx, PathPunchRequest, reqBody, &out); err != nil {
		return PunchResponse{}, err
	}
	return out, nil
}

// ReportResult posts best-effort punch telemetry to the hub.
func (c HubClient) ReportResult(ctx context.Context, res PunchResult) {
	_ = c.postJSON(ctx, PathPunchResult, res, nil)
}

func (c HubClient) postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HubHTTPError{Path: path, StatusCode: resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(out); err != nil {
			return fmt.Errorf("decode punch response: %w", err)
		}
	}
	return nil
}

// HubHTTPError is returned for non-2xx hub punch responses so callers can
// distinguish a stale/unknown relay (404/409 → re-fetch) from other failures.
type HubHTTPError struct {
	Path       string
	StatusCode int
}

func (e *HubHTTPError) Error() string {
	return fmt.Sprintf("punch hub %s: status %d", e.Path, e.StatusCode)
}
