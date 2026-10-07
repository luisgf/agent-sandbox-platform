package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Calls from the control plane to node agents (exec, exec/stdin).
//
// An agent endpoint is plain http:// on loopback (a same-host node, the default
// http://127.0.0.1:9100) or https:// served by the agent's --agent-tls-listen. HTTPS
// uses mutual TLS: the control plane trusts only its CA, expects the certificate of
// the node it means to reach (ServerName = node id, a SAN of every node certificate)
// and presents its own client identity (CN asp-control-plane). Plain HTTP to another
// host would expose exec and the sandbox egress policy to the network, so it needs
// ASP_INSECURE_AGENT_HTTP=1 (lab only).
//
// No client has an overall timeout: http.Client.Timeout also covers reading the
// response body, so it would cut a streamed exec (NDJSON) or a PTY session while
// output is still flowing. AgentTimeouts bounds each phase up to the agent's
// response headers instead. After that a stream lasts as long as the command, and
// a caller that disconnects cancels the upstream call, which carries the incoming
// request's context. Buffered calls also keep an overall deadline
// (Server.BufferedExecTimeout).

// EnvInsecureAgentHTTP allows plain http:// agent endpoints on non-loopback hosts.
const EnvInsecureAgentHTTP = "ASP_INSECURE_AGENT_HTTP"

// controlPlaneCertTTL is the lifetime of the control plane's client certificate.
// It is issued in memory from the CA and renewed at two thirds of its lifetime.
const controlPlaneCertTTL = 30 * 24 * time.Hour

// AgentTimeouts bounds the phases of a call to a node agent, up to its response
// headers. Zero means no limit for that phase.
type AgentTimeouts struct {
	// Dial bounds the TCP connect.
	Dial time.Duration
	// TLSHandshake bounds the mutual TLS handshake with an https:// agent.
	TLSHandshake time.Duration
	// ResponseHeader bounds the wait for the agent to start answering once the
	// request is sent. An agent answers a stream with its first event (at once for
	// a PTY or a stdin stream) but a buffered exec only when the command exits, so
	// a buffered exec goes through bufferedTwin, which has no such wait: its own
	// limit is the call's context (Server.BufferedExecTimeout).
	ResponseHeader time.Duration
}

// defaultAgentTimeouts gives an agent 30s to start answering, the overall limit
// every call had before.
func defaultAgentTimeouts() AgentTimeouts {
	return AgentTimeouts{Dial: 10 * time.Second, TLSHandshake: 10 * time.Second, ResponseHeader: 30 * time.Second}
}

// apply sets t's bounds on tr. A node that vanishes mid-stream is noticed by TCP
// keep-alive, which the dialer enables by default.
func (t AgentTimeouts) apply(tr *http.Transport) {
	tr.DialContext = (&net.Dialer{Timeout: t.Dial}).DialContext
	tr.TLSHandshakeTimeout = t.TLSHandshake
	tr.ResponseHeaderTimeout = t.ResponseHeader
}

// newAgentHTTPClient returns the client for plain http:// agents: the default
// transport with t's bounds.
func newAgentHTTPClient(t AgentTimeouts) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.apply(tr)
	return &http.Client{Transport: tr}
}

// AgentDialer builds the HTTP clients the control plane uses to reach node agents.
type AgentDialer struct {
	CA                *pki.CA
	AllowInsecureHTTP bool
	// Timeouts bounds every call. Clients are cached per node, so set it before
	// the first call.
	Timeouts AgentTimeouts

	mu          sync.Mutex
	clients     map[string]*http.Client
	cert        *tls.Certificate
	certRenewAt time.Time
}

// NewAgentDialer returns a dialer with the same timeouts as Server.Client.
func NewAgentDialer(ca *pki.CA, allowInsecureHTTP bool) *AgentDialer {
	return &AgentDialer{CA: ca, AllowInsecureHTTP: allowInsecureHTTP, Timeouts: defaultAgentTimeouts()}
}

func (d *AgentDialer) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cert != nil && time.Now().Before(d.certRenewAt) {
		return d.cert, nil
	}
	issued, err := d.CA.IssueControlPlaneClient(controlPlaneCertTTL)
	if err != nil {
		return nil, fmt.Errorf("issue control-plane client certificate: %w", err)
	}
	pair, err := tls.X509KeyPair(issued.CertPEM, issued.KeyPEM)
	if err != nil {
		return nil, err
	}
	d.cert = &pair
	d.certRenewAt = issued.NotBefore.Add(issued.NotAfter.Sub(issued.NotBefore) * 2 / 3)
	return d.cert, nil
}

// clientFor returns the mTLS client for one node. Clients are cached per node and
// endpoint so connections are reused; ServerName pins the node's identity, so an
// endpoint pointing at another node's agent fails the handshake.
func (d *AgentDialer) clientFor(nodeID, baseURL string) *http.Client {
	key := nodeID + "\x00" + baseURL
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.clients[key]; ok {
		return c
	}
	if d.clients == nil {
		d.clients = map[string]*http.Client{}
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:           tls.VersionTLS12,
			RootCAs:              d.CA.CertPool(),
			ServerName:           nodeID,
			GetClientCertificate: d.clientCertificate,
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	d.Timeouts.apply(tr)
	c := &http.Client{Transport: tr}
	d.clients[key] = c
	return c
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateAgentEndpoint rejects agent endpoints the control plane would refuse to call.
// Empty and local:// (stub) endpoints are accepted: they never receive exec traffic.
func ValidateAgentEndpoint(raw string, allowInsecureHTTP bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "local://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("agent endpoint %q is not a URL with a host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) || allowInsecureHTTP {
			return nil
		}
		return fmt.Errorf("agent endpoint %s is plain HTTP on a non-loopback host: serve the agent with --agent-tls-listen (https) or set %s=1 on the control plane (lab only)", raw, EnvInsecureAgentHTTP)
	default:
		return fmt.Errorf("agent endpoint %q: scheme must be http or https", raw)
	}
}

func (s *Server) allowInsecureAgentHTTP() bool {
	return s.Agents != nil && s.Agents.AllowInsecureHTTP
}

// effectiveAgentEndpoint is where exec goes: agent_endpoint, else endpoint.
func effectiveAgentEndpoint(agentEndpoint, endpoint string) string {
	if u := strings.TrimRight(strings.TrimSpace(agentEndpoint), "/"); u != "" {
		return u
	}
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}

// agentTarget resolves the node agent that runs sb and the client to reach it.
// When status is non-zero the caller writes it with msg.
func (s *Server) agentTarget(sb store.Sandbox) (baseURL string, client *http.Client, status int, msg string) {
	if sb.NodeID == nil || *sb.NodeID == "" {
		return "", nil, http.StatusConflict, "sandbox has no assigned node"
	}
	node, err := s.Store.GetNode(*sb.NodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, http.StatusConflict, "assigned node not registered"
		}
		return "", nil, http.StatusInternalServerError, err.Error()
	}
	if node.RevokedAt != nil {
		return "", nil, http.StatusConflict, fmt.Sprintf("assigned node %s is revoked", node.ID)
	}
	baseURL = effectiveAgentEndpoint(node.AgentEndpoint, node.Endpoint)
	if baseURL == "" || strings.HasPrefix(baseURL, "local://") {
		return "", nil, http.StatusBadGateway, "node has no agent_endpoint for exec"
	}
	if err := ValidateAgentEndpoint(baseURL, s.allowInsecureAgentHTTP()); err != nil {
		return "", nil, http.StatusBadGateway, err.Error()
	}
	if strings.HasPrefix(baseURL, "https://") {
		if s.Agents == nil || s.Agents.CA == nil {
			return "", nil, http.StatusBadGateway, "agent mTLS is not configured on the control plane"
		}
		return baseURL, s.Agents.clientFor(node.ID, baseURL), 0, ""
	}
	client = s.Client
	if client == nil {
		client = http.DefaultClient
	}
	return baseURL, client, 0, ""
}

// bufferedTwin returns a client like c for a buffered exec. The agent answers
// one only when the command exits, so the wait for response headers that bounds
// a call to the agent (30 s) would cut every buffered exec at 30 s whatever its
// limit; the twin has none, and the call's context bounds it. The twin of a
// client is made once and kept, so its connections are reused.
func (s *Server) bufferedTwin(c *http.Client) *http.Client {
	if c == nil {
		return c
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout == 0 {
		return c
	}
	if v, ok := s.bufferedTwins.Load(c); ok {
		return v.(*http.Client)
	}
	clone := tr.Clone()
	clone.ResponseHeaderTimeout = 0
	v, _ := s.bufferedTwins.LoadOrStore(c, &http.Client{Transport: clone, CheckRedirect: c.CheckRedirect})
	return v.(*http.Client)
}
