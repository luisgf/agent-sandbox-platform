package execproxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// levelLog records the level and message of every record it gets.
type levelLog struct {
	mu   sync.Mutex
	recs []string
}

func (l *levelLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r.Level.String()+" "+r.Message)
	l.mu.Unlock()
	return nil
}
func (l *levelLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelLog) WithGroup(string) slog.Handler      { return l }
func (l *levelLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.recs, "\n")
}

// A client that goes away (a `| head`, a Ctrl-C) ends its stream with a cancelled
// context or a reset connection: that is not an error of the agent, and it must
// not read as one in the log. A real failure still does.
func TestAClientThatLeftIsNotAnError(t *testing.T) {
	log := &levelLog{}
	s := &Server{Logger: slog.New(log)}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := httptest.NewRequest(http.MethodPost, "/v1/exec", nil).WithContext(gone)
	live := httptest.NewRequest(http.MethodPost, "/v1/exec", nil)

	s.logExecError(cancelled, "pod-daemon exec stream copy", errors.New("read: something"), "sandbox_id", "sb")
	s.logExecError(live, "pod-daemon exec stream copy", context.Canceled, "sandbox_id", "sb")
	s.logExecError(live, "pod-daemon exec stream copy", syscall.EPIPE, "sandbox_id", "sb")
	s.logExecError(live, "pod-daemon exec stream copy", syscall.ECONNRESET, "sandbox_id", "sb")
	got := log.all()
	if strings.Contains(got, "ERROR") || strings.Count(got, "INFO pod-daemon exec stream copy: the client went away") != 4 {
		t.Fatalf("client departures were logged as:\n%s", got)
	}

	s.logExecError(live, "pod-daemon exec", errors.New("connection refused"), "sandbox_id", "sb")
	if !strings.Contains(log.all(), "ERROR pod-daemon exec") {
		t.Fatalf("a real failure must stay an error:\n%s", log.all())
	}
	// No logger, no panic.
	(&Server{}).logExecError(live, "x", errors.New("y"))
}
