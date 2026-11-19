// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"openrung/internal/relay"
	"openrung/internal/wssbridge"
)

// The Reality-over-WebSocket (WSS) fallback needs a Foundation relay, a
// sidecar beside it and a CDN-style front in front of the sidecar:
//
//	client ─ wss://WSSFrontHost ─► internet stand-in (the "edge")
//	         ─ http RelayFAddr:wssOriginPort ─► relay-f-sidecar on 127.0.0.1
//	         ─ 127.0.0.1:443 ─► relay-f's Reality listener
//
// In production the edge is a CDN and the origin port is a TLS listener on
// the relay; neither ships in this repository, so the internet stand-in plays
// the CDN and the origin hop is plain HTTP inside the lab.
const (
	// FoundationToken lets the broker accept a foundation-class relay. It
	// must differ from VolunteerToken.
	FoundationToken = "lab-foundation-token"

	// FoundationRelay is the label, node and service of the WSS relay, and
	// WSSSidecar the service name of its sidecar.
	FoundationRelay = "relay-f"
	WSSSidecar      = "relay-f-sidecar"

	// WSSFrontID and WSSFrontHost name relay-f's one front. The host resolves
	// to the internet stand-in like every other name.
	WSSFrontID   = "edge-1"
	WSSFrontHost = "relay-f.cdn.example"
	WSSFrontURL  = "wss://" + WSSFrontHost + wssbridge.BridgePath

	// wssOriginPort is where the edge reaches relay-f's sidecar. The relay's
	// node translates it to the sidecar's loopback-only listener.
	wssOriginPort  = 8443
	wssSidecarAddr = "127.0.0.1:8081"
)

// wssOriginToken is the secret the edge adds to every request so the sidecar
// knows it came through the front.
var wssOriginToken = seed("wss origin token " + WSSFrontID)

// FoundationRelayService is a Foundation relay that advertises WSSFrontURL.
// A foundation relay refuses a plain-HTTP broker unless it is on loopback, so
// it reaches the broker through a loopback redirect in its node.
func FoundationRelayService() Service {
	return Service{Name: FoundationRelay, Node: FoundationRelay, Argv: []string{"entrypoint.sh"}, Env: append(relayEnv(FoundationRelay),
		"OPENRUNG_FOUNDATION_TOKEN="+FoundationToken,
		"OPENRUNG_MODE=direct",
		"OPENRUNG_BROKER_URL="+ClientBrokerURL,
		"OPENRUNG_PUBLIC_HOST="+RelayFAddr,
		"OPENRUNG_LISTEN_HOST=0.0.0.0",
		"OPENRUNG_HEARTBEAT_INTERVAL="+heartbeatInterval,
		"OPENRUNG_WSS_FRONTS="+WSSFrontID+"="+WSSFrontURL,
	)}
}

// WSSSidecarService is relay-f's WSS sidecar. It runs in the relay's node
// because it only ever dials the relay's Reality listener on loopback.
func WSSSidecarService() Service {
	return Service{Name: WSSSidecar, Node: FoundationRelay, Argv: []string{"wss-sidecar"}, Env: []string{
		"OPENRUNG_WSS_ADDR=" + wssSidecarAddr,
		"OPENRUNG_WSS_RELAY_ID=" + foundationRelayID(),
		"OPENRUNG_WSS_TICKET_PUBLIC_KEYS=" + wssTicketPublicKey(),
		"OPENRUNG_WSS_FRONT_ORIGIN_TOKENS=" + fmt.Sprintf(`{%q:[%q]}`, WSSFrontID, wssOriginToken),
		"OPENRUNG_WSS_REPLAY_STATE=" + StateDir(WSSSidecar) + "/replay.journal",
	}}
}

// StartWSSRelay starts relay-f and its sidecar. The caller waits for the
// broker to list relay-f.
func StartWSSRelay() error {
	if err := Start(FoundationRelayService()); err != nil {
		return err
	}
	return Start(WSSSidecarService())
}

// foundationRelayID is the ID the broker derives from relay-f's identity
// seed, which the sidecar must know up front.
func foundationRelayID() string {
	key, err := relay.ParseIdentitySeed(seed("relay identity " + FoundationRelay))
	if err != nil {
		panic(err)
	}
	return relay.DeriveRelayID(key.Public().(ed25519.PublicKey))
}

// wssTicketPublicKey is the broker's ticket verification key in the
// sidecar's "<key id>=<base64 key>" form.
func wssTicketPublicKey() string {
	raw, _ := base64.StdEncoding.DecodeString(wssTicketSeed)
	public := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	return wssbridge.TicketKeyID(public) + "=" + base64.StdEncoding.EncodeToString(public)
}

// wssTicketSeed signs the broker's WSS tickets.
var wssTicketSeed = seed("broker wss ticket signing key")

// buildWSSOrigin wires relay-f's node: a loopback redirect to the broker, and
// the origin port translated to the sidecar's loopback listener.
func buildWSSOrigin() error {
	if err := redirectLoopback(FoundationRelay, 8080, BrokerAddr+":8080"); err != nil {
		return err
	}
	return In(FoundationRelay, "iptables", "-t", "nat", "-A", "PREROUTING", "-i", "eth0", "-p", "tcp",
		"--dport", strconv.Itoa(wssOriginPort), "-j", "DNAT", "--to-destination", wssSidecarAddr)
}

// serveFront is the edge: for the front host it forwards the WebSocket
// upgrade to relay-f's origin and adds the two headers a CDN adds, the
// origin token and the viewer's address. Any other host gets the ordinary
// stand-in page.
func serveFront(fallback http.HandlerFunc) http.HandlerFunc {
	origin := &url.URL{Scheme: "http", Host: RelayFAddr + ":" + strconv.Itoa(wssOriginPort)}
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(origin)
		r.Out.Header.Set(wssbridge.OriginTokenHeader, wssOriginToken)
		r.Out.Header.Set(wssbridge.DefaultViewerAddressHeader, r.In.RemoteAddr)
	}}
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, WSSFrontHost) {
			fallback(w, r)
			return
		}
		fmt.Printf("lab edge: %s%s -> %s\n", r.Host, r.URL.Path, origin.Host)
		proxy.ServeHTTP(w, r)
	}
}
