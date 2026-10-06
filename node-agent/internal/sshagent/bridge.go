// Package sshagent proxies the SSH agent protocol from guests to an agent on
// the host. Every path (the --ssh-agent-bridge socket and the host-vsock
// acceptors) goes through ServeConn, which forwards only identity listing and
// signing.
package sshagent

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
)

// Bridge serves ServeConn on a unix socket (--ssh-agent-bridge). It cannot
// tell guests apart: with Confirm set, a sign through it needs an approval
// issued without a sandbox id, which only Approver.GlobalApprovals allows.
type Bridge struct {
	ListenPath string
	// HostSock is the upstream agent (the operator's SSH_AUTH_SOCK). Empty or
	// missing → an agent with no keys.
	HostSock string
	Logger   *slog.Logger
	Confirm  *Approver // optional confirmation gate for SIGN_REQUEST

	ln     net.Listener
	mu     sync.Mutex
	closed bool
}

// Start listens on ListenPath and serves until Close.
func (b *Bridge) Start() error {
	if b.ListenPath == "" {
		return errors.New("ListenPath required")
	}
	_ = os.Remove(b.ListenPath)
	ln, err := net.Listen("unix", b.ListenPath)
	if err != nil {
		return err
	}
	b.ln = ln
	go b.acceptLoop()
	return nil
}

func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.ln != nil {
		err := b.ln.Close()
		_ = os.Remove(b.ListenPath)
		return err
	}
	return nil
}

func (b *Bridge) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return
			}
			if b.Logger != nil {
				b.Logger.Warn("ssh-agent accept", "error", err)
			}
			continue
		}
		go ServeConn(conn, b.HostSock, "", b.Confirm, b.Logger)
	}
}

// ServeFakeAgentConn answers conn as an agent with no keys: an empty identity
// list, FAILURE for everything else. Tests use it as an upstream.
func ServeFakeAgentConn(conn net.Conn) {
	ServeConn(conn, "", "", nil, nil)
}

// RequestIdentities sends SSH_AGENTC_REQUEST_IDENTITIES and returns key count.
func RequestIdentities(conn net.Conn) (int, error) {
	req := []byte{0, 0, 0, 1, sshAgentCRequestIdentities}
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint32(hdr)
	if n < 5 || n > maxAgentMsg {
		return 0, errors.New("bad agent response length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, err
	}
	if body[0] != sshAgentIdentitiesAnswer {
		return 0, errors.New("unexpected agent message")
	}
	count := int(binary.BigEndian.Uint32(body[1:5]))
	return count, nil
}
