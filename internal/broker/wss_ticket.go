package broker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openrung/internal/relay"
	"openrung/internal/wssbridge"
)

const (
	defaultWSSTicketTTL      = 2 * time.Minute
	defaultWSSTicketStreams  = 64
	maxWSSTicketRequestBytes = 8 << 10
)

type wssTicketIssuer struct {
	signer     *wssbridge.TicketSigner
	ttl        time.Duration
	maxStreams int
	now        func() time.Time
}

func newWSSTicketIssuer(cfg Config) *wssTicketIssuer {
	if len(cfg.WSSTicketSigningSeed) == 0 {
		return nil
	}
	if len(cfg.WSSTicketSigningSeed) != ed25519.SeedSize {
		panic(fmt.Sprintf("broker: WSS ticket signing seed must be %d bytes", ed25519.SeedSize))
	}
	ttl := cfg.WSSTicketTTL
	if ttl == 0 {
		ttl = defaultWSSTicketTTL
	}
	if ttl < 2*time.Second || ttl > wssbridge.MaxTicketLifetime {
		panic(fmt.Sprintf("broker: WSS ticket TTL must be within [2s, %s]", wssbridge.MaxTicketLifetime))
	}
	maxStreams := cfg.WSSTicketMaxStreams
	if maxStreams == 0 {
		maxStreams = defaultWSSTicketStreams
	}
	if maxStreams < 1 || maxStreams > wssbridge.MaxTicketStreams {
		panic(fmt.Sprintf("broker: WSS ticket max streams must be within [1, %d]", wssbridge.MaxTicketStreams))
	}
	issuer := &wssTicketIssuer{ttl: ttl, maxStreams: maxStreams, now: time.Now}
	signer, err := wssbridge.NewTicketSigner(
		ed25519.NewKeyFromSeed(append([]byte(nil), cfg.WSSTicketSigningSeed...)),
		wssbridge.TicketOptions{
			MaxLifetime: ttl,
			// Keep the signing clock identical to the issuer clock. Besides making
			// issuance internally consistent, this gives deterministic boundary
			// tests without weakening the production time source.
			Now: func() time.Time { return issuer.now() },
		},
	)
	if err != nil {
		panic("broker: initialize WSS ticket signer: " + err.Error())
	}
	issuer.signer = signer
	return issuer
}

func wssTicketHandler(store RelayStore, issuer *wssTicketIssuer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		var request relay.WSSSessionTicketRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxWSSTicketRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid WSS ticket JSON")
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			writeError(w, http.StatusBadRequest, "WSS ticket request must contain one JSON object")
			return
		}
		request.RelayID = strings.TrimSpace(request.RelayID)
		request.FrontID = strings.TrimSpace(request.FrontID)
		if request.RelayID == "" || request.FrontID == "" {
			writeError(w, http.StatusBadRequest, "relay_id and front_id are required")
			return
		}

		now := issuer.now().UTC().Truncate(time.Second)
		desc, err := store.RelayByID(request.RelayID, now)
		if errors.Is(err, ErrRelayNotFound) {
			writeError(w, http.StatusNotFound, "relay or WSS front not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "could not resolve WSS relay")
			return
		}
		if !wssRelayEligible(desc) {
			writeError(w, http.StatusNotFound, "relay or WSS front not found")
			return
		}
		front, ok := findWSSFront(desc.WSSFronts, request.FrontID)
		if !ok {
			writeError(w, http.StatusNotFound, "relay or WSS front not found")
			return
		}

		expiresAt := now.Add(issuer.ttl)
		if desc.ExpiresAt.Before(expiresAt) {
			expiresAt = desc.ExpiresAt.UTC().Truncate(time.Second)
		}
		if !expiresAt.After(now.Add(time.Second)) {
			writeError(w, http.StatusConflict, "relay lease is too close to expiry; refresh the directory")
			return
		}
		jti, err := newWSSTicketID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not issue WSS ticket")
			return
		}
		ticket, err := issuer.signer.Sign(wssbridge.Claims{
			Version: wssbridge.TicketVersion, Audience: wssbridge.TicketAudience,
			JTI: jti, RelayID: desc.ID, FrontID: front.ID,
			IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: expiresAt.Unix(),
			MaxStreams: issuer.maxStreams,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not issue WSS ticket")
			return
		}
		writeJSON(w, http.StatusCreated, relay.WSSSessionTicketResponse{
			Ticket: ticket, ExpiresAt: expiresAt, URL: front.URL,
		})
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else {
		return err
	}
}

func newWSSTicketID() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func wssRelayEligible(desc relay.Descriptor) bool {
	return desc.NodeClass == relay.NodeClassFoundation &&
		desc.Transport == relay.TransportDirect &&
		desc.ExitMode == relay.ExitModeDirect &&
		desc.PublicPort == 443 &&
		desc.IdentityPublicKey != "" && len(desc.WSSFronts) > 0
}

func findWSSFront(fronts []relay.WSSFrontDescriptor, id string) (relay.WSSFrontDescriptor, bool) {
	for _, front := range fronts {
		if front.ID == id && front.ProtocolVersion == relay.WSSProtocolVersion {
			return front, true
		}
	}
	return relay.WSSFrontDescriptor{}, false
}

func reserveWSSCandidate(relays []relay.Descriptor, limit int) []relay.Descriptor {
	if limit <= 0 || len(relays) <= limit {
		return relays
	}
	page := relays[:limit]
	for _, desc := range page {
		if wssRelayEligible(desc) {
			return page
		}
	}
	for _, desc := range relays[limit:] {
		if !wssRelayEligible(desc) {
			continue
		}
		out := append([]relay.Descriptor(nil), page...)
		out[len(out)-1] = desc
		return out
	}
	return page
}
