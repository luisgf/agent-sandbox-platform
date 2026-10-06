package api

import (
	"log/slog"
	"net/http"
	"time"
)

// RequestLog logs every request with its status, size and duration. Node-agent
// polls and heartbeats (every few seconds per node) and /healthz go to Debug, so
// the Info log shows what people and tools do.
func RequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || isNodeAgentPath(r.URL.Path) {
			level = slog.LevelDebug
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", status,
			"bytes", rec.bytes, "duration", time.Since(started), "remote", r.RemoteAddr)
	})
}

// statusRecorder notes the status and the body size. It keeps Flush, which the
// streamed exec needs, and Unwrap for http.ResponseController.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
