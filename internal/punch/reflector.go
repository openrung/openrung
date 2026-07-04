package punch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// wire codecs -------------------------------------------------------------

func buildReflectRequest(nonce []byte) []byte {
	buf := make([]byte, reflectMinRequest)
	copy(buf, reflectMagicRequest)
	copy(buf[len(reflectMagicRequest):], nonce)
	// remainder stays zero padding to reach the anti-amplification floor
	return buf
}

// parseReflectRequest validates a request datagram and returns its nonce.
func parseReflectRequest(data []byte) ([]byte, bool) {
	if len(data) < reflectMinRequest {
		return nil, false
	}
	if string(data[:len(reflectMagicRequest)]) != reflectMagicRequest {
		return nil, false
	}
	nonce := make([]byte, reflectNonceLen)
	copy(nonce, data[len(reflectMagicRequest):len(reflectMagicRequest)+reflectNonceLen])
	return nonce, true
}

func buildReflectReply(nonce []byte, addr *net.UDPAddr) []byte {
	buf := make([]byte, 0, len(reflectMagicReply)+reflectNonceLen+1+16+2)
	buf = append(buf, reflectMagicReply...)
	buf = append(buf, nonce...)
	if ip4 := addr.IP.To4(); ip4 != nil {
		buf = append(buf, 4)
		buf = append(buf, ip4...)
	} else {
		buf = append(buf, 6)
		buf = append(buf, addr.IP.To16()...)
	}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], uint16(addr.Port))
	buf = append(buf, port[:]...)
	return buf
}

// parseReflectReply validates a reply and returns the echoed nonce and observed
// endpoint.
func parseReflectReply(data []byte) (nonce []byte, observed *net.UDPAddr, ok bool) {
	off := len(reflectMagicReply)
	if len(data) < off+reflectNonceLen+1 {
		return nil, nil, false
	}
	if string(data[:off]) != reflectMagicReply {
		return nil, nil, false
	}
	nonce = make([]byte, reflectNonceLen)
	copy(nonce, data[off:off+reflectNonceLen])
	off += reflectNonceLen
	family := data[off]
	off++
	var ipLen int
	switch family {
	case 4:
		ipLen = 4
	case 6:
		ipLen = 16
	default:
		return nil, nil, false
	}
	if len(data) < off+ipLen+2 {
		return nil, nil, false
	}
	ip := make(net.IP, ipLen)
	copy(ip, data[off:off+ipLen])
	off += ipLen
	port := binary.BigEndian.Uint16(data[off : off+2])
	return nonce, &net.UDPAddr{IP: ip, Port: int(port)}, true
}

// Reflector -----------------------------------------------------------------

// Reflector is the hub-side UDP STUN-like reflector. It binds one socket per
// configured public IP (same port) and echoes the observed source ip:port of each
// validated request. Binding on two distinct IPs lets the hub classify a peer's
// NAT mapping behaviour (EIM vs symmetric) by correlating the observations for a
// single client nonce across both IPs (see Classify).
type Reflector struct {
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	conns   []*net.UDPConn
	limiter *ipRateLimiter

	mu  sync.Mutex
	obs map[string]*reflectObservation
}

type reflectObservation struct {
	// endpoints maps the reflector's local IP (the IP a request arrived on) to
	// the source endpoint it observed for this nonce.
	endpoints map[string]Endpoint
	seen      time.Time
}

// NewReflector binds a UDP socket on each address (host:port) and starts
// serving. Call Close to release the sockets.
func NewReflector(addrs []string, logger *slog.Logger) (*Reflector, error) {
	if len(addrs) == 0 {
		return nil, errors.New("reflector requires at least one bind address")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reflector{
		Logger:  logger,
		limiter: newIPRateLimiter(20, 40), // ~20 req/s per source IP, burst 40
		obs:     make(map[string]*reflectObservation),
	}
	for _, addr := range addrs {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("resolve reflector addr %q: %w", addr, err)
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("listen reflector udp %q: %w", addr, err)
		}
		r.conns = append(r.conns, conn)
	}
	for _, conn := range r.conns {
		go r.serve(conn)
	}
	go r.gcLoop()
	return r, nil
}

func (r *Reflector) serve(conn *net.UDPConn) {
	localIP := conn.LocalAddr().(*net.UDPAddr).IP.String()
	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed
		}
		nonce, ok := parseReflectRequest(buf[:n])
		if !ok {
			continue
		}
		if !r.limiter.allow(src.IP.String()) {
			continue
		}
		r.record(string(nonce), localIP, endpointFromUDP(src, KindSrflx))
		_, _ = conn.WriteToUDP(buildReflectReply(nonce, src), src)
	}
}

func (r *Reflector) record(nonce, localIP string, observed Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.obs[nonce]
	if !ok {
		entry = &reflectObservation{endpoints: make(map[string]Endpoint)}
		r.obs[nonce] = entry
	}
	entry.endpoints[localIP] = observed
	entry.seen = time.Now()
}

// Classify returns the NAT class and observed reflexive endpoints for a nonce,
// based on this reflector's own observations. Two reflector IPs with the same
// observed source port => EIM (punchable); differing ports => symmetric; fewer
// than two observations => unknown. ok is false when the reflector saw nothing
// for the nonce (peer never probed, or datagrams were lost).
func (r *Reflector) Classify(nonce string) (class string, reflexive []Endpoint, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.obs[nonce]
	if !found || len(entry.endpoints) == 0 {
		return ClassUnknown, nil, false
	}
	ports := make(map[int]struct{})
	for _, ep := range entry.endpoints {
		reflexive = append(reflexive, ep)
		ports[ep.Port] = struct{}{}
	}
	reflexive = dedupeEndpoints(reflexive)
	switch {
	case len(entry.endpoints) < 2:
		class = ClassUnknown
	case len(ports) == 1:
		class = ClassEIM
	default:
		class = ClassSymmetric
	}
	return class, reflexive, true
}

func (r *Reflector) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if r.closed() {
			return
		}
		cutoff := time.Now().Add(-2 * time.Minute)
		r.mu.Lock()
		for nonce, entry := range r.obs {
			if entry.seen.Before(cutoff) {
				delete(r.obs, nonce)
			}
		}
		empty := r.conns == nil
		r.mu.Unlock()
		// Prune idle rate-limiter buckets too; otherwise an attacker spraying
		// spoofed source IPs at the reflector grows the map without bound (an
		// unauthenticated memory-exhaustion DoS).
		r.limiter.prune(cutoff)
		if empty {
			return
		}
	}
}

func (r *Reflector) closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns == nil
}

// Addrs returns the reflector's bound addresses as host:port strings.
func (r *Reflector) Addrs() []string {
	addrs := make([]string, 0, len(r.conns))
	for _, conn := range r.conns {
		addrs = append(addrs, conn.LocalAddr().String())
	}
	return addrs
}

// Close releases the reflector sockets.
func (r *Reflector) Close() error {
	r.mu.Lock()
	conns := r.conns
	r.conns = nil
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return nil
}

// ipRateLimiter is a simple per-source-IP token bucket.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens per second
	burst   float64
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(rate, burst float64) *ipRateLimiter {
	return &ipRateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    rate,
		burst:   burst,
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets untouched since cutoff, bounding memory against spoofed
// source IPs. A pruned bucket that reappears simply refills from full, which is
// harmless.
func (l *ipRateLimiter) prune(cutoff time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}
