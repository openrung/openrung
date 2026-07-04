package tunnel

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PathProbe is the hub HTTP endpoint a volunteer calls to test whether it is
// reachable from the public internet.
const PathProbe = "/api/v1/probe"

// ProbeLinePrefix is the line a volunteer's temporary probe listener writes on an
// accepted connection so the hub can confirm it reached that specific volunteer
// (not some other device answering on the same public IP:port).
const ProbeLinePrefix = "ORPROBE "

const (
	probeDialTimeout = 3 * time.Second
	probeMaxLine     = 128
)

// ProbeRequest is the volunteer -> hub body for POST PathProbe. The volunteer
// only chooses the port and a nonce; the hub always dials the request's own
// source IP, never a caller-specified host, so this cannot be used for SSRF.
type ProbeRequest struct {
	Port  int    `json:"port"`
	Nonce string `json:"nonce"`
}

// ProbeResponse reports whether the hub could open an inbound TCP connection to
// the volunteer at its observed public IP and the requested port.
type ProbeResponse struct {
	Reachable    bool   `json:"reachable"`
	ObservedHost string `json:"observed_host"`
	Error        string `json:"error,omitempty"`
}

// ReachabilityProber is the hub-side self-check service. It lets a volunteer
// discover whether it can accept inbound connections (so it should register
// directly) or is behind CGNAT/a firewall (so it should tunnel).
type ReachabilityProber struct {
	// Token, when non-empty, is required (constant-time) as a bearer token.
	Token string
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	limiter  *punchLimiter
	inflight *inflightCap
}

// NewReachabilityProber builds a prober. token is the shared volunteer token.
func NewReachabilityProber(token string, logger *slog.Logger) *ReachabilityProber {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReachabilityProber{
		Token:    token,
		Logger:   logger,
		limiter:  newPunchLimiter(2, 4), // ~2 probes/s per source IP, burst 4
		inflight: newInflightCap(64, 4), // <=64 concurrent probes total, <=4 per IP
	}
}

// Register mounts the probe route on mux.
func (p *ReachabilityProber) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathProbe, p.handle)
}

func (p *ReachabilityProber) handle(w http.ResponseWriter, r *http.Request) {
	if !bearerAuthorized(r, p.Token) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ip := requestIP(r)
	if !p.limiter.allow(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	// The handler blocks for up to a few seconds on the dial-back, so cap
	// concurrency (per IP and globally) to prevent goroutine exhaustion.
	if !p.inflight.acquire("probe", ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	defer p.inflight.release("probe", ip)

	var req ProbeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, ProbeResponse{Reachable: false, Error: "invalid request"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeJSONResponse(w, http.StatusBadRequest, ProbeResponse{Reachable: false, Error: "invalid port"})
		return
	}
	if req.Nonce == "" || len(req.Nonce) > 64 {
		writeJSONResponse(w, http.StatusBadRequest, ProbeResponse{Reachable: false, Error: "invalid nonce"})
		return
	}

	writeJSONResponse(w, http.StatusOK, p.dialAndVerify(ip, req.Port, req.Nonce))
}

// dialAndVerify dials back the volunteer's observed source IP at the requested
// port and confirms it answers with the expected nonce line. It only ever dials
// ip (the caller's own address), so it is not an SSRF vector.
func (p *ReachabilityProber) dialAndVerify(ip string, port int, nonce string) ProbeResponse {
	resp := ProbeResponse{ObservedHost: ip}
	target := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", target, probeDialTimeout)
	if err != nil {
		resp.Error = "dial failed"
		return resp
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(probeDialTimeout))
	// Hard-cap the read: we only need a short nonce line, and the peer (the
	// caller's own IP:port) is untrusted, so never buffer more than probeMaxLine.
	line, err := bufio.NewReader(io.LimitReader(conn, probeMaxLine)).ReadString('\n')
	if err != nil && line == "" {
		resp.Error = "no response"
		return resp
	}
	want := ProbeLinePrefix + nonce
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(line)), []byte(want)) == 1 {
		resp.Reachable = true
	} else {
		resp.Error = "nonce mismatch"
	}
	return resp
}

// bearerAuthorized checks an "Authorization: Bearer <token>" header in constant
// time. An empty configured token disables the check.
func bearerAuthorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(token)) == 1
}
