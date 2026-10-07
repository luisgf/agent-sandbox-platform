package vmm

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConsoleKeepsOnlyTheLastBytes(t *testing.T) {
	c := NewConsole(10)
	_, _ = c.Write([]byte("0123456789"))
	_, _ = c.Write([]byte("abc"))
	if got := c.Tail(0); got != "3456789abc" {
		t.Fatalf("kept %q", got)
	}
	// One write larger than the bound keeps its own tail.
	_, _ = c.Write([]byte("XXXXXXXXXXXXXXXXXXXXHELLOWORLD"))
	if got := c.Tail(0); got != "HELLOWORLD" {
		t.Fatalf("kept %q", got)
	}
	if NewConsole(0).max != ConsoleBytes {
		t.Fatal("the default bound is not ConsoleBytes")
	}
}

func TestConsoleTailStartsAtALine(t *testing.T) {
	c := NewConsole(0)
	_, _ = c.Write([]byte("first line\nsecond line\nthird line\n"))
	if got := c.Tail(25); got != "second line\nthird line\n" {
		t.Fatalf("Tail(25) = %q", got)
	}
	if got := c.Tail(12); got != "third line\n" {
		t.Fatalf("Tail(12) = %q", got)
	}
	if got := c.Tail(1000); got != "first line\nsecond line\nthird line\n" {
		t.Fatalf("Tail(1000) = %q", got)
	}
	// A cut with no line break inside keeps what fits.
	d := NewConsole(0)
	_, _ = d.Write([]byte("no newline at all here"))
	if got := d.Tail(7); got != "all here" && got != "ll here" && !strings.HasSuffix(got, "here") {
		t.Fatalf("Tail(7) = %q", got)
	}
	// Bytes that are not UTF-8 do not break a log line.
	e := NewConsole(0)
	_, _ = e.Write([]byte{'o', 'k', 0xff, 0xfe, '\n'})
	if got := e.Tail(0); !strings.HasPrefix(got, "ok") {
		t.Fatalf("Tail = %q", got)
	}
}

func shortSockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "con")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// Attach waits for the socket to appear (it exists once the VM is created, a
// moment after Attach starts), then keeps what the guest writes.
func TestConsoleAttachCapturesTheGuestOutput(t *testing.T) {
	path := shortSockPath(t, "serial.sock")
	c := NewConsole(0)
	c.Attach(path, 5*time.Second)
	defer c.Close()

	time.Sleep(150 * time.Millisecond) // the socket appears after Attach began
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("[    0.000000] Linux version 7.0\nVFS: Cannot open root device\n"))
		time.Sleep(200 * time.Millisecond)
	}()
	waitUntil(t, func() bool { return strings.Contains(c.Tail(0), "Cannot open root device") })
	if !strings.Contains(c.Tail(0), "Linux version") {
		t.Fatalf("tail=%q", c.Tail(0))
	}
}

func TestConsoleAttachGivesUpWhenThereIsNoSocket(t *testing.T) {
	c := NewConsole(0)
	c.Attach(shortSockPath(t, "none.sock"), 200*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	if c.Tail(0) != "" {
		t.Fatalf("tail=%q", c.Tail(0))
	}
	c.Close()
	c.Close() // closing twice is fine
}

func TestConsoleCloseStopsTheCapture(t *testing.T) {
	path := shortSockPath(t, "serial.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		accepted <- conn
	}()
	c := NewConsole(0)
	c.Attach(path, 5*time.Second)
	conn := <-accepted
	defer conn.Close()
	_, _ = conn.Write([]byte("before\n"))
	waitUntil(t, func() bool { return strings.Contains(c.Tail(0), "before") })
	c.Close()
	// The agent's end is closed: the guest's next write fails soon, and nothing
	// written after Close is kept.
	time.Sleep(100 * time.Millisecond)
	_, _ = conn.Write([]byte("after\n"))
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(c.Tail(0), "after") {
		t.Fatalf("kept output after Close: %q", c.Tail(0))
	}
	if !strings.Contains(c.Tail(0), "before") {
		t.Fatal("Close dropped what was kept")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
