package poddaemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
)

// DefaultGuestPort is the vsock/TCP port where guest pod-daemon listens for HTTP.
const DefaultGuestPort = 26500

// Dialer opens a single connection to pod-daemon. Tests inject fakes without /dev/vsock.
type Dialer interface {
	Dial(ctx context.Context) (net.Conn, error)
}

// UnixDialer dials a host Unix domain socket (dry-run / lab).
type UnixDialer struct {
	Path string
}

func (d *UnixDialer) Dial(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", d.Path)
}

// HybridVsockDialer dials Cloud Hypervisor's host-side vsock multiplexer UDS
// using the Firecracker/CH CONNECT protocol:
//
//	connect to SocketPath → write "CONNECT <port>\n" → read "OK <host_port>\n" → stream is ready.
//
// This is the productive host→guest path for CH (AF_VSOCK from host userspace is not used).
type HybridVsockDialer struct {
	SocketPath string // e.g. /run/asp/vsock-{sandbox}.sock
	Port       uint32 // guest AF_VSOCK listen port (DefaultGuestPort)
}

func (d *HybridVsockDialer) Dial(ctx context.Context) (net.Conn, error) {
	port := d.Port
	if port == 0 {
		port = DefaultGuestPort
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", d.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("hybrid vsock unix dial %s: %w", d.SocketPath, err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hybrid vsock ACK: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(strings.ToUpper(line), "OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("hybrid vsock ACK unexpected: %q", line)
	}

	// Prefer buffered leftovers (none expected after OK line) via multi-reader if needed.
	if br.Buffered() > 0 {
		leftover, _ := io.ReadAll(io.LimitReader(br, int64(br.Buffered())))
		if len(leftover) > 0 {
			conn = &prefixConn{Conn: conn, prefix: leftover}
		}
	}
	_ = conn.SetDeadline(time.Time{}) // clear; HTTP client manages timeouts
	return conn, nil
}

// AFVsockDialer dials guest CID:port via Linux AF_VSOCK (/dev/vsock).
// Useful when the host exposes guest CIDs directly; Cloud Hypervisor's hybrid
// muxer path is HybridVsockDialer instead.
type AFVsockDialer struct {
	CID  uint32
	Port uint32
}

func (d *AFVsockDialer) Dial(ctx context.Context) (net.Conn, error) {
	port := d.Port
	if port == 0 {
		port = DefaultGuestPort
	}
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := vsock.Dial(d.CID, port, nil)
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.c, r.err
	}
}

// prefixConn prepends unread bytes before reading from the underlying conn.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// ParseOKPort extracts the host-side port from an "OK <port>" ACK (tests/helpers).
func ParseOKPort(line string) (uint32, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.EqualFold(fields[0], "OK") {
		return 0, fmt.Errorf("not an OK ack: %q", line)
	}
	v, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}
