package egress

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress/mitm"
)

const (
	// AllowlistHeader and SandboxHeader are stripped from proxied requests.
	// The proxy never reads them: the guest sets its own headers.
	AllowlistHeader = "X-ASP-Allowlist-JSON"
	SandboxHeader   = "X-ASP-Sandbox-ID"
	DefaultMaxBody  = 8 << 20 // 8 MiB
	DefaultRate     = 40
	DefaultBurst    = 80
)

// AllowlistJSON is the wire format for env allowlists (mirrors execproxy DTO).
type AllowlistJSON struct {
	Mode  string `json:"mode"`
	Rules []struct {
		HostPattern string `json:"host_pattern"`
		Port        *int   `json:"port,omitempty"`
		Enabled     bool   `json:"enabled"`
	} `json:"rules"`
}

// ForwardProxy is an optional HTTP(S) forward proxy that deny-by-default checks allowlists.
// Guests should set HTTP_PROXY/HTTPS_PROXY to the host TAP IP listening address.
type ForwardProxy struct {
	// Default is used when no header/env/cache policy is available.
	Default *Allowlist
	// Cache maps the guest source address to its sandbox's allowlist.
	Cache *PolicyCache
	// EnvJSON, when non-empty, is parsed once as ASP_EGRESS_ALLOWLIST_JSON fallback.
	EnvJSON string
	Logger  *slog.Logger
	// Enforce when true returns 403 on deny; when false still checks but always dials
	// (lab). Wire this behind --egress-enforce.
	Enforce bool

	// RateLimit per host/sandbox (token bucket). Nil disables.
	RateLimit *TokenBucket
	// MaxBodyBytes limits proxied HTTP request bodies (0 = DefaultMaxBody).
	// Responses are streamed in full, as they are through a CONNECT tunnel.
	MaxBodyBytes int64
	// MITM enables CONNECT TLS bump when non-nil and ASP_EGRESS_MITM=1.
	MITM *mitm.CA

	envOnce sync.Once
	envAL   *Allowlist

	clientOnce sync.Once
	client     *http.Client
}

// upstreamClient is shared by every plain-HTTP request so connections to
// upstreams are reused. There is no overall timeout: it would also cover the
// response body and cut a long download. Each phase up to the response headers
// is bounded instead, and the guest closing its connection cancels the request.
func (p *ForwardProxy) upstreamClient() *http.Client {
	p.clientOnce.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		tr.TLSHandshakeTimeout = 10 * time.Second
		tr.ResponseHeaderTimeout = 30 * time.Second
		p.client = &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
	return p.client
}

// hopHeaders apply to one connection only and are not forwarded (RFC 7230 §6.1).
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopHeaders drops the hop-by-hop headers and the ones Connection names.
func removeHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// resolveAllowlist picks the policy from the connection's source address.
// Request headers are written by the guest and never select a policy: a
// guest that could send its own allowlist could allow everything.
func (p *ForwardProxy) resolveAllowlist(r *http.Request) *Allowlist {
	if r != nil && p.Cache != nil {
		if al, known := p.Cache.ForAddr(remoteAddr(r.RemoteAddr)); known {
			return al
		}
	}
	return p.nodeAllowlist()
}

// nodeAllowlist is the node-wide policy for sources that are not a known
// sandbox: ASP_EGRESS_ALLOWLIST_JSON, then Default, then deny.
func (p *ForwardProxy) nodeAllowlist() *Allowlist {
	p.envOnce.Do(func() {
		raw := p.EnvJSON
		if raw == "" {
			raw = os.Getenv("ASP_EGRESS_ALLOWLIST_JSON")
		}
		if raw != "" {
			al, err := ParseAllowlistJSON(raw)
			if err == nil {
				p.envAL = al
			}
		}
	})
	if p.envAL != nil {
		return p.envAL
	}
	if p.Default != nil {
		return p.Default
	}
	return NewAllowlistFromPolicy("deny-default", nil)
}

// ParseAllowlistJSON builds an Allowlist from JSON (mode + rules).
func ParseAllowlistJSON(raw string) (*Allowlist, error) {
	var dto AllowlistJSON
	if err := json.Unmarshal([]byte(raw), &dto); err != nil {
		return nil, err
	}
	mode := dto.Mode
	if mode == "" {
		mode = "deny-default"
	}
	rules := make([]Rule, 0, len(dto.Rules))
	anyEnabled := false
	for _, r := range dto.Rules {
		if r.Enabled {
			anyEnabled = true
			break
		}
	}
	for _, r := range dto.Rules {
		if r.HostPattern == "" {
			continue
		}
		if anyEnabled && !r.Enabled {
			continue
		}
		rules = append(rules, Rule{HostPattern: r.HostPattern, Port: r.Port})
	}
	return NewAllowlistFromPolicy(mode, rules), nil
}

// Handler returns an http.Handler suitable for ListenAndServe.
func (p *ForwardProxy) Handler() http.Handler {
	return http.HandlerFunc(p.ServeHTTP)
}

func (p *ForwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	al := p.resolveAllowlist(r)
	if r.Method == http.MethodConnect {
		p.handleCONNECT(w, r, al)
		return
	}
	p.handleHTTP(w, r, al)
}

func (p *ForwardProxy) allow(al *Allowlist, host string, port int) bool {
	if al == nil {
		return false
	}
	return al.CheckHostPort(host, port) == nil
}

// sandboxID names the caller for rate limiting. It comes from the source
// address, not from a header the guest controls.
func (p *ForwardProxy) sandboxID(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := remoteAddr(r.RemoteAddr)
	if id := p.Cache.SandboxFor(addr); id != "" {
		return id
	}
	if addr.IsValid() {
		return addr.String()
	}
	return ""
}

func (p *ForwardProxy) rateKey(sandbox, host string) string {
	if sandbox == "" {
		sandbox = "_"
	}
	return sandbox + "|" + strings.ToLower(host)
}

func (p *ForwardProxy) maxBody() int64 {
	if p.MaxBodyBytes > 0 {
		return p.MaxBodyBytes
	}
	if raw := strings.TrimSpace(os.Getenv("ASP_EGRESS_MAX_BODY")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxBody
}

func (p *ForwardProxy) mitmEnabled() bool {
	return p.MITM != nil && (os.Getenv("ASP_EGRESS_MITM") == "1" || os.Getenv("ASP_EGRESS_MITM") == "true")
}

func (p *ForwardProxy) audit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = event
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(fields)
	line := string(b)
	if p.Logger != nil {
		p.Logger.Info("egress_audit", "json", line)
	} else {
		slog.Info("egress_audit", "json", line)
	}
}

func (p *ForwardProxy) deny(w http.ResponseWriter, host string, port int, reason string, sandbox string) {
	p.audit("deny", map[string]any{"host": host, "port": port, "reason": reason, "sandbox_id": sandbox})
	http.Error(w, "host not allowed", http.StatusForbidden)
}

func (p *ForwardProxy) handleCONNECT(w http.ResponseWriter, r *http.Request, al *Allowlist) {
	sandbox := p.sandboxID(r)
	host, port := ParseHostPort(r.Host)
	if port <= 0 {
		port = 443
	}
	if !p.allow(al, host, port) {
		p.deny(w, host, port, "allowlist", sandbox)
		return
	}
	if p.RateLimit != nil && !p.RateLimit.Allow(p.rateKey(sandbox, host)) {
		p.audit("rate_limited", map[string]any{"host": host, "port": port, "sandbox_id": sandbox})
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if p.mitmEnabled() {
		p.handleCONNECTMITM(w, r, host, port, sandbox)
		return
	}
	dest := net.JoinHostPort(host, itoa(port))
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	serverConn, err := net.DialTimeout("tcp", dest, 15*time.Second)
	if err != nil {
		_, _ = bufrw.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = bufrw.Flush()
		_ = clientConn.Close()
		return
	}
	_, _ = bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = bufrw.Flush()
	p.audit("connect", map[string]any{"host": host, "port": port, "sandbox_id": sandbox, "mitm": false})
	go tunnel(serverConn, clientConn)
	go tunnel(clientConn, serverConn)
}

func (p *ForwardProxy) handleCONNECTMITM(w http.ResponseWriter, r *http.Request, host string, port int, sandbox string) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_, _ = bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = bufrw.Flush()

	leaf, err := p.MITM.CertificateForHost(host)
	if err != nil {
		_ = clientConn.Close()
		return
	}
	tlsClient := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		_ = clientConn.Close()
		return
	}
	dest := net.JoinHostPort(host, itoa(port))
	rawUp, err := net.DialTimeout("tcp", dest, 15*time.Second)
	if err != nil {
		_ = tlsClient.Close()
		return
	}
	tlsUp := tls.Client(rawUp, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsUp.Handshake(); err != nil {
		_ = rawUp.Close()
		_ = tlsClient.Close()
		return
	}
	p.audit("connect", map[string]any{"host": host, "port": port, "sandbox_id": sandbox, "mitm": true})
	go tunnel(tlsUp, tlsClient)
	go tunnel(tlsClient, tlsUp)
}

func (p *ForwardProxy) handleHTTP(w http.ResponseWriter, r *http.Request, al *Allowlist) {
	sandbox := p.sandboxID(r)
	if r.URL == nil || r.URL.Host == "" && r.Host == "" {
		http.Error(w, "absolute URL required", http.StatusBadRequest)
		return
	}
	scheme := strings.ToLower(r.URL.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		p.audit("deny", map[string]any{"reason": "scheme", "scheme": scheme, "sandbox_id": sandbox})
		http.Error(w, "scheme not allowed", http.StatusBadRequest)
		return
	}
	rawHost := r.URL.Host
	if rawHost == "" {
		rawHost = r.Host
	}
	host, port := ParseHostPort(rawHost)
	if port <= 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	if !p.allow(al, host, port) {
		p.deny(w, host, port, "allowlist", sandbox)
		return
	}
	if p.RateLimit != nil && !p.RateLimit.Allow(p.rateKey(sandbox, host)) {
		p.audit("rate_limited", map[string]any{"host": host, "port": port, "sandbox_id": sandbox})
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	// Rebuild absolute URL for outbound dial.
	outURL := *r.URL
	if outURL.Scheme == "" {
		outURL.Scheme = "http"
	}
	if outURL.Host == "" {
		outURL.Host = rawHost
	}
	if r.ContentLength > p.maxBody() {
		p.audit("deny", map[string]any{"reason": "body_too_large", "host": host, "sandbox_id": sandbox})
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var body io.Reader = http.NoBody
	if r.ContentLength != 0 {
		body = http.MaxBytesReader(w, r.Body, p.maxBody())
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Keep the guest's Content-Length (-1 when it sent chunked) so the upstream
	// gets a length when there is one.
	outReq.ContentLength = r.ContentLength
	outReq.Header = r.Header.Clone()
	removeHopHeaders(outReq.Header)
	outReq.Header.Del(AllowlistHeader)
	outReq.Header.Del(SandboxHeader)
	resp, err := p.upstreamClient().Do(outReq)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "http: request body too large") {
			p.audit("deny", map[string]any{"reason": "body_too_large", "host": host, "sandbox_id": sandbox})
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.audit("http", map[string]any{
		"host": host, "port": port, "method": r.Method, "status": resp.StatusCode, "sandbox_id": sandbox,
	})
	header := resp.Header.Clone()
	removeHopHeaders(header)
	for k, vv := range header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func tunnel(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ListenAndServe starts the forward proxy on addr (e.g. ":8888" or "0.0.0.0:8888").
func (p *ForwardProxy) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = ln.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
