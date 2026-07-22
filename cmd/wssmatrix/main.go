// Command wssmatrix runs the destructive-free direct-path and
// pre-advertisement acceptance checks for one relay-local WSS front. It signs
// only short-lived tickets with an explicitly supplied temporary test key;
// that key must be removed from the sidecar before the front is advertised.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/openrung/openrung/wsscore"

	"openrung/internal/client"
	"openrung/internal/relay"
	"openrung/internal/wssbridge"
)

const (
	testViewerAddress = "198.51.100.77:443"
	ticketLifetime    = 2 * time.Minute
)

type config struct {
	mode                string
	url                 string
	relayID             string
	frontID             string
	seedFile            string
	descriptorFile      string
	singBox             string
	probeURL            string
	originTokenFile     string
	originTokenNextFile string
	ticketResponseFile  string
	sourceLimit         int
	expectCloseWithin   time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "WSS matrix failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "keygen" {
		return keygen(args[1:])
	}
	if len(args) > 0 && args[0] == "keyring" {
		return keyring(args[1:])
	}
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	var signer *wssbridge.TicketSigner
	if cfg.mode != "issued" && cfg.mode != "direct" {
		signer, err = signerFromSeedFile(cfg.seedFile)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	switch cfg.mode {
	case "direct":
		err = runDirect(ctx, cfg)
	case "edge":
		err = runEdge(ctx, cfg, signer)
	case "origin":
		err = runOrigin(ctx, cfg, signer)
	case "revoked":
		err = runRevoked(ctx, cfg, signer)
	case "issued":
		err = runIssued(ctx, cfg)
	default:
		err = errors.New("unknown matrix mode")
	}
	if err != nil {
		return err
	}
	fmt.Printf("mode=%s checks=ok\n", cfg.mode)
	return nil
}

type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }
func (p *pathList) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("wssmatrix", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.mode, "mode", "edge", "matrix mode: direct, edge, origin, revoked, or issued")
	fs.StringVar(&cfg.url, "url", "", "exact WSS bridge URL")
	fs.StringVar(&cfg.relayID, "relay-id", "", "exact local relay ID")
	fs.StringVar(&cfg.frontID, "front-id", "", "exact front ID")
	fs.StringVar(&cfg.seedFile, "seed-file", "", "mode-0600 base64 Ed25519 test seed")
	fs.StringVar(&cfg.descriptorFile, "descriptor-file", "", "public relay descriptor JSON")
	fs.StringVar(&cfg.singBox, "sing-box", "sing-box", "sing-box binary")
	fs.StringVar(&cfg.probeURL, "probe-url", "https://www.cloudflare.com/cdn-cgi/trace", "end-to-end probe URL")
	fs.StringVar(&cfg.originTokenFile, "origin-token-file", "", "mode-0600 current origin token")
	fs.StringVar(&cfg.originTokenNextFile, "origin-token-next-file", "", "mode-0600 next origin token")
	fs.StringVar(&cfg.ticketResponseFile, "ticket-response-file", "", "mode-0600 broker ticket response")
	fs.IntVar(&cfg.sourceLimit, "source-limit", 0, "configured per-source session limit to verify")
	fs.DurationVar(&cfg.expectCloseWithin, "expect-close-within", 0, "maximum expected idle/lifetime cleanup delay")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return config{}, errors.New("invalid WSS matrix arguments")
	}
	if cfg.mode != "direct" && cfg.mode != "edge" && cfg.mode != "origin" && cfg.mode != "revoked" && cfg.mode != "issued" {
		return config{}, errors.New("mode must be direct, edge, origin, revoked, or issued")
	}
	if cfg.relayID == "" {
		return config{}, errors.New("relay-id is required")
	}
	if cfg.mode == "direct" {
		if cfg.descriptorFile == "" || cfg.url != "" || cfg.frontID != "" || cfg.seedFile != "" || cfg.sourceLimit != 0 || cfg.expectCloseWithin != 0 || cfg.originTokenFile != "" || cfg.originTokenNextFile != "" || cfg.ticketResponseFile != "" {
			return config{}, errors.New("direct mode requires only a relay id and descriptor file")
		}
		return cfg, nil
	}
	if cfg.url == "" || cfg.frontID == "" {
		return config{}, errors.New("url, relay-id, and front-id are required")
	}
	if cfg.mode != "issued" && cfg.seedFile == "" {
		return config{}, errors.New("seed-file is required")
	}
	parsed, err := url.Parse(cfg.url)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != wssbridge.BridgePath {
		return config{}, errors.New("url must be an exact bridge URL without query or fragment")
	}
	if cfg.mode == "edge" {
		if parsed.Scheme != "wss" || cfg.descriptorFile == "" || cfg.sourceLimit != 0 || cfg.expectCloseWithin != 0 || cfg.originTokenFile != "" || cfg.originTokenNextFile != "" || cfg.ticketResponseFile != "" {
			return config{}, errors.New("edge mode requires wss and a descriptor file only")
		}
	} else if cfg.mode == "revoked" {
		if parsed.Scheme != "wss" || cfg.descriptorFile != "" || cfg.sourceLimit != 0 || cfg.expectCloseWithin != 0 || cfg.originTokenFile != "" || cfg.originTokenNextFile != "" || cfg.ticketResponseFile != "" {
			return config{}, errors.New("revoked mode requires only a wss endpoint and test signer")
		}
	} else if cfg.mode == "issued" {
		if parsed.Scheme != "wss" || cfg.ticketResponseFile == "" || cfg.seedFile != "" || cfg.sourceLimit != 0 || cfg.expectCloseWithin != 0 || cfg.originTokenFile != "" || cfg.originTokenNextFile != "" {
			return config{}, errors.New("issued mode requires a wss endpoint and ticket response file")
		}
	} else {
		host := net.ParseIP(parsed.Hostname())
		if parsed.Scheme != "ws" || host == nil || !host.IsLoopback() || cfg.originTokenFile == "" || cfg.originTokenNextFile == "" || cfg.sourceLimit < 1 || cfg.descriptorFile != "" || cfg.ticketResponseFile != "" {
			return config{}, errors.New("origin mode requires a loopback ws URL, two token files, and a positive source limit")
		}
	}
	return cfg, nil
}

func runDirect(ctx context.Context, cfg config) error {
	descriptor, err := loadRelayDescriptor(cfg.descriptorFile, cfg.relayID)
	if err != nil {
		return err
	}
	if err := runRealityProbe(ctx, cfg, descriptor, "", 0); err != nil {
		return err
	}
	fmt.Println("direct_reality_probe=ok")
	return nil
}

func runIssued(ctx context.Context, cfg config) error {
	info, err := os.Lstat(cfg.ticketResponseFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 64<<10 {
		return errors.New("ticket response must be a bounded mode-0600 regular file")
	}
	raw, err := os.ReadFile(cfg.ticketResponseFile)
	if err != nil {
		return errors.New("read ticket response")
	}
	var response relay.WSSSessionTicketResponse
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return errors.New("decode ticket response")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("ticket response must contain one JSON object")
	}
	if response.URL != cfg.url || len(response.Ticket) < 1 || len(response.Ticket) > wssbridge.MaxTicketBytes {
		return errors.New("ticket response does not match the expected front")
	}
	remaining := time.Until(response.ExpiresAt)
	if remaining <= 0 || remaining > wssbridge.MaxTicketLifetime {
		return errors.New("ticket response expiry is outside protocol bounds")
	}
	if cfg.descriptorFile != "" {
		if err := endToEndProbeTicket(ctx, cfg, response.Ticket); err != nil {
			return err
		}
		fmt.Println("broker_ticket_issuance=ok production_reality_probe=ok")
		return nil
	}
	bridge, err := wsscore.DialClient(ctx, wsscore.ClientOptions{URL: cfg.url, Ticket: response.Ticket})
	if err != nil {
		return errors.New("issued production ticket was rejected")
	}
	_ = bridge.Close()
	fmt.Println("broker_ticket_issuance=ok production_sidecar_acceptance=ok")
	return nil
}

func runRevoked(ctx context.Context, cfg config, signer *wssbridge.TicketSigner) error {
	ticket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	if status, err := dialStatus(ctx, cfg.url, ticket, "", ""); err == nil || status != http.StatusUnauthorized {
		return fmt.Errorf("removed test ticket key returned HTTP %d", status)
	}
	fmt.Println("test_ticket_key_removal=ok")
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("wssmatrix keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	seedFile := fs.String("seed-file", "", "new seed output")
	publicFile := fs.String("public-key-file", "", "new public-key output")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *seedFile == "" || *publicFile == "" {
		return errors.New("keygen requires seed-file and public-key-file")
	}
	for _, path := range []string{*seedFile, *publicFile} {
		if !filepath.IsAbs(path) {
			return errors.New("key output paths must be absolute")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("refusing to replace a key output file")
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate test ticket key")
	}
	if err := writeSecretFile(*seedFile, base64.StdEncoding.EncodeToString(private.Seed())+"\n"); err != nil {
		return err
	}
	if err := writeSecretFile(*publicFile, base64.StdEncoding.EncodeToString(public)+"\n"); err != nil {
		_ = os.Remove(*seedFile)
		return err
	}
	fmt.Println("test_ticket_key=generated")
	return nil
}

func keyring(args []string) error {
	fs := flag.NewFlagSet("wssmatrix keyring", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	output := fs.String("output", "", "new key-ring output")
	var inputs pathList
	fs.Var(&inputs, "public-key-file", "mode-0600 public key input; repeat for overlap")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *output == "" || len(inputs) < 1 || len(inputs) > 8 {
		return errors.New("keyring requires output and 1..8 public-key-file inputs")
	}
	if !filepath.IsAbs(*output) {
		return errors.New("key-ring output path must be absolute")
	}
	if _, err := os.Lstat(*output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("refusing to replace a key-ring output file")
	}
	keys := make([]string, 0, len(inputs))
	seen := make(map[string]bool)
	for _, input := range inputs {
		value, err := readMode0600Line(input)
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(raw) != ed25519.PublicKeySize || seen[value] {
			return errors.New("public key input is invalid or duplicated")
		}
		seen[value] = true
		keys = append(keys, value)
	}
	if err := writeSecretFile(*output, strings.Join(keys, ",")+"\n"); err != nil {
		return err
	}
	fmt.Printf("test_ticket_keyring=generated keys=%d\n", len(keys))
	return nil
}

func writeSecretFile(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create key output")
	}
	if _, err = io.WriteString(file, value); err != nil {
		_ = file.Close()
		return errors.New("write key output")
	}
	if err := file.Close(); err != nil {
		return errors.New("close key output")
	}
	return nil
}

func signerFromSeedFile(path string) (*wssbridge.TicketSigner, error) {
	raw, err := readMode0600Line(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("test ticket seed is invalid")
	}
	return wssbridge.NewTicketSigner(ed25519.NewKeyFromSeed(seed), wssbridge.TicketOptions{})
}

func readMode0600Line(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", errors.New("secret input must be a mode-0600 regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("read secret input")
	}
	value := strings.TrimSuffix(string(raw), "\n")
	if value == "" || strings.ContainsAny(value, "\r\n\t ") {
		return "", errors.New("secret input must contain one non-whitespace line")
	}
	return value, nil
}

func signedTicket(signer *wssbridge.TicketSigner, relayID, frontID string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("generate ticket identifier")
	}
	now := time.Now().UTC().Truncate(time.Second)
	return signer.Sign(wssbridge.Claims{
		Version: wssbridge.TicketVersion, Audience: wssbridge.TicketAudience,
		JTI: hex.EncodeToString(random[:]), RelayID: relayID, FrontID: frontID,
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(ticketLifetime).Unix(), MaxStreams: 8,
	})
}

func runEdge(ctx context.Context, cfg config, signer *wssbridge.TicketSigner) error {
	if status, err := dialStatus(ctx, cfg.url, "", "", ""); err == nil || status != http.StatusUnauthorized {
		return errors.New("missing ticket was not rejected")
	}
	wrongRelay, err := signedTicket(signer, cfg.relayID+"_wrong", cfg.frontID)
	if err != nil {
		return err
	}
	if status, err := dialStatus(ctx, cfg.url, wrongRelay, "", ""); err == nil || status != http.StatusUnauthorized {
		return errors.New("wrong-relay ticket was not rejected")
	}
	wrongFront, err := signedTicket(signer, cfg.relayID, cfg.frontID+"-wrong")
	if err != nil {
		return err
	}
	if status, err := dialStatus(ctx, cfg.url, wrongFront, "", ""); err == nil || status != http.StatusUnauthorized {
		return errors.New("wrong-front ticket was not rejected")
	}
	replay, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	conn, status, err := dialWebSocket(ctx, cfg.url, replay, "", "")
	if err != nil || status != http.StatusSwitchingProtocols {
		return errors.New("valid replay probe ticket was rejected")
	}
	_ = conn.Close()
	if status, err = dialStatus(ctx, cfg.url, replay, "", ""); err == nil || status != http.StatusConflict {
		return errors.New("ticket replay was not rejected")
	}
	if err := endToEndProbe(ctx, cfg, signer); err != nil {
		return err
	}
	fmt.Println("edge_authorization=ok edge_binding=ok replay=ok reality_probe=ok")
	return nil
}

func runOrigin(ctx context.Context, cfg config, signer *wssbridge.TicketSigner) error {
	current, err := readMode0600Line(cfg.originTokenFile)
	if err != nil {
		return err
	}
	next, err := readMode0600Line(cfg.originTokenNextFile)
	if err != nil {
		return err
	}
	if current == next {
		return errors.New("origin rotation tokens are not unique")
	}
	badTicket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	if status, err := dialStatus(ctx, cfg.url, badTicket, strings.Repeat("x", 48), testViewerAddress); err == nil || status != http.StatusUnauthorized {
		return fmt.Errorf("invalid origin token rejection returned HTTP %d (%v)", status, err)
	}
	for _, token := range []string{current, next} {
		ticket, signErr := signedTicket(signer, cfg.relayID, cfg.frontID)
		if signErr != nil {
			return signErr
		}
		conn, status, dialErr := dialWebSocket(ctx, cfg.url, ticket, token, testViewerAddress)
		if dialErr != nil || status != http.StatusSwitchingProtocols {
			return errors.New("overlapping origin token was rejected")
		}
		_ = conn.Close()
	}
	if err := sourceLimitProbe(ctx, cfg, signer, current); err != nil {
		return err
	}
	if cfg.expectCloseWithin > 0 {
		if err := lifecycleProbe(ctx, cfg, signer, current); err != nil {
			return err
		}
	}
	fmt.Println("origin_auth=ok origin_rotation=ok source_limit=ok lifecycle_cleanup=ok")
	return nil
}

func dialStatus(ctx context.Context, endpoint, ticket, originToken, viewerAddress string) (int, error) {
	conn, status, err := dialWebSocket(ctx, endpoint, ticket, originToken, viewerAddress)
	if conn != nil {
		_ = conn.Close()
	}
	return status, err
}

func dialWebSocket(ctx context.Context, endpoint, ticket, originToken, viewerAddress string) (*websocket.Conn, int, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second, EnableCompression: false,
		Subprotocols: []string{wssbridge.Subprotocol}, Proxy: nil,
		NetDialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
	header := make(http.Header)
	if ticket != "" {
		header.Set("Authorization", "Bearer "+ticket)
	}
	if originToken != "" {
		header.Set(wssbridge.OriginTokenHeader, originToken)
	}
	if viewerAddress != "" {
		header.Set(wssbridge.DefaultViewerAddressHeader, viewerAddress)
	}
	conn, response, err := dialer.DialContext(ctx, endpoint, header)
	status := 0
	if response != nil {
		status = response.StatusCode
		if response.Body != nil {
			_ = response.Body.Close()
		}
	}
	if err != nil {
		return nil, status, fmt.Errorf("WebSocket handshake failed: %w", err)
	}
	if conn.Subprotocol() != wssbridge.Subprotocol {
		_ = conn.Close()
		return nil, status, errors.New("required subprotocol was not negotiated")
	}
	return conn, status, nil
}

func sourceLimitProbe(ctx context.Context, cfg config, signer *wssbridge.TicketSigner, token string) error {
	connections := make([]*websocket.Conn, 0, cfg.sourceLimit)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for range cfg.sourceLimit {
		ticket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
		if err != nil {
			return err
		}
		conn, status, err := dialWebSocket(ctx, cfg.url, ticket, token, testViewerAddress)
		if err != nil || status != http.StatusSwitchingProtocols {
			return errors.New("configured per-source capacity was rejected early")
		}
		connections = append(connections, conn)
	}
	ticket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	if status, err := dialStatus(ctx, cfg.url, ticket, token, testViewerAddress); err == nil || status != http.StatusTooManyRequests {
		return errors.New("per-source session limit was not enforced")
	}
	_ = connections[0].Close()
	connections = connections[1:]
	time.Sleep(250 * time.Millisecond)
	ticket, err = signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	conn, status, err := dialWebSocket(ctx, cfg.url, ticket, token, testViewerAddress)
	if err != nil || status != http.StatusSwitchingProtocols {
		return errors.New("per-source capacity was not released after cleanup")
	}
	connections = append(connections, conn)
	return nil
}

func lifecycleProbe(ctx context.Context, cfg config, signer *wssbridge.TicketSigner, token string) error {
	ticket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	conn, status, err := dialWebSocket(ctx, cfg.url, ticket, token, testViewerAddress)
	if err != nil || status != http.StatusSwitchingProtocols {
		return errors.New("lifecycle probe session was rejected")
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(cfg.expectCloseWithin))
	for {
		if _, _, err = conn.ReadMessage(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return errors.New("session exceeded configured cleanup deadline")
			}
			return nil
		}
	}
}

func endToEndProbe(ctx context.Context, cfg config, signer *wssbridge.TicketSigner) error {
	ticket, err := signedTicket(signer, cfg.relayID, cfg.frontID)
	if err != nil {
		return err
	}
	return endToEndProbeTicket(ctx, cfg, ticket)
}

func endToEndProbeTicket(ctx context.Context, cfg config, ticket string) error {
	descriptor, err := loadRelayDescriptor(cfg.descriptorFile, cfg.relayID)
	if err != nil {
		return err
	}
	bridge, err := wsscore.DialClient(ctx, wsscore.ClientOptions{URL: cfg.url, Ticket: ticket})
	if err != nil {
		return errors.New("start WSS bridge client")
	}
	defer bridge.Close()
	bridgeCtx, cancelBridge := context.WithCancel(ctx)
	defer cancelBridge()
	serveDone := make(chan error, 1)
	go func() { serveDone <- bridge.Serve(bridgeCtx) }()

	bridgeHost, bridgePort := bridge.Endpoint()
	if err := runRealityProbe(ctx, cfg, descriptor, bridgeHost, bridgePort); err != nil {
		return err
	}
	cancelBridge()
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		return errors.New("WSS bridge cleanup timed out")
	}
	return nil
}

func loadRelayDescriptor(path, relayID string) (relay.Descriptor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return relay.Descriptor{}, errors.New("read public relay descriptor")
	}
	var descriptor relay.Descriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		return relay.Descriptor{}, errors.New("decode public relay descriptor")
	}
	if descriptor.ID != relayID {
		var list struct {
			Relays []relay.Descriptor `json:"relays"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return relay.Descriptor{}, errors.New("decode public relay list")
		}
		descriptor = relay.Descriptor{}
		for _, candidate := range list.Relays {
			if candidate.ID == relayID {
				descriptor = candidate
				break
			}
		}
	}
	if descriptor.ID != relayID {
		return relay.Descriptor{}, errors.New("public relay descriptor does not match matrix relay")
	}
	return descriptor, nil
}

func runRealityProbe(ctx context.Context, cfg config, descriptor relay.Descriptor, bridgeHost string, bridgePort int) error {
	proxyPort, err := availableLoopbackPort()
	if err != nil {
		return err
	}
	configJSON, err := client.BuildSingBoxConfig(client.SingBoxConfigInput{
		Relay: descriptor, Mode: client.ModeProxy,
		ProxyListenAddress: "127.0.0.1", ProxyListenPort: proxyPort,
		BridgeHost: bridgeHost, BridgePort: bridgePort,
	})
	if err != nil {
		return errors.New("build sing-box Reality bridge config")
	}
	configFile, err := os.CreateTemp("", "openrung-wss-matrix-*.json")
	if err != nil {
		return errors.New("create temporary sing-box config")
	}
	configPath := configFile.Name()
	defer os.Remove(configPath)
	if err := configFile.Chmod(0o600); err != nil {
		_ = configFile.Close()
		return errors.New("protect temporary sing-box config")
	}
	if _, err := configFile.Write(configJSON); err != nil {
		_ = configFile.Close()
		return errors.New("write temporary sing-box config")
	}
	if err := configFile.Close(); err != nil {
		return errors.New("close temporary sing-box config")
	}

	singCtx, cancelSing := context.WithCancel(ctx)
	defer cancelSing()
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- (client.SingBoxRunner{Path: cfg.singBox, KillGrace: time.Second}).Run(singCtx, configPath)
	}()
	if err := waitForListener(ctx, net.JoinHostPort("127.0.0.1", fmt.Sprint(proxyPort)), runnerDone); err != nil {
		return err
	}
	proxy, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", fmt.Sprint(proxyPort)))
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	requestCtx, cancelRequest := context.WithTimeout(ctx, 20*time.Second)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, cfg.probeURL, nil)
	if err != nil {
		return errors.New("construct end-to-end probe")
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return errors.New("end-to-end Reality probe failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return errors.New("end-to-end Reality probe returned an unsuccessful status")
	}
	cancelSing()
	select {
	case <-runnerDone:
	case <-time.After(5 * time.Second):
		return errors.New("sing-box cleanup timed out")
	}
	return nil
}

func availableLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("reserve local proxy port")
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForListener(ctx context.Context, address string, runnerDone <-chan error) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("matrix timed out before sing-box was ready")
		case <-deadline.C:
			return errors.New("sing-box proxy did not become ready")
		case <-runnerDone:
			return errors.New("sing-box exited before its proxy became ready")
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
}
