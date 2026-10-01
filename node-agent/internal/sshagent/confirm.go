package sshagent

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// SSH agent protocol message types (OpenSSH).
const (
	sshAgentSignRequest = 13 // SSH2_AGENTC_SIGN_REQUEST
)

// Approver holds one-shot approvals for SSH agent SignRequest messages.
// An approval is consumed on the next SignRequest (or expires by TTL).
type Approver struct {
	mu      sync.Mutex
	pending map[string]time.Time // token -> expiry
	// AllowAny, when true with a valid pending entry, consumes the oldest/any token.
	DefaultTTL time.Duration
}

// NewApprover creates an empty Approver with default TTL (30s if unset).
func NewApprover(ttl time.Duration) *Approver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Approver{pending: make(map[string]time.Time), DefaultTTL: ttl}
}

// Approve registers a one-shot approval token valid for ttl (or DefaultTTL).
func (a *Approver) Approve(ttl time.Duration) (token string, expires time.Time) {
	if a == nil {
		return "", time.Time{}
	}
	if ttl <= 0 {
		ttl = a.DefaultTTL
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	token = hex.EncodeToString(b[:])
	expires = time.Now().UTC().Add(ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(time.Now().UTC())
	a.pending[token] = expires
	return token, expires
}

// PendingCount returns non-expired approvals (tests).
func (a *Approver) PendingCount() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(time.Now().UTC())
	return len(a.pending)
}

// Consume removes one valid approval. Returns false if none available (auto-deny).
func (a *Approver) Consume() bool {
	if a == nil {
		return false
	}
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(now)
	for tok, exp := range a.pending {
		if exp.After(now) {
			delete(a.pending, tok)
			return true
		}
	}
	return false
}

func (a *Approver) gcLocked(now time.Time) {
	for tok, exp := range a.pending {
		if !exp.After(now) {
			delete(a.pending, tok)
		}
	}
}

// ConfirmingServeConn proxies the SSH agent protocol to hostSock, but denies
// SignRequest messages unless Approver.Consume succeeds (one-shot TTL approval).
// Identities and other non-sign messages are forwarded (or FakeAgent if no host).
// Empty hostSock falls back to process SSH_AUTH_SOCK (legacy).
func ConfirmingServeConn(client net.Conn, hostSock string, approver *Approver, logger *slog.Logger) {
	ConfirmingServeConnOpts(client, hostSock, approver, logger, true)
}

// ConfirmingServeConnOpts is ConfirmingServeConn with explicit env-fallback control.
// allowEnvFallback=false is required for per-sandbox scoped upstreams (ADR-0007 §4).
func ConfirmingServeConnOpts(client net.Conn, hostSock string, approver *Approver, logger *slog.Logger, allowEnvFallback bool) {
	defer client.Close()
	if hostSock == "" && allowEnvFallback {
		hostSock = os.Getenv("SSH_AUTH_SOCK")
	}
	var upstream net.Conn
	if hostSock != "" {
		if _, err := os.Stat(hostSock); err == nil {
			u, err := net.Dial("unix", hostSock)
			if err == nil {
				upstream = u
				defer upstream.Close()
			} else if logger != nil {
				logger.Warn("ssh-agent confirm dial host failed; FakeAgent for non-sign", "error", err)
			}
		}
	}
	buf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(buf)
		if n == 0 || n > 1<<20 {
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(client, payload); err != nil {
			return
		}
		msgType := payload[0]
		if msgType == sshAgentSignRequest {
			if approver == nil || !approver.Consume() {
				if logger != nil {
					logger.Warn("ssh-agent SignRequest denied (no approval)")
				}
				if err := writeAgentFailure(client); err != nil {
					return
				}
				continue
			}
			if logger != nil {
				logger.Info("ssh-agent SignRequest approved (one-shot consumed)")
			}
		}
		if upstream == nil {
			// No host agent: FakeAgent responses for identities; failure for everything else including signed.
			if msgType == sshAgentCRequestIdentities {
				resp := []byte{sshAgentIdentitiesAnswer, 0, 0, 0, 0}
				if err := writeAgentMsg(client, resp); err != nil {
					return
				}
				continue
			}
			if err := writeAgentFailure(client); err != nil {
				return
			}
			continue
		}
		// Forward request to upstream and relay one response message.
		out := make([]byte, 4+len(payload))
		binary.BigEndian.PutUint32(out, uint32(len(payload)))
		copy(out[4:], payload)
		if _, err := upstream.Write(out); err != nil {
			return
		}
		rh := make([]byte, 4)
		if _, err := io.ReadFull(upstream, rh); err != nil {
			return
		}
		rn := binary.BigEndian.Uint32(rh)
		if rn == 0 || rn > 1<<20 {
			return
		}
		rbody := make([]byte, rn)
		if _, err := io.ReadFull(upstream, rbody); err != nil {
			return
		}
		resp := make([]byte, 4+len(rbody))
		binary.BigEndian.PutUint32(resp, uint32(len(rbody)))
		copy(resp[4:], rbody)
		if _, err := client.Write(resp); err != nil {
			return
		}
	}
}

func writeAgentFailure(conn net.Conn) error {
	return writeAgentMsg(conn, []byte{sshAgentFailure})
}

func writeAgentMsg(conn net.Conn, body []byte) error {
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	_, err := conn.Write(out)
	return err
}

// SignRequestRaw builds a minimal SSH2_AGENTC_SIGN_REQUEST for tests (key blob empty, data "x", flags 0).
func SignRequestRaw() []byte {
	// type(1) + keyblob_len(4)+keyblob + data_len(4)+data + flags(4)
	data := []byte("x")
	body := make([]byte, 1+4+0+4+len(data)+4)
	body[0] = sshAgentSignRequest
	binary.BigEndian.PutUint32(body[1:5], 0) // empty key blob
	binary.BigEndian.PutUint32(body[5:9], uint32(len(data)))
	copy(body[9:], data)
	binary.BigEndian.PutUint32(body[9+len(data):], 0)
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	return out
}

// ReadAgentReply reads one length-prefixed agent reply; returns message type byte.
func ReadAgentReply(conn net.Conn) (byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint32(hdr)
	if n == 0 || n > 1<<20 {
		return 0, errors.New("bad reply length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, err
	}
	return body[0], nil
}
