// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRequestWSSTicketRejectsEndpointUnboundFrontBeforeHTTP(t *testing.T) {
	called := false
	api := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("transport must not be called")
	})}, Options{})

	for _, brokerURL := range []string{
		AzureBrokerURL,
		"https://ANOTHER-NATIVE-FRONT.z09.AzureFD.net:443/",
	} {
		_, err := api.RequestWSSTicket(t.Context(), brokerURL, WSSTicketRequest{
			RelayID: "relay-a",
			FrontID: "front-a",
		})
		if err == nil || !strings.Contains(err.Error(), "exact broker endpoint") {
			t.Fatalf("RequestWSSTicket(%q) error = %v, want endpoint-authentication refusal", brokerURL, err)
		}
	}
	if called {
		t.Fatal("endpoint-unbound WSS ticket request reached the HTTP transport")
	}
}
