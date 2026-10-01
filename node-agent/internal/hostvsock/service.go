// Package hostvsock serves guest→host services (SSH agent + identity).
//
// Port map (guest dials CID 2):
//
//	26500 — reserved for host→guest pod-daemon (CH hybrid CONNECT; not served here)
//	26501 — SSH agent protocol byte-pump → per-sandbox HostSock / FakeAgent
//	26502 — identity HTTP (POST /v1/tokens/oidc)
//
// Two host listen paths:
//
//  1. Optional global AF_VSOCK (or lab UnixFactory under --host-vsock-dir).
//  2. Per-sandbox Cloud Hypervisor / Firecracker hybrid muxer sockets
//     `{vsockMuxer}_{port}` via AttachSandbox — required for CH guest→host.
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
	"strings"
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
// Note: Cloud Hypervisor hybrid vsock does NOT deliver guest→host connections
// to AF_VSOCK Listen; use AttachSandbox for CH.
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
	Factory     ListenerFactory
	SSHHostSock string             // legacy node-wide SSH_AUTH_SOCK; empty → FakeAgent (global listener)
	SSHConfirm  *sshagent.Approver // optional SignRequest confirmation gate
	// SSHRegistry optional per-sandbox upstream map (ADR-0007 phase 4).
	// When set, hybrid AttachSandbox ServeConn uses Registry.Lookup(sandboxID)
	// with no process-env fallback.
	SSHRegistry     *sshagent.Registry
	IdentityHandler http.Handler
	Logger          *slog.Logger

	// SkipGlobalListeners, when true, Start does not open Factory listeners.
	// Useful when only hybrid AttachSandbox paths are desired (tests / CH-only).
	SkipGlobalListeners bool

	mu      sync.Mutex
	sshLn   net.Listener
	idLn    net.Listener
	hybrids map[string]*sandboxHybrid
	closed  bool
}

// Start opens optional global listeners and serves until Close.
// Hybrid per-sandbox acceptors are added via AttachSandbox.
func (s *Service) Start() error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	s.mu.Lock()
	if s.hybrids == nil {
		s.hybrids = make(map[string]*sandboxHybrid)
	}
	s.closed = false
	s.mu.Unlock()

	if s.SkipGlobalListeners {
		s.Logger.Info("host vsock: hybrid-only mode (no global AF_VSOCK/unix listeners)",
			"ssh_agent_port", PortSSHAgent,
			"identity_port", PortIdentity,
			"guest_host_cid", HostCID,
		)
		return nil
	}

	if s.Factory == nil {
		s.Factory = AFVsockFactory{}
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

// Close stops global and all hybrid listeners.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closeAllHybridsLocked()
	var errs []error
	if s.sshLn != nil {
		errs = append(errs, s.sshLn.Close())
		s.sshLn = nil
	}
	if s.idLn != nil {
		errs = append(errs, s.idLn.Close())
		s.idLn = nil
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// acceptSSH serves the optional global listener (legacy node-wide HostSock).
func (s *Service) acceptSSH(ln net.Listener) {
	s.acceptSSHUpstream(ln, s.SSHHostSock, false /* scoped */)
}

// acceptSSHUpstream pumps SSH agent to hostSock. When scoped, empty/missing
// sock → FakeAgent (no SSH_AUTH_SOCK env fallback).
func (s *Service) acceptSSHUpstream(ln net.Listener, hostSock string, scoped bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed by Close() or DetachSandbox — exit quietly.
			if isListenerClosed(err) {
				return
			}
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
			if scoped {
				sshagent.ServeConnScoped(c, hostSock, s.SSHConfirm, s.Logger)
			} else {
				sshagent.ServeConnWithConfirm(c, hostSock, s.SSHConfirm, s.Logger)
			}
		}(conn)
	}
}

// resolveHybridSSHSock returns (hostSock, scoped) for a sandbox at Attach time.
// scoped=true → ServeConnScoped (no process SSH_AUTH_SOCK fallback).
func (s *Service) resolveHybridSSHSock(sandboxID string) (string, bool) {
	if s.SSHRegistry == nil {
		return s.SSHHostSock, false
	}
	if p, ok := s.SSHRegistry.Get(sandboxID); ok {
		return p, true
	}
	// Unbound: Lookup → Fallback (lab) or "" when template mode.
	return s.SSHRegistry.Lookup(sandboxID), true
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

func isListenerClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Some platforms still surface the historic Accept string.
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "listener closed")
}
