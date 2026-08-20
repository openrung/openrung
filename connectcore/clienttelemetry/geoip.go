package clienttelemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// geoIPEndpoint matches the Android GeoIpClient (ipwho.is). The CLI looks up its
// own public IP geo so the broker dashboard can populate the country/city/ISP
// columns the same way it does for mobile clients.
const geoIPEndpoint = "https://ipwho.is/"

type geoIPResponse struct {
	IP          string `json:"ip"`
	Success     bool   `json:"success"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	Connection  struct {
		ASN int64  `json:"asn"`
		Org string `json:"org"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

// LookupGeoAttributes resolves the caller's public-IP geo and returns telemetry
// attributes keyed exactly as the broker dashboard expects (client_ip, country,
// country_code, city, asn, isp, organization). Best-effort: returns nil on any
// failure so telemetry/connect never depend on it.
func LookupGeoAttributes(ctx context.Context, httpClient *http.Client) map[string]string {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 4 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoIPEndpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil
	}
	return decodeGeoAttributes(body)
}

func decodeGeoAttributes(body []byte) map[string]string {
	var parsed geoIPResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	if !parsed.Success || strings.TrimSpace(parsed.IP) == "" {
		return nil
	}
	attrs := map[string]string{}
	put := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			attrs[key] = value
		}
	}
	put("client_ip", parsed.IP)
	put("country", parsed.Country)
	put("country_code", parsed.CountryCode)
	put("city", parsed.City)
	if parsed.Connection.ASN > 0 {
		attrs["asn"] = fmt.Sprintf("AS%d", parsed.Connection.ASN)
	}
	put("isp", parsed.Connection.ISP)
	put("organization", parsed.Connection.Org)
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}
