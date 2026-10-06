package sshagent

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
)

// Agent protocol message types (draft-miller-ssh-agent).
const (
	sshAgentFailure            = 5
	sshAgentCRequestIdentities = 11
	sshAgentIdentitiesAnswer   = 12
	sshAgentSignRequest        = 13
	sshAgentCExtension         = 27
)

// maxAgentMsg bounds one agent message in either direction.
const maxAgentMsg = 1 << 20

// refusedNames names the requests a guest is most likely to try, for the log.
var refusedNames = map[byte]string{
	17: "add_identity",
	18: "remove_identity",
	19: "remove_all_identities",
	20: "add_smartcard_key",
	21: "remove_smartcard_key",
	22: "lock",
	23: "unlock",
	25: "add_id_constrained",
	26: "add_smartcard_key_constrained",
	27: "extension",
}

// ServeConn proxies one guest connection to the SSH agent at hostSock, one
// request and one reply at a time:
//
//   - REQUEST_IDENTITIES is forwarded.
//   - SIGN_REQUEST is forwarded when confirm is nil, or when confirm holds an
//     approval for sandboxID (Approver.Consume); otherwise the guest gets
//     FAILURE.
//   - EXTENSION is forwarded only for "query".
//   - Anything else (ADD_IDENTITY, REMOVE_IDENTITY, REMOVE_ALL_IDENTITIES,
//     LOCK, UNLOCK, other extensions…) gets FAILURE and never reaches the
//     upstream agent: a guest must not add its own key to the operator's
//     agent, remove its keys or lock it.
//
// sandboxID is the sandbox the connection comes from, or "" for a listener
// that cannot tell guests apart (--ssh-agent-bridge, the global host-vsock
// listener). An empty, missing or unreachable hostSock behaves like an agent
// with no keys; ServeConn never falls back to the process's SSH_AUTH_SOCK.
// It closes client when done.
func ServeConn(client net.Conn, hostSock, sandboxID string, confirm *Approver, logger *slog.Logger) {
	defer client.Close()
	p := &proxy{sandboxID: sandboxID, confirm: confirm, logger: logger}
	if hostSock != "" {
		if _, err := os.Stat(hostSock); err == nil {
			up, err := net.Dial("unix", hostSock)
			if err == nil {
				defer up.Close()
				p.upstream = up
			} else {
				p.log(slog.LevelWarn, "ssh-agent: cannot reach the upstream agent; answering as an agent with no keys",
					"error", err, "host_sock", hostSock)
			}
		}
	}
	for {
		msg, err := readMsg(client)
		if err != nil {
			return
		}
		reply, err := p.handle(msg)
		if err != nil {
			return
		}
		if err := writeMsg(client, reply); err != nil {
			return
		}
	}
}

type proxy struct {
	upstream  net.Conn // nil: no agent, no keys
	sandboxID string
	confirm   *Approver
	logger    *slog.Logger
	refused   bool // a request was refused on this connection already
}

var failure = []byte{sshAgentFailure}

// handle answers one request, forwarding it upstream when allowed. An error
// means the upstream connection broke.
func (p *proxy) handle(msg []byte) ([]byte, error) {
	switch {
	case msg[0] == sshAgentCRequestIdentities:
		if p.upstream == nil {
			return []byte{sshAgentIdentitiesAnswer, 0, 0, 0, 0}, nil
		}
		return p.forward(msg)
	case msg[0] == sshAgentSignRequest:
		if !p.allowSign() || p.upstream == nil {
			return failure, nil
		}
		return p.forward(msg)
	case msg[0] == sshAgentCExtension && extensionName(msg) == "query":
		if p.upstream == nil {
			return failure, nil
		}
		return p.forward(msg)
	default:
		p.refuse(msg)
		return failure, nil
	}
}

func (p *proxy) forward(msg []byte) ([]byte, error) {
	if err := writeMsg(p.upstream, msg); err != nil {
		return nil, err
	}
	return readMsg(p.upstream)
}

// allowSign consumes an approval for the connection's sandbox when the
// confirmation gate is on.
func (p *proxy) allowSign() bool {
	if p.confirm == nil {
		return true
	}
	id, ok := p.confirm.consume(p.sandboxID)
	switch {
	case ok:
		p.log(slog.LevelInfo, "ssh-agent sign request approved (one-shot approval consumed)",
			"sandbox_id", p.sandboxID, "approval_id", id)
	case p.sandboxID == "" && !p.confirm.GlobalApprovals:
		p.log(slog.LevelWarn, "ssh-agent sign request denied: this listener cannot tell guests apart and approvals are per sandbox (--insecure-ssh-agent-global-approvals allows it; lab only)")
	default:
		p.log(slog.LevelWarn, "ssh-agent sign request denied: no approval for this sandbox", "sandbox_id", p.sandboxID)
	}
	return ok
}

// refuse logs a request that does not reach the upstream agent: the first one
// on the connection as a warning, the rest at debug level, so a guest cannot
// flood the log.
func (p *proxy) refuse(msg []byte) {
	level := slog.LevelWarn
	if p.refused {
		level = slog.LevelDebug
	}
	p.refused = true
	attrs := []any{"type", msg[0], "sandbox_id", p.sandboxID}
	if name, ok := refusedNames[msg[0]]; ok {
		attrs = append(attrs, "request", name)
	}
	if msg[0] == sshAgentCExtension {
		attrs = append(attrs, "extension", truncate(extensionName(msg), 64))
	}
	p.log(level, "ssh-agent request refused: only identity listing and signing reach the host agent", attrs...)
}

func (p *proxy) log(level slog.Level, msg string, attrs ...any) {
	if p.logger != nil {
		p.logger.Log(context.Background(), level, msg, attrs...)
	}
}

// extensionName returns the name of an EXTENSION request ("" if malformed).
func extensionName(msg []byte) string {
	if len(msg) < 5 {
		return ""
	}
	n := binary.BigEndian.Uint32(msg[1:5])
	if uint64(n) > uint64(len(msg)-5) {
		return ""
	}
	return string(msg[5 : 5+n])
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// readMsg reads one length-prefixed agent message (type byte first).
func readMsg(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxAgentMsg {
		return nil, errors.New("bad agent message length")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeMsg(w io.Writer, msg []byte) error {
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out, uint32(len(msg)))
	copy(out[4:], msg)
	_, err := w.Write(out)
	return err
}
