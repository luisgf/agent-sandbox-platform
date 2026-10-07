package vmm

import (
	"net"
	"strings"
	"sync"
	"time"
)

// ConsoleBytes is how much of a guest's serial console the agent keeps.
const ConsoleBytes = 32 << 10

// Console keeps the last bytes a guest wrote to its serial console, in memory.
// It is bounded on purpose: a guest can write to its console as fast as it likes,
// and the agent must not turn that into a file that fills the host's disk (or
// its tmpfs). What it keeps is for diagnosis: why a guest never answered.
type Console struct {
	max  int
	stop chan struct{}
	once sync.Once

	mu   sync.Mutex
	buf  []byte
	conn net.Conn
}

// NewConsole keeps up to max bytes (ConsoleBytes when max is not positive).
func NewConsole(max int) *Console {
	if max <= 0 {
		max = ConsoleBytes
	}
	return &Console{max: max, stop: make(chan struct{})}
}

// Write appends to the kept bytes, dropping the oldest ones beyond the bound.
func (c *Console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) >= c.max {
		c.buf = append(c.buf[:0], p[len(p)-c.max:]...)
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	if over := len(c.buf) - c.max; over > 0 {
		c.buf = append(c.buf[:0], c.buf[over:]...)
	}
	return len(p), nil
}

// Tail returns up to the last max bytes kept (all of them when max is not
// positive), starting at a line start when the cut falls inside a line. The
// console's own control sequences stay in: they are text to a log.
func (c *Console) Tail(max int) string {
	c.mu.Lock()
	b := append([]byte(nil), c.buf...)
	c.mu.Unlock()
	if max > 0 && len(b) > max {
		b = b[len(b)-max:]
		if i := strings.IndexByte(string(b), '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	return strings.ToValidUTF8(string(b), "?")
}

// Attach connects to the unix socket Cloud Hypervisor serves the guest's console
// on and copies what it reads into the Console until the guest or Close ends the
// connection. The socket exists once the VM is created; Attach retries until
// timeout. It returns at once: the connecting and copying run in a goroutine.
func (c *Console) Attach(path string, timeout time.Duration) {
	go func() {
		deadline := time.Now().Add(timeout)
		var conn net.Conn
		for {
			var err error
			if conn, err = net.DialTimeout("unix", path, time.Second); err == nil {
				break
			}
			if !time.Now().Before(deadline) {
				return
			}
			select {
			case <-time.After(50 * time.Millisecond):
			case <-c.stop:
				return
			}
		}
		c.mu.Lock()
		select {
		case <-c.stop: // closed while connecting
			c.mu.Unlock()
			_ = conn.Close()
			return
		default:
		}
		c.conn = conn
		c.mu.Unlock()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_, _ = c.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

// Close ends the capture. What was kept can still be read.
func (c *Console) Close() {
	c.once.Do(func() {
		close(c.stop)
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}
