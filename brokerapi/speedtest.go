// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	DefaultSpeedTestWarmupBytes      = 1_000_000
	DefaultSpeedTestMeasurementBytes = 10_000_000
	MaxSpeedTestBytes                = 25_000_000
)

// SpeedTestResult contains the streamed measurement. DownloadDuration starts
// when response headers arrive, excluding TLS and TTFB; TotalDuration and
// MegabitsPerSecond cover the complete request, matching existing mobile
// telemetry semantics.
type SpeedTestResult struct {
	Bytes             int64
	TTFB              time.Duration
	DownloadDuration  time.Duration
	TotalDuration     time.Duration
	MegabitsPerSecond float64
}

// DownloadSpeedTest performs one streaming speed-test request without
// buffering the response.
func (c *Client) DownloadSpeedTest(ctx context.Context, brokerURL string, byteCount int) (SpeedTestResult, error) {
	endpoint, err := SpeedTestURL(brokerURL, byteCount)
	if err != nil {
		return SpeedTestResult{}, err
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return SpeedTestResult{}, err
	}
	query := parsedEndpoint.Query()
	query.Set("cacheBust", strconv.FormatInt(time.Now().UnixNano(), 10))
	parsedEndpoint.RawQuery = query.Encode()
	endpoint = parsedEndpoint.String()
	ctx, cancel := withRequestTimeout(ctx, DefaultSpeedTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SpeedTestResult{}, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Accept-Encoding", "identity")
	c.addCommonHeaders(req, Identity{})

	started := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SpeedTestResult{}, err
	}
	headersAt := time.Now()
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return SpeedTestResult{}, statusError(resp, "speed test", brokerURL)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, int64(byteCount)+1))
	finished := time.Now()
	if err != nil {
		return SpeedTestResult{}, fmt.Errorf("read speed-test response: %w", err)
	}
	if n != int64(byteCount) {
		return SpeedTestResult{}, fmt.Errorf("speed-test response is %d bytes, want %d", n, byteCount)
	}
	downloadDuration := finished.Sub(headersAt)
	totalDuration := finished.Sub(started)
	mbps := 0.0
	if totalDuration > 0 {
		mbps = float64(n*8) / totalDuration.Seconds() / 1_000_000
	}
	return SpeedTestResult{
		Bytes:             n,
		TTFB:              headersAt.Sub(started),
		DownloadDuration:  downloadDuration,
		TotalDuration:     totalDuration,
		MegabitsPerSecond: mbps,
	}, nil
}

// RunSpeedTest performs the standard unmeasured 1 MB warm-up followed by the
// measured 10 MB stream used by OpenRung mobile clients.
func (c *Client) RunSpeedTest(ctx context.Context, brokerURL string) (SpeedTestResult, error) {
	if _, err := c.DownloadSpeedTest(ctx, brokerURL, DefaultSpeedTestWarmupBytes); err != nil {
		return SpeedTestResult{}, fmt.Errorf("speed-test warm-up: %w", err)
	}
	return c.DownloadSpeedTest(ctx, brokerURL, DefaultSpeedTestMeasurementBytes)
}
