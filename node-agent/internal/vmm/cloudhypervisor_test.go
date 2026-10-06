package vmm

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCloudHypervisorCreateBootDelete(t *testing.T) {
	var seen []string
	var createBody chVMConfig

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			switch {
			case r.URL.Path == chPathVMCreate && r.Method == http.MethodPut:
				if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
					t.Fatalf("decode create: %v", err)
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			case r.URL.Path == chPathVMBoot && r.Method == http.MethodPut:
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			case r.URL.Path == chPathVMDelete && r.Method == http.MethodPut:
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			case r.URL.Path == chPathVMMPing:
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
			case r.URL.Path == chPathVMMInfo:
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"version":"test"}`)), Header: make(http.Header)}, nil
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
		}),
	}

	ch := NewCloudHypervisorWithClient("/tmp/fake.sock", client)
	ctx := context.Background()
	if err := ch.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := ch.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info["version"] != "test" {
		t.Fatalf("info=%v", info)
	}
	cfg := MicroVMConfig{
		ID:         "sb-1",
		KernelPath: "/kernel",
		RootFSPath: "/rootfs.img",
		CPUs:       2,
		MemoryMiB:  512,
		TapDevice:  "tap0",
		VsockCID:   3,
		VsockPath:  "/tmp/vsock.sock",
		Cmdline:    "console=hvc0 root=/dev/vda rw",
	}
	if err := ch.CreateVM(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if createBody.CPUs.BootVCPUs != 2 || createBody.Memory.Size != 512*1024*1024 {
		t.Fatalf("create body=%+v", createBody)
	}
	if createBody.Payload.Kernel != "/kernel" || len(createBody.Disks) != 1 {
		t.Fatalf("payload/disks=%+v", createBody)
	}
	if createBody.Vsock == nil || createBody.Vsock.CID != 3 {
		t.Fatalf("vsock=%+v", createBody.Vsock)
	}
	if err := ch.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ch.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET " + chPathVMMPing,
		"GET " + chPathVMMInfo,
		"PUT " + chPathVMCreate,
		"PUT " + chPathVMBoot,
		"PUT " + chPathVMDelete,
	}
	if len(seen) != len(want) {
		t.Fatalf("seen=%v want=%v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen[%d]=%s want %s", i, seen[i], want[i])
		}
	}
}

func TestFakeVMM(t *testing.T) {
	f := NewFakeVMM(nil)
	ctx := context.Background()
	if err := f.Start(ctx, MicroVMConfig{ID: "x", CPUs: 1, MemoryMiB: 128}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) < 2 || f.Calls[0] != "create" || f.Calls[1] != "boot" {
		t.Fatalf("calls=%v", f.Calls)
	}
}

func TestCloudHypervisorSkipWithoutSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	ch := NewCloudHypervisor("cloud-hypervisor", "/tmp/does-not-exist-ch.sock")
	err := ch.Ping(context.Background())
	if err == nil {
		t.Fatal("expected ping error without socket")
	}
}

// fakeCHRunner starts a minimal HTTP-over-unix server that looks like CH's API.
type fakeCHRunner struct {
	mu      sync.Mutex
	starts  []fakeStart
	binary  string
	creates int
	boots   int
	deletes int
}

type fakeStart struct {
	Name string
	Args []string
	Sock string
}

type fakeCHProc struct {
	ln     net.Listener
	srv    *http.Server
	done   chan struct{}
	runner *fakeCHRunner
}

func (p *fakeCHProc) Pid() int { return 4242 }

func (p *fakeCHProc) Kill() error {
	_ = p.srv.Close()
	_ = p.ln.Close()
	return nil
}

func (p *fakeCHProc) Wait() error {
	<-p.done
	return nil
}

func (r *fakeCHRunner) Start(name string, args ...string) (Process, error) {
	sock := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--api-socket" {
			sock = args[i+1]
			break
		}
	}
	if sock == "" {
		return nil, os.ErrInvalid
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.starts = append(r.starts, fakeStart{Name: name, Args: append([]string{}, args...), Sock: sock})
	r.binary = name
	r.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc(chPathVMMPing, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc(chPathVMMInfo, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"fake"}`))
	})
	mux.HandleFunc(chPathVMCreate, func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.creates++
		r.mu.Unlock()
		_, _ = io.Copy(io.Discard, req.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(chPathVMBoot, func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.boots++
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(chPathVMDelete, func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.deletes++
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(chPathVMPause, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	return &fakeCHProc{ln: ln, srv: srv, done: done, runner: r}, nil
}

func TestPerSandboxSpawnStartStop(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("/usr/bin/fake-cloud-hypervisor", dir)
	ch.Runner = runner
	ch.ReadyTimeout = 2 * time.Second

	ctx := context.Background()
	cfg1 := MicroVMConfig{
		ID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		KernelPath: "/k", RootFSPath: "/r", CPUs: 1, MemoryMiB: 128,
	}
	cfg2 := MicroVMConfig{
		ID:         "11111111-2222-3333-4444-555555555555",
		KernelPath: "/k", RootFSPath: "/r", CPUs: 2, MemoryMiB: 256,
	}

	if err := ch.Start(ctx, cfg1); err != nil {
		t.Fatalf("start1: %v", err)
	}
	if err := ch.Start(ctx, cfg2); err != nil {
		t.Fatalf("start2: %v", err)
	}
	if ch.InstanceCount() != 2 {
		t.Fatalf("instances=%d", ch.InstanceCount())
	}

	sock1 := ch.SocketPath(cfg1.ID)
	sock2 := ch.SocketPath(cfg2.ID)
	want1 := filepath.Join(dir, "ch-"+cfg1.ID+".sock")
	want2 := filepath.Join(dir, "ch-"+cfg2.ID+".sock")
	if sock1 != want1 || sock2 != want2 {
		t.Fatalf("sockets got %q %q want %q %q", sock1, sock2, want1, want2)
	}

	runner.mu.Lock()
	nStarts := len(runner.starts)
	creates, boots := runner.creates, runner.boots
	bin := runner.binary
	runner.mu.Unlock()
	if nStarts != 2 {
		t.Fatalf("starts=%d", nStarts)
	}
	if creates != 2 || boots != 2 {
		t.Fatalf("creates=%d boots=%d", creates, boots)
	}
	if bin != "/usr/bin/fake-cloud-hypervisor" {
		t.Fatalf("binary=%s", bin)
	}
	for _, s := range runner.starts {
		if len(s.Args) < 2 || s.Args[0] != "--api-socket" {
			t.Fatalf("args=%v", s.Args)
		}
	}

	// Duplicate Start should fail.
	if err := ch.Start(ctx, cfg1); err == nil {
		t.Fatal("expected duplicate start error")
	}

	if err := ch.Stop(ctx, cfg1.ID); err != nil {
		t.Fatalf("stop1: %v", err)
	}
	if ch.InstanceCount() != 1 {
		t.Fatalf("after stop1 instances=%d", ch.InstanceCount())
	}
	if _, err := os.Stat(sock1); !os.IsNotExist(err) {
		t.Fatalf("socket1 should be removed: err=%v", err)
	}

	if err := ch.Stop(ctx, cfg2.ID); err != nil {
		t.Fatalf("stop2: %v", err)
	}
	if ch.InstanceCount() != 0 {
		t.Fatalf("after stop2 instances=%d", ch.InstanceCount())
	}

	runner.mu.Lock()
	deletes := runner.deletes
	runner.mu.Unlock()
	if deletes != 2 {
		t.Fatalf("deletes=%d", deletes)
	}

	// Idempotent stop.
	if err := ch.Stop(ctx, cfg1.ID); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
}

// The reaper finds the VMs a previous agent left by the argv Start runs. Spawn
// through the real path: that argv, and nothing else, names the sandbox.
func TestSpawnedSandboxMatchesWhatStartRuns(t *testing.T) {
	// A short directory: unix socket paths are limited to 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	runner := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("/usr/local/bin/cloud-hypervisor", dir)
	ch.Runner = runner
	ch.ReadyTimeout = 2 * time.Second
	ctx := context.Background()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := ch.Start(ctx, MicroVMConfig{ID: id, KernelPath: "/k"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Stop(ctx, id) })

	runner.mu.Lock()
	s := runner.starts[0]
	runner.mu.Unlock()
	argv := append([]string{s.Name}, s.Args...)
	if got, ok := SpawnedSandbox(argv, dir); !ok || got != id {
		t.Fatalf("SpawnedSandbox(%q) = %q %v, want %s", argv, got, ok, id)
	}
	sock := ch.SocketPath(id)
	if got, ok := ParseAPISocketName(filepath.Base(sock)); !ok || got != id {
		t.Fatalf("socket %s parses as %q %v", sock, got, ok)
	}

	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"cloud-hypervisor", "--api-socket=" + sock}, true},
		{[]string{"cloud-hypervisor", "--api-socket", "path=" + sock}, true},
		{[]string{"cloud-hypervisor", "--api-socket", filepath.Join(dir+"-other", APISocketName(id))}, false},
		{[]string{"cloud-hypervisor", "--api-socket", "/run/cloud-hypervisor/api.sock"}, false},
		{[]string{"cloud-hypervisor", "--api-socket", filepath.Join(dir, "api.sock")}, false},
		{[]string{"curl", "--unix-socket", sock, "http://localhost/api/v1/vmm.ping"}, false},
		{[]string{"cloud-hypervisor"}, false},
	} {
		if _, ok := SpawnedSandbox(tc.argv, dir); ok != tc.want {
			t.Errorf("SpawnedSandbox(%q) = %v, want %v", tc.argv, ok, tc.want)
		}
	}
}

func TestPerSandboxStartRequiresID(t *testing.T) {
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", t.TempDir())
	ch.Runner = &fakeCHRunner{}
	err := ch.Start(context.Background(), MicroVMConfig{KernelPath: "/k"})
	if err == nil {
		t.Fatal("expected error without ID")
	}
}

func TestPerSandboxWaitReadyTimeout(t *testing.T) {
	dir := t.TempDir()
	// Runner that never opens the socket.
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", dir)
	ch.ReadyTimeout = 150 * time.Millisecond
	ch.Runner = hangRunner{}
	err := ch.Start(context.Background(), MicroVMConfig{ID: "sb-timeout", KernelPath: "/k"})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "wait for CH API") {
		t.Fatalf("err=%v", err)
	}
	if ch.InstanceCount() != 0 {
		t.Fatalf("leaked instances=%d", ch.InstanceCount())
	}
}

type hangRunner struct{}

type hangProc struct {
	killed chan struct{}
}

func (hangRunner) Start(name string, args ...string) (Process, error) {
	return &hangProc{killed: make(chan struct{})}, nil
}

func (p *hangProc) Pid() int { return 1 }
func (p *hangProc) Kill() error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	return nil
}
func (p *hangProc) Wait() error {
	<-p.killed
	return nil
}

func TestSharedModeStartStop(t *testing.T) {
	var seen []string
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}),
	}
	ch := NewCloudHypervisorWithClient("/tmp/shared.sock", client)
	ctx := context.Background()
	if err := ch.Start(ctx, MicroVMConfig{ID: "x", KernelPath: "/k", CPUs: 1, MemoryMiB: 64}); err != nil {
		t.Fatal(err)
	}
	if err := ch.Stop(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 2 || !strings.Contains(seen[0], "vm.create") || !strings.Contains(seen[1], "vm.boot") {
		t.Fatalf("seen=%v", seen)
	}
}

func TestPerSandboxPingInfoNoop(t *testing.T) {
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", "/run/asp")
	if err := ch.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := ch.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info["mode"] != "per-sandbox" {
		t.Fatalf("info=%v", info)
	}
}
