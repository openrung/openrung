package connectcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"

	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/proxyconfig"
)

const (
	accessTransportWSS = "wss"
	// wssSessionEndedStage marks an orderly session end rather than a failure.
	wssSessionEndedStage = "wss_session_ended"
	// wssNetworkEpochStage marks a WSS session retired by a physical-network
	// epoch boundary (see network.go): orderly like a session end — neither
	// the relay nor the front failed — but distinguishable in telemetry.
	wssNetworkEpochStage  = "wss_network_epoch"
	wssTicketAttemptLimit = 5 * time.Second
	wssTicketDefaultRetry = 10 * time.Second
	wssTicketMaxRetry     = 30 * time.Second
)

type wssBridge interface {
	Endpoint() (host string, port int)
	Serve(context.Context) error
	SessionEnd() wsscore.SessionEnd
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
func supportedWSSFronts(candidate brokerapi.RelayDescriptor) []brokerapi.RelayWSSFront {
	transport := strings.ToLower(strings.TrimSpace(candidate.Transport))
	if transport == "" {
		transport = brokerapi.TransportDirect
	}
	if transport != brokerapi.TransportDirect ||
		brokerapi.EffectiveNodeClass(candidate.NodeClass) != brokerapi.NodeClassFoundation ||
		candidate.ExitMode != brokerapi.ExitModeDirect ||
		candidate.PublicPort != 443 {
		return nil
	}
	// The signed entries must already be canonical by wsscore's rules and in
	// canonical order; anything else is rejected rather than repaired.
	fronts := make([]wsscore.Front, len(candidate.WSSFronts))
	for index, front := range candidate.WSSFronts {
		fronts[index] = wsscore.Front(front)
	}
	normalized, err := wsscore.NormalizeFronts(fronts)
	if err != nil || !slices.Equal(normalized, fronts) {
		return nil
	}
	return candidate.WSSFronts
}

func (s *Engine) wssTicketRequester() func(context.Context, string, brokerapi.WSSTicketRequest, string, string) (brokerapi.WSSTicketResponse, error) {
	if s.requestWSSTicket != nil {
		return s.requestWSSTicket
	}
	return func(ctx context.Context, brokerURL string, request brokerapi.WSSTicketRequest, clientID, sessionID string) (brokerapi.WSSTicketResponse, error) {
		brokerClient := client.BrokerClient{
			BaseURL:    brokerURL,
			HTTPClient: s.brokerHTTPClient(),
			Platform:   s.telemetryPlatform(),
		}
		return brokerClient.RequestWSSSessionTicket(ctx, request, clientID, sessionID)
	}
}

func (s *Engine) wssDialer() func(context.Context, string, string) (wssBridge, error) {
	if s.dialWSS != nil {
		return s.dialWSS
	}
	return func(ctx context.Context, rawURL, ticket string) (wssBridge, error) {
		return wsscore.DialClient(ctx, wsscore.ClientOptions{
			URL: rawURL, Ticket: ticket, NativeFrontNoSNI: true,
			SocketProtector: s.SocketProtector,
		})
	}
}

// wssTicketBrokerFronts places the endpoint-authenticated broker front that
// served this session's signed directory first, then adds each equally strong
// configured default. A relay-list signature can recover authenticity when a
// discovery front authenticates only its CDN, but a bearer ticket also needs
// confidentiality: ticket requests therefore require TLS authentication of
// the exact broker endpoint and never use endpoint-unbound fronts.
func wssTicketBrokerFronts(primary string) []string {
	fronts := make([]string, 0, len(DefaultBrokerURLs)+1)
	seen := make(map[string]struct{}, len(DefaultBrokerURLs)+1)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || brokerapi.EndpointUnboundBrokerFront(value) {
			return
		}
		if _, duplicate := seen[value]; duplicate {
			return
		}
		seen[value] = struct{}{}
		fronts = append(fronts, value)
	}
	add(primary)
	for _, fallback := range DefaultBrokerURLs {
		add(fallback)
	}
	return fronts
}

func (s *Engine) requestWSSSessionTicket(
	ctx context.Context,
	conn *connection,
	request brokerapi.WSSTicketRequest,
) (brokerapi.WSSTicketResponse, error) {
	fronts := wssTicketBrokerFronts(s.connBrokerURL(conn))
	if len(fronts) == 0 {
		return brokerapi.WSSTicketResponse{}, errors.New("no HTTPS broker fronts configured for WSS ticket")
	}
	requester := s.wssTicketRequester()
	for round := 0; round < 2; round++ {
		var firstErr error
		var retryAfter time.Duration
		for index, brokerURL := range fronts {
			attemptCtx, cancel := context.WithTimeout(ctx, wssTicketAttemptLimit)
			ticket, err := requester(attemptCtx, brokerURL, request, managerClientID(conn.mgr), s.SessionID())
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
				return brokerapi.WSSTicketResponse{}, ctx.Err()
			}
			if index+1 < len(fronts) {
				s.appendLog("WSS ticket request failed; trying another broker front")
			}
		}
		if round > 0 || retryAfter <= 0 || conn.wssTicketRetryUsed {
			return brokerapi.WSSTicketResponse{}, firstErr
		}
		if retryAfter > wssTicketMaxRetry {
			retryAfter = wssTicketMaxRetry
		}
		conn.wssTicketRetryUsed = true
		s.appendLog(fmt.Sprintf("broker fronts rate-limited WSS tickets; retrying once in %s", retryAfter))
		s.notify(Notice{Kind: NoticeWSSTicketRetry, RelayID: request.RelayID, FrontID: request.FrontID, Wait: retryAfter})
		if err := s.wssRetryWaiter()(ctx, retryAfter); err != nil {
			return brokerapi.WSSTicketResponse{}, err
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

func (s *Engine) wssRetryWaiter() func(context.Context, time.Duration) error {
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

func (s *Engine) attemptWSSCandidate(
	ctx context.Context,
	conn *connection,
	candidate brokerapi.RelayDescriptor,
	front brokerapi.RelayWSSFront,
	proxyPort int,
	attempt int,
) (*candidateResult, error) {
	candidateCtx, cancel := context.WithCancel(ctx)
	ticket, err := s.requestWSSSessionTicket(candidateCtx, conn, brokerapi.WSSTicketRequest{
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
		if isSocketProtectionFailure(err) {
			// A refused protection is a local platform failure, not front
			// evidence: stop the ladder instead of burning the remaining
			// fronts' single-use tickets on dials that cannot succeed.
			return nil, markLocalCandidateError("socket_protection", err)
		}
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
	// Serve returns nil for every session end, orderly or not, so the reason has
	// to come from the transport itself. An orderly end still costs the tunnel
	// and has to be rebuilt, but nothing failed: it must not count against the
	// front or the relay.
	end := bridge.SessionEnd()
	stage := "wss_session"
	if end.Graceful() {
		stage = wssSessionEndedStage
	}
	if err == nil {
		err = fmt.Errorf("WSS session ended (%s)", end)
	}
	err = markWSSTransportError(stage, result.frontID, err)
	select {
	case result.transportErr <- err:
	default:
	}
}

// gracefulWSSSessionEnd reports an orderly end of a promoted WSS session: the
// relay closed it, or its bounded lifetime elapsed. Only a session that ended
// without the peer saying so is evidence that the path was lost.
func gracefulWSSSessionEnd(err error) bool {
	stage, ok := wssTransportStage(err)
	return ok && (stage == wssSessionEndedStage || stage == wssNetworkEpochStage)
}

func (s *Engine) recordTransportFallback(mgr *clienttelemetry.Manager, relayID string, directErr error) {
	if mgr == nil {
		return
	}
	attrs := map[string]string{"from_transport": brokerapi.TransportDirect, "to_transport": accessTransportWSS}
	if reason := clienttelemetry.ClassifyError(directErr); reason != "" {
		attrs["failure_reason"] = reason
	}
	mgr.Record("transport_fallback", relayID, attrs, nil)
}

// recordWSSTransportEnded reports an orderly session end on its own event, so
// transport_failed keeps meaning "the path broke" and stays usable as a signal.
func (s *Engine) recordWSSTransportEnded(mgr *clienttelemetry.Manager, relayID string, err error) {
	if mgr == nil {
		return
	}
	attrs := map[string]string{"transport": accessTransportWSS}
	if stage, frontID, ok := wssTransportMetadata(err); ok {
		if frontID != "" {
			attrs["front_id"] = frontID
		}
		if stage == wssNetworkEpochStage {
			attrs["trigger"] = "network_epoch"
		}
	}
	mgr.Record("transport_session_ended", relayID, attrs, nil)
}

func (s *Engine) recordWSSTransportFailed(mgr *clienttelemetry.Manager, relayID string, err error) {
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
