package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Loopback reports whether addr ("host:port") binds only the loopback interface.
// An empty host (":9100") listens on every interface, so it is not.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Serve binds addr and serves h on it until ctx ends. These endpoints (metrics,
// pprof) have no authentication of their own, so unless insecure is set only a
// loopback address is accepted; to reach one from another host, put an
// authenticating proxy in front of it. It returns once the listener is bound.
func Serve(ctx context.Context, name, addr string, insecure bool, h http.Handler) error {
	if !insecure && !Loopback(addr) {
		return fmt.Errorf("%s listen %q: not a loopback address, and this endpoint has no authentication (bind 127.0.0.1, or set the insecure flag for a lab)", name, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s listen %s: %w", name, addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	slog.Info(name+" listening", "addr", ln.Addr().String(), "loopback_only", !insecure)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error(name+" stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	return nil
}

// PprofHandler serves the runtime profiles under /debug/pprof/ on a mux of its own.
func PprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// Mux serves the registry at /metrics, for the listener a process may open just for it.
func (r *Registry) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	return mux
}
