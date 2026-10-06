// Package agenttest provides a fake upstream SSH agent for tests of the
// node-agent's agent proxy. It records every message type it receives, so a
// test can tell whether the proxy forwarded a request or refused it locally.
package agenttest

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

// Agent protocol message types (draft-miller-ssh-agent).
const (
	Failure             = 5
	Success             = 6
	RequestIdentities   = 11
	IdentitiesAnswer    = 12
	SignRequest         = 13
	SignResponse        = 14
	AddIdentity         = 17
	RemoveIdentity      = 18
	RemoveAllIdentities = 19
	Lock                = 22
	Unlock              = 23
	AddIDConstrained    = 25
	Extension           = 27
)

// Upstream is a fake SSH agent on a unix socket. It answers
// REQUEST_IDENTITIES with no keys, SIGN_REQUEST with a dummy SIGN_RESPONSE
// and any other message with SUCCESS: a reply of SUCCESS proves that the
// proxy in front of it forwarded the request.
type Upstream struct {
	Path string

	mu   sync.Mutex
	seen []byte
}

// Start listens on path until the test ends.
func Start(t testing.TB, path string) *Upstream {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	u := &Upstream{Path: path}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(c)
		}
	}()
	return u
}

// Seen returns the type of every message received so far, in order.
func (u *Upstream) Seen() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.seen...)
}

func (u *Upstream) serve(c net.Conn) {
	defer c.Close()
	for {
		msg, err := ReadMsg(c)
		if err != nil {
			return
		}
		u.mu.Lock()
		u.seen = append(u.seen, msg[0])
		u.mu.Unlock()
		var reply []byte
		switch msg[0] {
		case RequestIdentities:
			reply = []byte{IdentitiesAnswer, 0, 0, 0, 0}
		case SignRequest:
			reply = append([]byte{SignResponse}, str([]byte("dummy-signature"))...)
		default:
			reply = []byte{Success}
		}
		if _, err := c.Write(Frame(reply)); err != nil {
			return
		}
	}
}

// Frame prefixes an agent message (type byte first) with its length.
func Frame(msg []byte) []byte {
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out, uint32(len(msg)))
	copy(out[4:], msg)
	return out
}

// ReadMsg reads one length-prefixed agent message.
func ReadMsg(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > 1<<20 {
		return nil, errors.New("bad agent message length")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// Request sends one message and returns the type of the reply.
func Request(c net.Conn, msg []byte) (byte, error) {
	if _, err := c.Write(Frame(msg)); err != nil {
		return 0, err
	}
	reply, err := ReadMsg(c)
	if err != nil {
		return 0, err
	}
	return reply[0], nil
}

// SignRequestMsg is a SIGN_REQUEST for an empty key blob over the data "x".
func SignRequestMsg() []byte {
	msg := []byte{SignRequest}
	msg = append(msg, str(nil)...)
	msg = append(msg, str([]byte("x"))...)
	return append(msg, 0, 0, 0, 0) // flags
}

// ExtensionMsg is an EXTENSION request for name with no contents.
func ExtensionMsg(name string) []byte {
	return append([]byte{Extension}, str([]byte(name))...)
}

func str(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}
