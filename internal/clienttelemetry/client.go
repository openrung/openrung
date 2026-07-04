package clienttelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPClient posts telemetry batches to the broker. It is the CLI analog of the
// Android TelemetryClient.
type HTTPClient struct {
	BaseURL string
	HTTP    *http.Client
}

// Send posts the given events to POST /api/v1/telemetry/events. It is a no-op for
// an empty slice. Identity headers are taken from the first event, matching the
// Android client and the broker's recordClientSeen expectations.
func (c HTTPClient) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	endpoint, err := TelemetryURL(c.BaseURL)
	if err != nil {
		return err
	}

	body, err := json.Marshal(batch{Events: events})
	if err != nil {
		return fmt.Errorf("encode telemetry batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenRung-Client-ID", events[0].ClientID)
	req.Header.Set("X-OpenRung-Session-ID", events[0].SessionID)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return telemetryStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// TelemetryURL builds the telemetry endpoint URL from a broker base URL,
// mirroring RelayListURL in internal/client/broker.go.
func TelemetryURL(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", fmt.Errorf("broker URL is required")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse broker URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("broker URL must include scheme and host")
	}

	basePath := strings.Trim(parsed.Path, "/")
	pathParts := []string{"api/v1/telemetry/events"}
	if basePath != "" {
		pathParts = append([]string{basePath}, pathParts...)
	}
	parsed.Path = "/" + strings.Join(pathParts, "/")
	parsed.RawQuery = ""

	return parsed.String(), nil
}

func telemetryStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apiErr struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &apiErr)
	message := apiErr.Error
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("broker telemetry: %s", message)
}
