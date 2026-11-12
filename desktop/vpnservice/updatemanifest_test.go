// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// stubManifestCandidate answers one candidate URL from respond instead of the
// network, keeping that candidate's redirect policy.
func stubManifestCandidate(t *testing.T, candidateURL string, respond roundTripFunc) {
	t.Helper()
	previous := manifestClients[candidateURL]
	t.Cleanup(func() { manifestClients[candidateURL] = previous })
	stubbed := *previous
	stubbed.Transport = respond
	manifestClients[candidateURL] = &stubbed
}

func manifestResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestFetchManifestCandidateReturnsTheBody(t *testing.T) {
	const envelope = `{"schema":1,"payload_b64":"e30="}`
	for _, candidateURL := range []string{manifestCloudFrontURL, manifestBrokerURL, manifestGitHubURL} {
		var requested *http.Request
		stubManifestCandidate(t, candidateURL, func(request *http.Request) (*http.Response, error) {
			requested = request
			return manifestResponse(request, http.StatusOK, envelope), nil
		})

		candidate, err := (&Service{}).FetchManifestCandidate(candidateURL)
		if err != nil {
			t.Fatalf("%s: %v", candidateURL, err)
		}
		if candidate.SourceURL != candidateURL || candidate.BodyJSON != envelope {
			t.Fatalf("%s: got %+v", candidateURL, candidate)
		}
		if requested.URL.String() != candidateURL || requested.Method != http.MethodGet {
			t.Fatalf("%s: requested %s %s", candidateURL, requested.Method, requested.URL)
		}
		if requested.Header.Get("X-OpenRung-App-Version") == "" {
			t.Fatalf("%s: request carries no app version", candidateURL)
		}
	}
}

func TestFetchManifestCandidateRefusesOtherURLs(t *testing.T) {
	for _, candidateURL := range []string{
		"",
		"https://example.com/update-manifest.json",
		manifestBrokerURL + "?x=1",
		"http://broker.openrung.org/api/v1/app-manifest",
		"https://github.com/openrung/openrung-mobile-app/releases/latest/download/other.json",
	} {
		if _, err := (&Service{}).FetchManifestCandidate(candidateURL); err == nil {
			t.Fatalf("%q was fetched", candidateURL)
		}
	}
}

func TestFetchManifestCandidateRejectsBadResponses(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"not found":    {http.StatusNotFound, "missing"},
		"server error": {http.StatusBadGateway, ""},
		"oversized":    {http.StatusOK, strings.Repeat("a", manifestMaxBodyBytes+1)},
		"not UTF-8":    {http.StatusOK, "\xff\xfe"},
	}
	for name, tc := range cases {
		stubManifestCandidate(t, manifestBrokerURL, func(request *http.Request) (*http.Response, error) {
			return manifestResponse(request, tc.status, tc.body), nil
		})
		if _, err := (&Service{}).FetchManifestCandidate(manifestBrokerURL); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestManifestRedirectPolicy(t *testing.T) {
	redirect := func(location string) roundTripFunc {
		return func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "github.com" {
				return manifestResponse(request, http.StatusOK, "{}"), nil
			}
			response := manifestResponse(request, http.StatusFound, "")
			response.Header.Set("Location", location)
			return response, nil
		}
	}

	// The release asset is served through a redirect to another HTTPS host.
	stubManifestCandidate(t, manifestGitHubURL, redirect("https://objects.example.com/asset"))
	if _, err := (&Service{}).FetchManifestCandidate(manifestGitHubURL); err != nil {
		t.Fatalf("https redirect: %v", err)
	}

	stubManifestCandidate(t, manifestGitHubURL, redirect("http://objects.example.com/asset"))
	if _, err := (&Service{}).FetchManifestCandidate(manifestGitHubURL); err == nil {
		t.Fatal("followed a redirect to plain http")
	}

	// The broker fronts never redirect.
	stubManifestCandidate(t, manifestBrokerURL, func(request *http.Request) (*http.Response, error) {
		response := manifestResponse(request, http.StatusFound, "")
		response.Header.Set("Location", "https://example.com/elsewhere")
		return response, nil
	})
	if _, err := (&Service{}).FetchManifestCandidate(manifestBrokerURL); err == nil {
		t.Fatal("followed a redirect from a broker front")
	}
}
