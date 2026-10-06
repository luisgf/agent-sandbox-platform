package poddaemon

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownSandbox: no endpoint is registered for the sandbox (it is not
// running on this node) and there is no fallback.
var ErrUnknownSandbox = errors.New("no pod-daemon endpoint for sandbox")

// Transport names for Endpoint.Mode.
const (
	ModeUnix    = "unix"
	ModeHybrid  = "hybrid" // CH UDS + CONNECT
	ModeAFVsock = "afvsock"
)

// Endpoint describes how to reach pod-daemon for one sandbox.
type Endpoint struct {
	Mode      string // ModeUnix | ModeHybrid | ModeAFVsock
	UnixPath  string
	VsockPath string // host CH vsock muxer UDS
	CID       uint32
	Port      uint32
}

// Registry maps sandbox_id → dial endpoint. Exec looks up by sandbox_id. It
// also keeps one Client per sandbox, so the connections to that guest's
// pod-daemon are reused across execs and stdin posts.
type Registry struct {
	mu       sync.RWMutex
	byID     map[string]Endpoint
	clients  map[string]*Client
	Fallback Dialer // used when sandbox unknown (dry-run shared sock)
	fallback *Client
}

func NewRegistry(fallback Dialer) *Registry {
	return &Registry{
		byID:     make(map[string]Endpoint),
		Fallback: fallback,
	}
}

func (r *Registry) Register(sandboxID string, ep Endpoint) {
	if r == nil || sandboxID == "" {
		return
	}
	if ep.Port == 0 {
		ep.Port = DefaultGuestPort
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = make(map[string]Endpoint)
	}
	if old, ok := r.byID[sandboxID]; ok && old != ep {
		r.dropClientLocked(sandboxID)
	}
	r.byID[sandboxID] = ep
}

func (r *Registry) Unregister(sandboxID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, sandboxID)
	r.dropClientLocked(sandboxID)
}

// dropClientLocked closes the sandbox's idle connections and forgets its client.
func (r *Registry) dropClientLocked(sandboxID string) {
	if c, ok := r.clients[sandboxID]; ok {
		c.CloseIdleConnections()
		delete(r.clients, sandboxID)
	}
}

func (r *Registry) Lookup(sandboxID string) (Endpoint, bool) {
	if r == nil {
		return Endpoint{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep, ok := r.byID[sandboxID]
	return ep, ok
}

// DialerFor returns a Dialer for the sandbox, or Fallback, or an error.
func (r *Registry) DialerFor(sandboxID string) (Dialer, error) {
	if r == nil {
		return nil, fmt.Errorf("poddaemon registry not configured")
	}
	if ep, ok := r.Lookup(sandboxID); ok {
		d, err := DialerFromEndpoint(ep)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	if r.Fallback != nil {
		return r.Fallback, nil
	}
	return nil, fmt.Errorf("%w %s", ErrUnknownSandbox, sandboxID)
}

// ClientFor returns the sandbox's client, created on first use and reused after
// that so its connections to pod-daemon stay open between calls.
func (r *Registry) ClientFor(sandboxID string) (*Client, error) {
	if r == nil {
		return nil, fmt.Errorf("poddaemon registry not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep, ok := r.byID[sandboxID]; ok {
		if c, ok := r.clients[sandboxID]; ok {
			return c, nil
		}
		d, err := DialerFromEndpoint(ep)
		if err != nil {
			return nil, err
		}
		c := NewClientFromDialer(d)
		if r.clients == nil {
			r.clients = make(map[string]*Client)
		}
		r.clients[sandboxID] = c
		return c, nil
	}
	if r.Fallback != nil {
		if r.fallback == nil {
			r.fallback = NewClientFromDialer(r.Fallback)
		}
		return r.fallback, nil
	}
	return nil, fmt.Errorf("%w %s", ErrUnknownSandbox, sandboxID)
}

// DialerFromEndpoint constructs the concrete Dialer for an Endpoint.
func DialerFromEndpoint(ep Endpoint) (Dialer, error) {
	port := ep.Port
	if port == 0 {
		port = DefaultGuestPort
	}
	switch ep.Mode {
	case ModeUnix, "":
		if ep.UnixPath == "" {
			return nil, fmt.Errorf("unix endpoint missing path")
		}
		return &UnixDialer{Path: ep.UnixPath}, nil
	case ModeHybrid:
		if ep.VsockPath == "" {
			return nil, fmt.Errorf("hybrid endpoint missing vsock path")
		}
		return &HybridVsockDialer{SocketPath: ep.VsockPath, Port: port}, nil
	case ModeAFVsock:
		if ep.CID < 3 {
			return nil, fmt.Errorf("afvsock endpoint needs CID >= 3, got %d", ep.CID)
		}
		return &AFVsockDialer{CID: ep.CID, Port: port}, nil
	default:
		return nil, fmt.Errorf("unknown pod-daemon transport %q", ep.Mode)
	}
}

// Len returns registered sandbox count (tests).
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}
