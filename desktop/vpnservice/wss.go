package vpnservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/openrung/openrung/wsscore"

	"openrung/desktop/config"
	"openrung/desktop/proxyconfig"
	"openrung/internal/client"
	"openrung/internal/clienttelemetry"
	"openrung/internal/relay"
)

const (
	accessTransportWSS    = "wss"
	wssTicketHTTPTimeout  = 15 * time.Second
	wssTicketAttemptLimit = 5 * time.Second
	wssTicketDefaultRetry = 10 * time.Second
	wssTicketMaxRetry     = 30 * time.Second
)

type wssBridge interface {
	Endpoint() (host string, port int)
	Serve(context.Context) error
	Close() error
}

// directPathError is the sole authority to unlock WSS fallback. It marks only
// a raw relay TCP failure or a failed end-to-end probe after sing-box became
// ready; local configuration/process/readiness failures never receive it.
type directPathError struct {
	stage string
	err   error
}

func (e *directPathError) Error() string { return e.err.Error() }
func (e *directPathError) Unwrap() error { return e.err }

func markDirectPathError(stage string, err error) error {
	if err == nil {
		err = errors.New("direct relay path failed")
	}
	return &directPathError{stage: stage, err: err}
}

func directPathErrorStage(err error) (string, bool) {
	var pathErr *directPathError
	if !errors.As(err, &pathErr) {
		return "", false
	}
	return pathErr.stage, true
}

// wssTransportError keeps ticket, CDN, handshake, and WSS-session failures out
// of relay health. frontID is a signed, non-secret operational dimension.
type wssTransportError struct {
	stage   string
	frontID string
	err     error
}

func (e *wssTransportError) Error() string { return e.err.Error() }
func (e *wssTransportError) Unwrap() error { return e.err }

func markWSSTransportError(stage, frontID string, err error) error {
	if err == nil {
		err = errors.New("WSS access transport failed")
	}
	return &wssTransportError{stage: stage, frontID: frontID, err: err}
}

func wssTransportStage(err error) (string, bool) {
	stage, _, ok := wssTransportMetadata(err)
	return stage, ok
}

func wssTransportMetadata(err error) (stage, frontID string, ok bool) {
	var transportErr *wssTransportError
	if !errors.As(err, &transportErr) {
		return "", "", false
	}
	return transportErr.stage, transportErr.frontID, true
}

// relayFailureRecordedError prevents the outer ladder from recording the same
// direct failure again after the relay's WSS fronts have also been attempted.
type relayFailureRecordedError struct{ err error }

func (e *relayFailureRecordedError) Error() string { return e.err.Error() }
func (e *relayFailureRecordedError) Unwrap() error { return e.err }

func markRelayFailureRecorded(err error) error { return &relayFailureRecordedError{err: err} }

func relayFailureAlreadyRecorded(err error) bool {
	var recorded *relayFailureRecordedError
	return errors.As(err, &recorded)
}

// supportedWSSFronts accepts only canonical fronts on a direct-mode Foundation
// relay at public port 443. It returns the signed per-relay entries verbatim;
// there is no shared URL or client-selected destination.
func supportedWSSFronts(candidate relay.Descriptor) []relay.WSSFrontDescriptor {
	transport := strings.ToLower(strings.TrimSpace(candidate.Transport))
	if transport == "" {
		transport = relay.TransportDirect
	}
	if transport != relay.TransportDirect ||
		candidate.NodeClass != relay.NodeClassFoundation ||
		candidate.ExitMode != relay.ExitModeDirect ||
		candidate.PublicPort != 443 {
		return nil
	}
	normalized, err := relay.NormalizeWSSFronts(candidate.WSSFronts)
	if err != nil || !slices.Equal(normalized, candidate.WSSFronts) {
		return nil
	}
	return normalized
}

func (s *Service) wssTicketRequester() func(context.Context, string, relay.WSSSessionTicketRequest, string, string) (relay.WSSSessionTicketResponse, error) {
	if s.requestWSSTicket != nil {
		return s.requestWSSTicket
	}
	return func(ctx context.Context, brokerURL string, request relay.WSSSessionTicketRequest, clientID, sessionID string) (relay.WSSSessionTicketResponse, error) {
		brokerClient := client.BrokerClient{
			BaseURL: brokerURL,
			HTTPClient: &http.Client{
				Timeout: wssTicketHTTPTimeout,
			},
		}
		return brokerClient.RequestWSSSessionTicket(ctx, request, clientID, sessionID)
	}
}

func (s *Service) wssDialer() func(context.Context, string, string) (wssBridge, error) {
	if s.dialWSS != nil {
		return s.dialWSS
	}
	return func(ctx context.Context, rawURL, ticket string) (wssBridge, error) {
		return wsscore.DialClient(ctx, wsscore.ClientOptions{
			URL: rawURL, Ticket: ticket, CloudFrontNoSNI: true,
		})
	}
}

// wssTicketBrokerFronts places the broker front that served this session's
// signed directory first, then adds every independent configured default.
func wssTicketBrokerFronts(primary string) []string {
	fronts := make([]string, 0, len(config.DefaultBrokerURLs)+1)
	seen := make(map[string]struct{}, len(config.DefaultBrokerURLs)+1)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, duplicate := seen[value]; duplicate {
			return
		}
		seen[value] = struct{}{}
		fronts = append(fronts, value)
	}
	add(primary)
	for _, fallback := range config.DefaultBrokerURLs {
		add(fallback)
	}
	return fronts
}

func (s *Service) requestWSSSessionTicket(
	ctx context.Context,
	conn *connection,
	request relay.WSSSessionTicketRequest,
) (relay.WSSSessionTicketResponse, error) {
	fronts := wssTicketBrokerFronts(s.connBrokerURL(conn))
	if len(fronts) == 0 {
		return relay.WSSSessionTicketResponse{}, errors.New("no HTTPS broker fronts configured for WSS ticket")
	}
	requester := s.wssTicketRequester()
	for round := 0; round < 2; round++ {
		var firstErr error
		var retryAfter time.Duration
		for index, brokerURL := range fronts {
			attemptCtx, cancel := context.WithTimeout(ctx, wssTicketAttemptLimit)
			ticket, err := requester(attemptCtx, brokerURL, request, managerClientID(conn.mgr), s.currentSessionID())
			cancel()
			if err == nil {
				return ticket, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			if delay := wssRetryAfter(err); delay > retryAfter {
				retryAfter = delay
			}
			if ctx.Err() != nil {
				return relay.WSSSessionTicketResponse{}, ctx.Err()
			}
			if index+1 < len(fronts) {
				s.appendLog("WSS ticket request failed; trying another broker front")
			}
		}
		if round > 0 || retryAfter <= 0 || conn.wssTicketRetryUsed {
			return relay.WSSSessionTicketResponse{}, firstErr
		}
		if retryAfter > wssTicketMaxRetry {
			retryAfter = wssTicketMaxRetry
		}
		conn.wssTicketRetryUsed = true
		s.appendLog(fmt.Sprintf("broker fronts rate-limited WSS tickets; retrying once in %s", retryAfter))
		if err := s.wssRetryWaiter()(ctx, retryAfter); err != nil {
			return relay.WSSSessionTicketResponse{}, err
		}
	}
	panic("unreachable")
}

func wssRetryAfter(err error) time.Duration {
	var statusErr *client.WSSTicketStatusError
	if !errors.As(err, &statusErr) {
		return 0
	}
	switch statusErr.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if statusErr.RetryAfter > 0 {
			return statusErr.RetryAfter
		}
		return wssTicketDefaultRetry
	default:
		return 0
	}
}

func (s *Service) wssRetryWaiter() func(context.Context, time.Duration) error {
	if s.waitWSSRetry != nil {
		return s.waitWSSRetry
	}
	return func(ctx context.Context, delay time.Duration) error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}

func (s *Service) attemptWSSCandidate(
	ctx context.Context,
	conn *connection,
	candidate relay.Descriptor,
	front relay.WSSFrontDescriptor,
	proxyPort int,
	attempt int,
) (*candidateResult, error) {
	candidateCtx, cancel := context.WithCancel(ctx)
	ticket, err := s.requestWSSSessionTicket(candidateCtx, conn, relay.WSSSessionTicketRequest{
		RelayID: candidate.ID,
		FrontID: front.ID,
	})
	if err != nil {
		cancel()
		return nil, markWSSTransportError("ticket", front.ID, fmt.Errorf("request WSS ticket: %w", err))
	}
	if ticket.URL != front.URL {
		cancel()
		return nil, markWSSTransportError("ticket_binding", front.ID, errors.New("WSS ticket URL does not match the signed relay front"))
	}
	if !ticket.ExpiresAt.After(time.Now()) {
		cancel()
		return nil, markWSSTransportError("ticket_expired", front.ID, errors.New("WSS ticket is already expired"))
	}

	started := time.Now()
	bridge, err := s.wssDialer()(candidateCtx, front.URL, ticket.Ticket)
	if err != nil {
		cancel()
		return nil, markWSSTransportError("wss_handshake", front.ID, fmt.Errorf("connect WSS front: %w", err))
	}
	host, port := bridge.Endpoint()
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		cancel()
		_ = bridge.Close()
		return nil, markWSSTransportError("local_adapter", front.ID, errors.New("WSS adapter returned no safe loopback endpoint"))
	}

	serveCtx, serveCancel := context.WithCancel(context.Background())
	result := &candidateResult{
		relay: candidate, accessTransport: accessTransportWSS, frontID: front.ID,
		ctx: candidateCtx, cancel: cancel,
		wssBridge: bridge, wssDone: make(chan struct{}), wssCancel: serveCancel,
		transportErr: make(chan error, 1),
		proxyPort:    proxyPort, transportMS: time.Since(started).Milliseconds(),
		attempt: int64(attempt), brokerIndex: -1,
	}
	go serveWSS(result, serveCtx, bridge)
	s.appendLog(fmt.Sprintf("connected through WSS front %s", front.ID))
	return s.startCandidate(result, client.SingBoxConfigInput{
		Relay: candidate, Mode: client.ModeProxy,
		ProxyListenAddress: proxyconfig.Host, ProxyListenPort: proxyPort,
		BridgeHost: ip.String(), BridgePort: port,
	})
}

func serveWSS(result *candidateResult, ctx context.Context, bridge wssBridge) {
	defer close(result.wssDone)
	err := bridge.Serve(ctx)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("WSS session stopped unexpectedly")
	}
	err = markWSSTransportError("wss_session", result.frontID, err)
	select {
	case result.transportErr <- err:
	default:
	}
}

func (s *Service) recordTransportFallback(mgr *clienttelemetry.Manager, relayID string, directErr error) {
	if mgr == nil {
		return
	}
	attrs := map[string]string{"from_transport": relay.TransportDirect, "to_transport": accessTransportWSS}
	if reason := clienttelemetry.ClassifyError(directErr); reason != "" {
		attrs["failure_reason"] = reason
	}
	mgr.Record("transport_fallback", relayID, attrs, nil)
}

func (s *Service) recordWSSTransportFailed(mgr *clienttelemetry.Manager, relayID string, err error) {
	if mgr == nil {
		return
	}
	attrs := map[string]string{"transport": accessTransportWSS}
	if reason := clienttelemetry.ClassifyError(err); reason != "" {
		attrs["failure_reason"] = reason
	}
	if stage, frontID, ok := wssTransportMetadata(err); ok {
		attrs["failure_stage"] = stage
		if frontID != "" {
			attrs["front_id"] = frontID
		}
	}
	mgr.Record("transport_failed", relayID, attrs, nil)
}
