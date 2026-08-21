// SPDX-License-Identifier: GPL-3.0-or-later

package relay_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

// The broker serializes internal/relay.Descriptor while every client decodes
// brokerapi.RelayDescriptor; the two structs promise "identical JSON tags in
// identical order" in comments. This suite is the machine check behind that
// promise: only the root module can see both models, so it — not the contract
// vectors, which moved into the connectcore module — owns their parity.

// flattenedJSONTags walks a struct's fields in declaration order, recursing
// into anonymous embedded structs exactly as encoding/json flattens them, and
// returns the JSON tags of every serialized field. json:"-" fields are the
// server-only extras the client schema deliberately omits.
func flattenedJSONTags(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var tags []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		if field.Anonymous && field.Type.Kind() == reflect.Struct && tag == "" {
			tags = append(tags, flattenedJSONTags(t, field.Type)...)
			continue
		}
		if tag == "" {
			t.Fatalf("%s.%s has no json tag; every wire field must tag explicitly", typ, field.Name)
		}
		tags = append(tags, tag)
	}
	return tags
}

func TestDescriptorSchemaTagParity(t *testing.T) {
	pairs := []struct {
		name   string
		server reflect.Type
		client reflect.Type
	}{
		{"descriptor", reflect.TypeOf(relay.Descriptor{}), reflect.TypeOf(brokerapi.RelayDescriptor{})},
		{"list envelope", reflect.TypeOf(relay.ListResponse{}), reflect.TypeOf(brokerapi.RelayListResponse{})},
		{"wss front", reflect.TypeOf(relay.WSSFrontDescriptor{}), reflect.TypeOf(brokerapi.RelayWSSFront{})},
	}
	for _, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			server := flattenedJSONTags(t, pair.server)
			client := flattenedJSONTags(t, pair.client)
			if !reflect.DeepEqual(server, client) {
				t.Fatalf("JSON tag sequences drifted:\n  server %s: %q\n  client %s: %q",
					pair.server, server, pair.client, client)
			}
		})
	}
}

// parityDescriptorPair returns one fully-populated descriptor in both models,
// with every wire field non-zero (so a value mismatch cannot hide behind
// omitempty) and the server-only fields set to prove they never serialize.
func parityDescriptorPair() (relay.Descriptor, brokerapi.RelayDescriptor) {
	geo := brokerapi.RelayGeoLocation{
		City: "Seoul", Country: "South Korea", CountryCode: "KR",
		Latitude: 37.5665, Longitude: 126.978,
	}
	registered := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	heartbeat := registered.Add(30 * time.Second)
	expires := registered.Add(2 * time.Minute)
	front := brokerapi.RelayWSSFront{ID: "front-1", URL: "wss://cdn.example/tunnel", ProtocolVersion: 1}

	server := relay.Descriptor{
		ID: "relay-1", Label: "seoul-1", PublicHost: "203.0.113.5", PublicPort: 443,
		GeoLocation: geo,
		// Server-only, json:"-": must never reach the wire.
		ExitHost: "198.51.100.7", IdentityPublicKey: "identity-key", LeaseToken: "lease-token",
		NodeClass: "foundation", Protocol: "vless", ClientID: "client-uuid",
		RealityPublicKey: "reality-pub", ShortID: "abcd1234", ServerName: "www.example.com",
		Flow: "xtls-rprx-vision", ExitMode: "direct", MaxSessions: 64, MaxMbps: 400,
		RelayVersion: "1.2.3", Transport: "direct", PunchCapable: true,
		PunchEndpoint: "https://203.0.113.5:9444",
		WSSFronts:     []relay.WSSFrontDescriptor{relay.WSSFrontDescriptor(front)},
		RegisteredAt:  registered, LastHeartbeatAt: heartbeat, ExpiresAt: expires,
	}
	client := brokerapi.RelayDescriptor{
		ID: "relay-1", Label: "seoul-1", PublicHost: "203.0.113.5", PublicPort: 443,
		RelayGeoLocation: geo,
		NodeClass:        "foundation", Protocol: "vless", ClientID: "client-uuid",
		RealityPublicKey: "reality-pub", ShortID: "abcd1234", ServerName: "www.example.com",
		Flow: "xtls-rprx-vision", ExitMode: "direct", MaxSessions: 64, MaxMbps: 400,
		RelayVersion: "1.2.3", Transport: "direct", PunchCapable: true,
		PunchEndpoint: "https://203.0.113.5:9444",
		WSSFronts:     []brokerapi.RelayWSSFront{front},
		RegisteredAt:  registered, LastHeartbeatAt: heartbeat, ExpiresAt: expires,
	}
	return server, client
}

func TestDescriptorSchemaMarshalParity(t *testing.T) {
	server, client := parityDescriptorPair()

	t.Run("populated descriptor", func(t *testing.T) {
		serverJSON, err := json.Marshal(server)
		if err != nil {
			t.Fatalf("marshal server descriptor: %v", err)
		}
		clientJSON, err := json.Marshal(client)
		if err != nil {
			t.Fatalf("marshal client descriptor: %v", err)
		}
		if !bytes.Equal(serverJSON, clientJSON) {
			t.Fatalf("serialized shapes drifted:\n  server: %s\n  client: %s", serverJSON, clientJSON)
		}
		if bytes.Contains(serverJSON, []byte("lease-token")) ||
			bytes.Contains(serverJSON, []byte("identity-key")) ||
			bytes.Contains(serverJSON, []byte("198.51.100.7")) {
			t.Fatalf("server-only field leaked into the wire shape: %s", serverJSON)
		}
	})

	// Zero values exercise the omitempty set: with every optional field
	// absent, an omitempty added or dropped on one side becomes visible.
	t.Run("zero descriptor", func(t *testing.T) {
		serverJSON, err := json.Marshal(relay.Descriptor{})
		if err != nil {
			t.Fatalf("marshal zero server descriptor: %v", err)
		}
		clientJSON, err := json.Marshal(brokerapi.RelayDescriptor{})
		if err != nil {
			t.Fatalf("marshal zero client descriptor: %v", err)
		}
		if !bytes.Equal(serverJSON, clientJSON) {
			t.Fatalf("zero-value shapes drifted:\n  server: %s\n  client: %s", serverJSON, clientJSON)
		}
	})

	t.Run("list envelope", func(t *testing.T) {
		now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
		serverJSON, err := json.Marshal(relay.ListResponse{
			Count: 1, ServerTime: now, NotAfter: now.Add(30 * time.Minute),
			KeyID: "0011223344556677", Channel: relay.ChannelAPI, Limit: 5,
			Relays: []relay.Descriptor{server},
		})
		if err != nil {
			t.Fatalf("marshal server list: %v", err)
		}
		clientJSON, err := json.Marshal(brokerapi.RelayListResponse{
			Count: 1, ServerTime: now, NotAfter: now.Add(30 * time.Minute),
			KeyID: "0011223344556677", Channel: brokerapi.ChannelAPI, Limit: 5,
			Relays: []brokerapi.RelayDescriptor{client},
		})
		if err != nil {
			t.Fatalf("marshal client list: %v", err)
		}
		if !bytes.Equal(serverJSON, clientJSON) {
			t.Fatalf("signed envelope shapes drifted:\n  server: %s\n  client: %s", serverJSON, clientJSON)
		}
	})
}

// TestDescriptorSchemaDecodeParity pins that both models read the same bytes
// the same way, including the deprecated volunteer_version alias preference.
func TestDescriptorSchemaDecodeParity(t *testing.T) {
	bodies := map[string]string{
		"volunteer_version only":        `{"id":"relay-1","public_host":"203.0.113.5","public_port":443,"node_class":"volunteer","volunteer_version":"0.9.0"}`,
		"both keys, relay_version wins": `{"id":"relay-1","public_host":"203.0.113.5","public_port":443,"node_class":"volunteer","relay_version":"1.2.3","volunteer_version":"0.9.0"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var server relay.Descriptor
			if err := json.Unmarshal([]byte(body), &server); err != nil {
				t.Fatalf("decode server descriptor: %v", err)
			}
			var client brokerapi.RelayDescriptor
			if err := json.Unmarshal([]byte(body), &client); err != nil {
				t.Fatalf("decode client descriptor: %v", err)
			}
			serverJSON, err := json.Marshal(server)
			if err != nil {
				t.Fatalf("re-marshal server descriptor: %v", err)
			}
			clientJSON, err := json.Marshal(client)
			if err != nil {
				t.Fatalf("re-marshal client descriptor: %v", err)
			}
			if !bytes.Equal(serverJSON, clientJSON) {
				t.Fatalf("decoded models diverged:\n  server: %s\n  client: %s", serverJSON, clientJSON)
			}
			if server.RelayVersion != client.RelayVersion {
				t.Fatalf("version alias preference drifted: server %q, client %q",
					server.RelayVersion, client.RelayVersion)
			}
		})
	}
}
