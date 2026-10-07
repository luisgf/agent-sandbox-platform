package safe

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func logger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRecoverLogsOnceAndTheGoroutineEnds(t *testing.T) {
	var buf bytes.Buffer
	var wg sync.WaitGroup
	var saw any
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer Recover(logger(&buf), "worker", func(v any) { saw = v }, "sandbox_id", "sb-1")
		nilMapWrite(nil)
		t.Error("ran past the panic")
	}()
	wg.Wait()
	out := buf.String()
	if !strings.Contains(out, "worker panicked") || !strings.Contains(out, "sandbox_id=sb-1") || !strings.Contains(out, "nil map") || !strings.Contains(out, "safe_test.go") {
		t.Fatalf("log: %s", out)
	}
	if strings.Count(out, "panicked") != 1 {
		t.Fatalf("the panic was logged more than once: %s", out)
	}
	if s, _ := saw.(error); s == nil || !strings.Contains(s.Error(), "nil map") {
		t.Fatalf("onPanic saw %v", saw)
	}
}

func TestRecoverDoesNothingWithoutAPanic(t *testing.T) {
	var buf bytes.Buffer
	called := false
	func() {
		defer Recover(logger(&buf), "worker", func(any) { called = true })
	}()
	if called || buf.Len() != 0 {
		t.Fatalf("called=%v log=%q", called, buf.String())
	}
}

func TestAPanicInTheCleanupIsContained(t *testing.T) {
	var buf bytes.Buffer
	func() {
		defer Recover(logger(&buf), "worker", func(any) { panic("again") })
		panic("first")
	}()
	if !strings.Contains(buf.String(), "cleaning up after the panic panicked too") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestNilLoggerUsesTheDefault(t *testing.T) {
	func() {
		defer Recover(nil, "worker", nil)
		panic("x")
	}()
}

// nilMapWrite panics the way a real bug would: an assignment to a nil map.
func nilMapWrite(m map[string]int) { m["boom"] = 1 }
