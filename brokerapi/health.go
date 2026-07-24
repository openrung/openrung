// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"io"
	"net/http"
)

// ProbeHealth performs one broker reachability probe. Any HTTP response below
// 500 proves Internet reachability; redirects are returned rather than
// followed, and 5xx responses fail.
func (c *Client) ProbeHealth(ctx context.Context, brokerURL string) error {
	endpoint, err := HealthURL(brokerURL)
	if err != nil {
		return err
	}
	ctx, cancel := withRequestTimeout(ctx, DefaultHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c.addCommonHeaders(req, Identity{})

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusInternalServerError {
		return nil
	}
	return &BrokerStatusError{
		Operation:  "health probe",
		BrokerURL:  brokerURL,
		StatusCode: resp.StatusCode,
		Message:    resp.Status,
	}
}
