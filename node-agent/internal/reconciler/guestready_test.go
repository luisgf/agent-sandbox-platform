package reconciler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// fakeGuest plays Cloud Hypervisor's hybrid vsock muxer for a guest that is
// still booting: it hangs up on the first refuse connections, as CH does while
// nothing listens on the guest port, then answers CONNECT and serves
// pod-daemon's /healthz.
type fakeGuest struct {
	mu     sync.Mutex
	refuse int
	conns  int
	ready  time.Time // first /healthz served
}

func startFakeGuest(t *testing.T, path string, refuse int) *fakeGuest {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	g := &fakeGuest{refuse: refuse}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go g.serve(conn)
		}
	}()
	return g
}

func (g *fakeGuest) serve(conn net.Conn) {
	defer conn.Close()
	g.mu.Lock()
	g.conns++
	booting := g.conns <= g.refuse
	g.mu.Unlock()
	if booting {
		return
	}
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "CONNECT ") {
		return
	}
	_, _ = fmt.Fprintf(conn, "OK 1073741824\n")
	req, err := http.ReadRequest(br)
	if err != nil || req.URL.Path != "/healthz" {
		return
	}
	g.mu.Lock()
	if g.ready.IsZero() {
		g.ready = time.Now()
	}
	g.mu.Unlock()
	_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
}

func (g *fakeGuest) seen() (time.Time, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ready, g.conns
}

// bootingEngine is FakeVMM plus a guest that binds the vsock muxer socket when
// the VM starts, as Cloud Hypervisor does.
type bootingEngine struct {
	*vmm.FakeVMM
	t      *testing.T
	refuse int
	// console is what the guest printed; ConsoleTail serves it. Empty: the engine
	// has no console to read.
	console string

	mu    sync.Mutex
	guest *fakeGuest
}

func (e *bootingEngine) Start(ctx context.Context, cfg vmm.MicroVMConfig) error {
	if err := e.FakeVMM.Start(ctx, cfg); err != nil {
		return err
	}
	g := startFakeGuest(e.t, cfg.VsockPath, e.refuse)
	e.mu.Lock()
	e.guest = g
	e.mu.Unlock()
	return nil
}

// runningCP serves one requested sandbox to node n1 and records when the node
// reports it running.
type runningCP struct {
	mu        sync.Mutex
	state     string
	detail    string
	runningAt time.Time
}

func newRunningCP(t *testing.T, id string) (*runningCP, *httptest.Server) {
	t.Helper()
	type sb struct {
		ID    string  `json:"id"`
		Node  *string `json:"node_id"`
		State string  `json:"state"`
		Image string  `json:"image_ref"`
		CPU   int     `json:"cpu_millis"`
		Mem   int     `json:"memory_mib"`
	}
	cp := &runningCP{state: "requested"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		s := sb{ID: id, State: cp.state, Image: "img", CPU: 1000, Mem: 256}
		if cp.state != "requested" {
			node := "n1"
			s.Node = &node
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []sb{s}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			cp.state = "starting"
			node := "n1"
			s.Node, s.State = &node, cp.state
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				State  string `json:"state"`
				Detail string `json:"detail"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			cp.state, cp.detail = body.State, body.Detail
			if body.State == "running" && cp.runningAt.IsZero() {
				cp.runningAt = time.Now()
			}
			s.State = cp.state
			_ = json.NewEncoder(w).Encode(s)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return cp, srv
}

func (cp *runningCP) running() (string, time.Time) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.state, cp.runningAt
}

func newGuestReadyReconciler(t *testing.T, srv *httptest.Server, eng *bootingEngine, timeout time.Duration) *Reconciler {
	t.Helper()
	rec := New(cpclient.New(srv.URL, srv.Client()), "n1", eng, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = timeout
	return rec
}

// "asp sandbox run" execs as soon as it sees running. Cloud Hypervisor is up
// before the guest boots, so a start reports running only once pod-daemon in
// the guest answers.
func TestStartReportsRunningOnceTheGuestAnswers(t *testing.T) {
	cp, srv := newRunningCP(t, "guest-ready-01")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 3}
	rec := newGuestReadyReconciler(t, srv, eng, 10*time.Second)
	rec.tick(context.Background())

	state, runningAt := cp.running()
	if state != "running" {
		t.Fatalf("state=%s, want running", state)
	}
	eng.mu.Lock()
	g := eng.guest
	eng.mu.Unlock()
	if g == nil {
		t.Fatal("the VM never started")
	}
	ready, conns := g.seen()
	if ready.IsZero() || runningAt.Before(ready) {
		t.Fatalf("running reported at %v, guest answered at %v", runningAt, ready)
	}
	if conns <= 3 {
		t.Fatalf("guest saw %d connections, want the 3 refused ones and a /healthz", conns)
	}
}

// A first boot whose guest never answers is not running: a broken image or a
// kernel that cannot find its disk looks like a healthy sandbox to everything
// but exec. After GuestReadyTimeout the VM is torn down and the start is
// reported failed with guest_not_ready, and the guest's last console line.
func TestStartFailsWhenTheGuestStaysSilent(t *testing.T) {
	cp, srv := newRunningCP(t, "guest-silent-01")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 1 << 30,
		console: "[    0.5] VFS: Cannot open root device \"vda\"\r\n\x1b[31mKernel panic - not syncing: VFS: Unable to mount root fs\x1b[0m\r\n"}
	rec := newGuestReadyReconciler(t, srv, eng, 500*time.Millisecond)
	start := time.Now()
	rec.tick(context.Background())

	state, _ := cp.running()
	cp.mu.Lock()
	detail := cp.detail
	cp.mu.Unlock()
	if state != "failed" || !strings.Contains(detail, "guest_not_ready") || !strings.Contains(detail, "500ms") ||
		!strings.Contains(detail, "last console line: Kernel panic - not syncing: VFS: Unable to mount root fs)") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Fatalf("gave up after %v, before the %v timeout", waited, 500*time.Millisecond)
	}
	if len(rec.Handles()) != 0 || len(eng.Running) != 0 {
		t.Fatalf("the VM of a failed start is still there: handles=%v running=%v", rec.Handles(), eng.Running)
	}
}

// A guest that cannot be asked about its console still fails cleanly.
func TestSilentGuestWithoutAConsole(t *testing.T) {
	cp, srv := newRunningCP(t, "guest-silent-02")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 1 << 30}
	rec := newGuestReadyReconciler(t, srv, eng, 300*time.Millisecond)
	rec.tick(context.Background())
	state, _ := cp.running()
	cp.mu.Lock()
	detail := cp.detail
	cp.mu.Unlock()
	if state != "failed" || strings.Contains(detail, "console") || !strings.Contains(detail, "guest_not_ready") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
}

func TestLastConsoleLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"\r\n \r\n":              "",
		"boot\r\nlogin: ":        " (last console line: login:)",
		"a\x1b[0m\n\x1b[2J\n":    " (last console line: a)",
		strings.Repeat("x", 500): " (last console line: " + strings.Repeat("x", 120) + ")",
	} {
		if got := lastConsoleLine(in); got != want {
			t.Errorf("lastConsoleLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// ConsoleTail makes bootingEngine a vmm.ConsoleReader when it has a console.
func (e *bootingEngine) ConsoleTail(id string, max int) string { return e.console }

// A resume whose guest never answers goes back to stopped with its disk, so it
// can be tried again: the same teardown as a first boot, without losing the disk.
func TestResumeFailsWhenTheGuestStaysSilent(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t}
	rec, disks := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("first boot: %s", got)
	}
	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "stopped" {
		t.Fatalf("stop: %s", got)
	}

	eng.refuse = 1 << 30 // the next guest never answers
	rec.GuestReadyTimeout = 300 * time.Millisecond
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "stopped" || !strings.Contains(detail, "resume failed") || !strings.Contains(detail, "guest_not_ready") {
		t.Fatalf("state=%s detail=%q", got, detail)
	}
	if !exists(diskOf(disks, idA)) || len(rec.Handles()) != 0 {
		t.Fatalf("disk=%v handles=%v", exists(diskOf(disks, idA)), rec.Handles())
	}
}
