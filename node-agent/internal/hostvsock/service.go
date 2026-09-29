// Package hostvsock serves guest→host services over AF_VSOCK (host CID 2).
//
// Port map (guest dials CID 2):
//
//	26500 — reserved for host→guest pod-daemon (CH hybrid CONNECT; not served here)
//	26501 — SSH agent protocol byte-pump → SSH_AUTH_SOCK / FakeAgent
//	26502 — identity HTTP (POST /v1/tokens/oidc)
package hostvsock

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/mdlayher/vsock"
)

const (
	// PortSSHAgent is the guest→host SSH agent pump port.
	PortSSHAgent uint32 = 26501
	// PortIdentity is the guest→host identity HTTP port.
	PortIdentity uint32 = 26502
	// HostCID is the vsock CID guests use to reach the host/hypervisor.
	HostCID uint32 = 2
)

// ListenerFactory opens a listener for a vsock port. Tests inject UnixFactory.
type ListenerFactory interface {
	Listen(port uint32) (net.Listener, error)
}

// AFVsockFactory listens on Linux AF_VSOCK (requires /dev/vsock).
type AFVsockFactory struct{}

func (AFVsockFactory) Listen(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}

// UnixFactory listens on Dir/host-vsock-{port}.sock for lab/tests without /dev/vsock.
type UnixFactory struct {
	Dir string
}

func (f UnixFactory) Listen(port uint32) (net.Listener, error) {
	if f.Dir == "" {
		return nil, errors.New("UnixFactory.Dir required")
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(f.Dir, fmt.Sprintf("host-vsock-%d.sock", port))
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

// PathFor returns the unix path UnixFactory would bind for port (tests/docs).
func (f UnixFactory) PathFor(port uint32) string {
	return filepath.Join(f.Dir, fmt.Sprintf("host-vsock-%d.sock", port))
}

// Service accepts guest connections on SSH + identity ports.
type Service struct {
	Factory         ListenerFactory
	SSHHostSock     string // SSH_AUTH_SOCK path; empty → FakeAgent
	SSHConfirm      *sshagent.Approver // optional SignRequest confirmation gate
	IdentityHandler http.Handler
	Logger          *slog.Logger

	mu     sync.Mutex
	sshLn  net.Listener
	idLn   net.Listener
	closed bool
}

// Start opens listeners and serves until Close.
func (s *Service) Start() error {
	if s.Factory == nil {
		s.Factory = AFVsockFactory{}
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	sshLn, err := s.Factory.Listen(PortSSHAgent)
	if err != nil {
		return fmt.Errorf("listen ssh-agent port %d: %w", PortSSHAgent, err)
	}
	idLn, err := s.Factory.Listen(PortIdentity)
	if err != nil {
		_ = sshLn.Close()
		return fmt.Errorf("listen identity port %d: %w", PortIdentity, err)
	}
	s.mu.Lock()
	s.sshLn = sshLn
	s.idLn = idLn
	s.mu.Unlock()

	go s.acceptSSH(sshLn)
	if s.IdentityHandler != nil {
		go s.serveIdentity(idLn)
	} else {
		go s.acceptDrain(idLn, "identity")
	}
	s.Logger.Info("host vsock services listening",
		"ssh_agent_port", PortSSHAgent,
		"identity_port", PortIdentity,
		"guest_host_cid", HostCID,
	)
	return nil
}

// Close stops both listeners.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var errs []error
	if s.sshLn != nil {
		errs = append(errs, s.sshLn.Close())
	}
	if s.idLn != nil {
		errs = append(errs, s.idLn.Close())
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) acceptSSH(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.Logger.Warn("host-vsock ssh accept", "error", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			sshagent.ServeConnWithConfirm(c, s.SSHHostSock, s.SSHConfirm, s.Logger)
		}(conn)
	}
}

func (s *Service) serveIdentity(ln net.Listener) {
	srv := &http.Server{
		Handler:           s.IdentityHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			s.Logger.Warn("host-vsock identity serve", "error", err)
		}
	}
}

func (s *Service) acceptDrain(ln net.Listener, name string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}(conn)
		s.Logger.Warn("host-vsock: no identity handler; draining", "service", name)
	}
}
