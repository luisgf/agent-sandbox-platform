package hostvsock

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
)

// HybridGuestPath returns the host Unix socket Cloud Hypervisor / Firecracker
// expect for guest→host connections to destination port.
//
// Convention (CH docs/vsock.md, Firecracker docs/vsock.md):
//
//	{muxerUDS}_{port}
//
// Example: muxer /run/asp/vsock-sb1.sock, port 26501 → /run/asp/vsock-sb1.sock_26501
// Guest dials AF_VSOCK CID 2:port; the VMM connects to this UDS (or RSTs if absent).
func HybridGuestPath(muxerPath string, port uint32) string {
	return fmt.Sprintf("%s_%d", muxerPath, port)
}

// sandboxHybrid holds per-sandbox hybrid guest→host listeners.
type sandboxHybrid struct {
	muxerPath string
	sandboxID string
	hostSock  string // resolved upstream at Attach; empty + scoped → FakeAgent
	scoped    bool   // true → ServeConnScoped (no env SSH_AUTH_SOCK fallback)
	sshLn     net.Listener
	idLn      net.Listener
}

// AttachSandbox starts CH/Firecracker hybrid guest→host acceptors for the
// sandbox muxer path: {muxerPath}_26501 (SSH agent) and {muxerPath}_26502
// (identity HTTP). Safe to call before or after the VMM binds the muxer UDS
// itself — those are distinct paths.
//
// Idempotent: re-attach with the same id replaces previous listeners.
// Identity requests on {muxerPath}_26502 are bound to sandboxID, so a muxer
// path that another sandbox holds is refused.
func (s *Service) AttachSandbox(sandboxID, muxerPath string) error {
	if sandboxID == "" {
		return fmt.Errorf("sandboxID required")
	}
	if muxerPath == "" {
		return fmt.Errorf("muxerPath required")
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}

	// Held until the new listeners are registered: the check below must not
	// race another attach for the same path.
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if holder := s.muxerHolder(muxerPath); holder != "" && holder != sandboxID {
		return fmt.Errorf("muxer %s is attached to sandbox %s", muxerPath, holder)
	}

	sshPath := HybridGuestPath(muxerPath, PortSSHAgent)
	idPath := HybridGuestPath(muxerPath, PortIdentity)

	sshLn, err := listenUnix(sshPath)
	if err != nil {
		return fmt.Errorf("hybrid listen ssh %s: %w", sshPath, err)
	}
	idLn, err := listenUnix(idPath)
	if err != nil {
		_ = sshLn.Close()
		_ = os.Remove(sshPath)
		return fmt.Errorf("hybrid listen identity %s: %w", idPath, err)
	}

	hostSock, scoped := s.resolveHybridSSHSock(sandboxID)
	h := &sandboxHybrid{
		muxerPath: muxerPath,
		sandboxID: sandboxID,
		hostSock:  hostSock,
		scoped:    scoped,
		sshLn:     sshLn,
		idLn:      idLn,
	}

	s.mu.Lock()
	if s.hybrids == nil {
		s.hybrids = make(map[string]*sandboxHybrid)
	}
	if old, ok := s.hybrids[sandboxID]; ok {
		closeHybrid(old)
	}
	s.hybrids[sandboxID] = h
	closed := s.closed
	s.mu.Unlock()

	if closed {
		closeHybrid(h)
		s.mu.Lock()
		delete(s.hybrids, sandboxID)
		s.mu.Unlock()
		return fmt.Errorf("hostvsock service closed")
	}

	go s.acceptSSHUpstream(sshLn, hostSock, scoped)
	if s.IdentityHandler != nil {
		go s.serveIdentity(idLn, sandboxID)
	} else {
		go s.acceptDrain(idLn, "identity-hybrid")
	}

	s.Logger.Info("hybrid guest→host acceptors attached",
		"sandbox_id", sandboxID,
		"muxer", muxerPath,
		"ssh_path", sshPath,
		"identity_path", idPath,
		"ssh_host_sock", hostSock,
		"ssh_scoped", scoped,
	)
	return nil
}

// DetachSandbox stops hybrid acceptors for a sandbox and removes their sockets.
func (s *Service) DetachSandbox(sandboxID string) {
	s.mu.Lock()
	h, ok := s.hybrids[sandboxID]
	if ok {
		delete(s.hybrids, sandboxID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	closeHybrid(h)
	if s.Logger != nil {
		s.Logger.Info("hybrid guest→host acceptors detached", "sandbox_id", sandboxID, "muxer", h.muxerPath)
	}
}

// muxerHolder returns the sandbox attached to muxerPath, or "".
func (s *Service) muxerHolder(muxerPath string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, h := range s.hybrids {
		if filepath.Clean(h.muxerPath) == filepath.Clean(muxerPath) {
			return id
		}
	}
	return ""
}

func closeHybrid(h *sandboxHybrid) {
	if h == nil {
		return
	}
	if h.sshLn != nil {
		_ = h.sshLn.Close()
		_ = os.Remove(HybridGuestPath(h.muxerPath, PortSSHAgent))
	}
	if h.idLn != nil {
		_ = h.idLn.Close()
		_ = os.Remove(HybridGuestPath(h.muxerPath, PortIdentity))
	}
}

func listenUnix(path string) (net.Listener, error) {
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

func (s *Service) closeAllHybridsLocked() {
	for id, h := range s.hybrids {
		closeHybrid(h)
		delete(s.hybrids, id)
	}
}

// HostSockFor returns the resolved SSH upstream for an attached sandbox (tests).
func (s *Service) HostSockFor(sandboxID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hybrids[sandboxID]
	if !ok || h == nil {
		return "", false
	}
	return h.hostSock, true
}
