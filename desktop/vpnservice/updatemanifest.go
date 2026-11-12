// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
)

// The app's update check reads one signed manifest from a fixed list of
// candidate URLs (openrung-mobile-app docs/UPDATE_MANIFEST.md). The webview
// cannot fetch them itself, so the service fetches a candidate's bytes and the
// app verifies the signature and applies its freshness and rollback rules.
// Only these exact URLs are fetched: the bridge is not a general HTTP client.
const (
	manifestCloudFrontURL = "https://d2r7mdpyevvs1m.cloudfront.net/api/v1/app-manifest"
	manifestBrokerURL     = "https://broker.openrung.org/api/v1/app-manifest"
	manifestGitHubURL     = "https://github.com/openrung/openrung-mobile-app/releases/latest/download/update-manifest.json"

	manifestTimeout      = 10 * time.Second
	manifestMaxBodyBytes = 1 << 20
	manifestMaxRedirects = 5
)

// NativeManifestCandidate is one fetched update-manifest candidate, unverified.
type NativeManifestCandidate struct {
	SourceURL string `json:"sourceUrl"`
	BodyJSON  string `json:"bodyJson"`
}

// manifestClients maps each candidate URL to the client that fetches it. The
// broker fronts use the broker transport, which refuses redirects. The GitHub
// release asset is served through a redirect, so it gets an ordinary client
// that follows a few HTTPS hops. A variable so tests can point the URLs at a
// local server.
var manifestClients = map[string]*http.Client{
	manifestCloudFrontURL: brokerapi.NewHTTPClient(manifestTimeout),
	manifestBrokerURL:     brokerapi.NewHTTPClient(manifestTimeout),
	manifestGitHubURL: {
		Timeout: manifestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("update manifest: redirect is not https")
			}
			if len(via) >= manifestMaxRedirects {
				return errors.New("update manifest: too many redirects")
			}
			return nil
		},
	},
}

// FetchManifestCandidate is Wails-bound. It returns the body of one of the
// app's update-manifest candidates.
func (s *Service) FetchManifestCandidate(candidateURL string) (NativeManifestCandidate, error) {
	httpClient, ok := manifestClients[candidateURL]
	if !ok {
		return NativeManifestCandidate{}, errors.New("update manifest: unsupported candidate URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), manifestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, candidateURL, nil)
	if err != nil {
		return NativeManifestCandidate{}, err
	}
	request.Header.Set("X-OpenRung-App-Version", client.AppVersion())
	request.Header.Set("Cache-Control", "no-cache, no-store")

	response, err := httpClient.Do(request)
	if err != nil {
		return NativeManifestCandidate{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return NativeManifestCandidate{}, fmt.Errorf("update manifest: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, manifestMaxBodyBytes+1))
	if err != nil {
		return NativeManifestCandidate{}, err
	}
	if len(body) > manifestMaxBodyBytes || !utf8.Valid(body) {
		return NativeManifestCandidate{}, errors.New("update manifest: body is too large or not UTF-8")
	}
	return NativeManifestCandidate{SourceURL: candidateURL, BodyJSON: string(body)}, nil
}
