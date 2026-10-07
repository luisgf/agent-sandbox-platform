package execproxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"time"
)

// The control plane reaches a node agent on another host through --agent-tls-listen:
// TLS with the node certificate from enrollment, and only the control plane's client
// identity accepted. These names mirror control-plane/internal/pki.
const (
	ControlPlaneCN = "asp-control-plane"
	OUControlPlane = "control-plane"
)

// ErrNoServerAuth: the node certificate predates the server-auth profile.
var ErrNoServerAuth = errors.New("node certificate cannot serve TLS (no server-auth usage); re-enroll with --enroll or rotate-cert")

// RemoteHandler serves what the control plane needs from another host: exec,
// exec/stdin and the node's self-checks. Operator routes (ssh-agent approve,
// egress-check) stay on the loopback listener only.
func (s *Server) RemoteHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("POST /v1/internal/exec", s.handleExec)
	mux.HandleFunc("POST /v1/internal/exec/stdin", s.handleExecStdin)
	mux.HandleFunc("GET /v1/internal/doctor", s.handleDoctor)
	return mux
}

// MTLSConfig returns the TLS config of the control-plane listener: the node
// certificate as server certificate, client certificates required and verified
// against the enrollment CA, and only the control plane's identity accepted. A
// certificate of another node from the same CA cannot call this node's exec API.
func MTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load node certificate: %w", err)
	}
	return MTLSConfigFor(func() *tls.Certificate { return &pair }, caFile)
}

// MTLSConfigFor is MTLSConfig with the server certificate read from cert at
// each handshake, so a renewed node certificate is served without a restart.
// The certificate in use when the config is built must allow server auth.
func MTLSConfigFor(cert func() *tls.Certificate, caFile string) (*tls.Config, error) {
	cur := cert()
	if cur == nil || len(cur.Certificate) == 0 {
		return nil, errors.New("no node certificate")
	}
	leaf := cur.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(cur.Certificate[0]); err != nil {
			return nil, fmt.Errorf("parse node certificate: %w", err)
		}
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return nil, ErrNoServerAuth
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no CA certificates in %s", caFile)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert(), nil },
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client certificate required")
			}
			peer := cs.PeerCertificates[0]
			if peer.Subject.CommonName != ControlPlaneCN || !slices.Contains(peer.Subject.OrganizationalUnit, OUControlPlane) {
				return fmt.Errorf("client %q is not the control plane", peer.Subject.CommonName)
			}
			return nil
		},
	}, nil
}

// ListenAndServeTLS binds addr and serves h over TLS until the returned server closes.
func ListenAndServeTLS(addr string, h http.Handler, tlsCfg *tls.Config) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		_ = srv.ServeTLS(ln, "", "")
	}()
	return srv, ln, nil
}
