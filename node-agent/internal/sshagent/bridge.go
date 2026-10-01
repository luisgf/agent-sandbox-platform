// Package sshagent proxies the SSH agent protocol over a Unix socket.
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

const (
	sshAgentFailure            = 5
	sshAgentCRequestIdentities = 11
	sshAgentIdentitiesAnswer   = 12
)

// Bridge accepts connections on ListenPath and byte-pumps to HostSock (SSH_AUTH_SOCK).
// If HostSock is empty or missing, a FakeAgent (no keys) answers locally.
// When Confirm is non-nil, SignRequest requires a one-shot approval (see Approver).
type Bridge struct {
	ListenPath string
	HostSock   string
	Logger     *slog.Logger
	Confirm    *Approver // optional confirmation gate for SignRequest

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
		go b.handle(conn)
	}
}

func (b *Bridge) handle(client net.Conn) {
	hostSock := b.HostSock
	if hostSock == "" {
		hostSock = os.Getenv("SSH_AUTH_SOCK")
	}
	if b.Confirm != nil {
		ConfirmingServeConn(client, hostSock, b.Confirm, b.Logger)
		return
	}
	defer client.Close()
	if hostSock != "" {
		if _, err := os.Stat(hostSock); err == nil {
			upstream, err := net.Dial("unix", hostSock)
			if err == nil {
				defer upstream.Close()
				pump(client, upstream)
				return
			}
			if b.Logger != nil {
				b.Logger.Warn("ssh-agent dial host sock failed; using FakeAgent", "error", err)
			}
		}
	}
	// FakeAgent: respond with empty identities / failure.
	serveFakeAgent(client)
}

// ServeConn handles one SSH agent client: byte-pumps to hostSock when present,
// otherwise FakeAgent. Used by Bridge and hostvsock guest→host port 26501.
// Empty hostSock falls back to process SSH_AUTH_SOCK (legacy node-wide bridge).
func ServeConn(client net.Conn, hostSock string) {
	ServeConnWithConfirm(client, hostSock, nil, nil)
}

// ServeConnWithConfirm is ServeConn with an optional SignRequest confirmation gate.
// Empty hostSock falls back to process SSH_AUTH_SOCK (legacy).
func ServeConnWithConfirm(client net.Conn, hostSock string, confirm *Approver, logger *slog.Logger) {
	serveConn(client, hostSock, confirm, logger, true)
}

// ServeConnScoped is ServeConn for per-sandbox upstreams (ADR-0007 phase 4).
// Empty or missing hostSock → FakeAgent; never falls back to process SSH_AUTH_SOCK
// (that would re-share the operator agent across sandboxes/users).
func ServeConnScoped(client net.Conn, hostSock string, confirm *Approver, logger *slog.Logger) {
	serveConn(client, hostSock, confirm, logger, false)
}

func serveConn(client net.Conn, hostSock string, confirm *Approver, logger *slog.Logger, allowEnvFallback bool) {
	if confirm != nil {
		ConfirmingServeConnOpts(client, hostSock, confirm, logger, allowEnvFallback)
		return
	}
	defer client.Close()
	if hostSock == "" && allowEnvFallback {
		hostSock = os.Getenv("SSH_AUTH_SOCK")
	}
	if hostSock != "" {
		if _, err := os.Stat(hostSock); err == nil {
			upstream, err := net.Dial("unix", hostSock)
			if err == nil {
				defer upstream.Close()
				pump(client, upstream)
				return
			}
			if logger != nil {
				logger.Warn("ssh-agent dial host sock failed; using FakeAgent", "error", err, "host_sock", hostSock)
			}
		}
	}
	serveFakeAgent(client)
}

func pump(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// serveFakeAgent implements a minimal agent with no identities.
func serveFakeAgent(conn net.Conn) {
	buf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(buf)
		if n == 0 || n > 1<<20 {
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		msgType := payload[0]
		var resp []byte
		switch msgType {
		case sshAgentCRequestIdentities:
			// type(1) + count(4) = 5 bytes body
			resp = []byte{sshAgentIdentitiesAnswer, 0, 0, 0, 0}
		default:
			resp = []byte{sshAgentFailure}
		}
		out := make([]byte, 4+len(resp))
		binary.BigEndian.PutUint32(out, uint32(len(resp)))
		copy(out[4:], resp)
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

// ServeFakeAgentConn exposes FakeAgent for tests (e.g. net.Pipe).
func ServeFakeAgentConn(conn net.Conn) {
	serveFakeAgent(conn)
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
	if n < 5 || n > 1<<20 {
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
