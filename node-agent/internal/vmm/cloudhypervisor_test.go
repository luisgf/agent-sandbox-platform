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

// fakeCHRunner starts a minimal HTTP-over-unix server that looks like CH's API,
// and leaves a lock file next to the socket as CH does. A stubborn process
// ignores Kill until the test closes it.
type fakeCHRunner struct {
	mu      sync.Mutex
	starts  []fakeStart
	binary  string
	creates int
	boots   int
	deletes int
	// vmInfoState is what vm.info reports; "" means Running.
	vmInfoState string
	// stubborn makes the processes ignore Kill until the test closes them.
	stubborn bool
	procs    []*fakeCHProc
	// serialOut, when set, is what a guest writes to its serial console: the fake
	// serves it on the socket vm.create names, once something connects.
	serialOut string
	// created holds the vm.create bodies.
	created []chVMConfig
}

type fakeStart struct {
	Name string
	Args []string
	Sock string
}

type fakeCHProc struct {
	ln       net.Listener
	srv      *http.Server
	done     chan struct{}
	runner   *fakeCHRunner
	stubborn bool
}

func (p *fakeCHProc) Pid() int { return 4242 }

func (p *fakeCHProc) Kill() error {
	if p.stubborn {
		return nil
	}
	return p.exit()
}

func (p *fakeCHProc) exit() error {
	_ = p.srv.Close()
	_ = p.ln.Close()
	return nil
}

func (p *fakeCHProc) Wait() error {
	<-p.done
	return nil
}

// apiSocketArg is the --api-socket that Start passes to cloud-hypervisor.
func apiSocketArg(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--api-socket" {
			return args[i+1]
		}
	}
	return ""
}

func (r *fakeCHRunner) Start(name string, args ...string) (Process, error) {
	sock := apiSocketArg(args)
	if sock == "" {
		return nil, os.ErrInvalid
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(sock+APISocketLockSuffix, nil, 0o600); err != nil {
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
		var body chVMConfig
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.creates++
		r.created = append(r.created, body)
		out := r.serialOut
		r.mu.Unlock()
		if out != "" && body.Serial != nil && body.Serial.Socket != "" {
			if sln, err := net.Listen("unix", body.Serial.Socket); err == nil {
				go func() {
					defer sln.Close()
					if conn, err := sln.Accept(); err == nil {
						_, _ = conn.Write([]byte(out))
						time.Sleep(300 * time.Millisecond)
						_ = conn.Close()
					}
				}()
			}
		}
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
	mux.HandleFunc(chPathVMInfo, func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		state := r.vmInfoState
		r.mu.Unlock()
		if state == "" {
			state = "Running"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"` + state + `"}`))
	})

	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	proc := &fakeCHProc{ln: ln, srv: srv, done: done, runner: r, stubborn: r.stubborn}
	r.procs = append(r.procs, proc)
	return proc, nil
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
	if _, err := os.Stat(sock1 + APISocketLockSuffix); !os.IsNotExist(err) {
		t.Fatalf("lock of socket1 should be removed: err=%v", err)
	}
	if _, err := os.Stat(sock2 + APISocketLockSuffix); err != nil {
		t.Fatalf("lock of the running sandbox: %v", err)
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

	if _, err := os.Stat(sock2 + APISocketLockSuffix); !os.IsNotExist(err) {
		t.Fatalf("lock of socket2 should be removed: err=%v", err)
	}

	// Idempotent stop.
	if err := ch.Stop(ctx, cfg1.ID); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
}

// The lock is Cloud Hypervisor's while it runs: a Stop that cannot see the
// process exit removes the socket and leaves the lock.
func TestStopKeepsLockOfLiveCH(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeCHRunner{stubborn: true}
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", dir)
	ch.Runner = runner
	ch.ReadyTimeout = 2 * time.Second
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := ch.Start(context.Background(), MicroVMConfig{ID: id, KernelPath: "/k"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.procs[0].exit() })
	sock := ch.SocketPath(id)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := ch.Stop(ctx, id); err == nil {
		t.Fatal("Stop reported success for a CH that did not exit")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket should be removed: err=%v", err)
	}
	if _, err := os.Stat(sock + APISocketLockSuffix); err != nil {
		t.Fatalf("lock of a running CH: %v", err)
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
	lock := filepath.Join(dir, APISocketName("sb-timeout")+APISocketLockSuffix)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock of a failed start should be removed: err=%v", err)
	}
}

// hangRunner starts a CH that takes its lock but never serves the API.
type hangRunner struct{}

type hangProc struct {
	killed chan struct{}
}

func (hangRunner) Start(name string, args ...string) (Process, error) {
	if err := os.WriteFile(apiSocketArg(args)+APISocketLockSuffix, nil, 0o600); err != nil {
		return nil, err
	}
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

// WaitShutdown tells a guest that is still up from one that powered off: the VM
// reports Shutdown, or Cloud Hypervisor exits and its API goes away.
func TestWaitShutdown(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("/usr/bin/fake-cloud-hypervisor", dir)
	ch.Runner = runner
	ch.ReadyTimeout = 2 * time.Second
	ctx := context.Background()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := ch.Start(ctx, MicroVMConfig{ID: id, KernelPath: "/k", RootFSPath: "/r", CPUs: 1, MemoryMiB: 128}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if ch.WaitShutdown(ctx, id, 250*time.Millisecond) {
		t.Fatal("a running guest was reported down")
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("returned after %s, before the grace period", time.Since(start))
	}

	runner.mu.Lock()
	runner.vmInfoState = "Shutdown"
	runner.mu.Unlock()
	if !ch.WaitShutdown(ctx, id, time.Second) {
		t.Fatal("state Shutdown was not seen")
	}

	if !ch.WaitShutdown(ctx, "11111111-2222-3333-4444-555555555555", time.Second) {
		t.Fatal("an unknown sandbox has nothing running")
	}
}

// After the guest powers itself off Cloud Hypervisor exits on its own. Stop
// must then not ask the dead API to delete the VM, and must not fail.
func TestStopAfterGuestPoweredOff(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("/usr/bin/fake-cloud-hypervisor", dir)
	ch.Runner = runner
	ch.ReadyTimeout = 2 * time.Second
	ctx := context.Background()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := ch.Start(ctx, MicroVMConfig{ID: id, KernelPath: "/k", RootFSPath: "/r", CPUs: 1, MemoryMiB: 128}); err != nil {
		t.Fatal(err)
	}
	sock := ch.SocketPath(id)

	runner.mu.Lock()
	proc := runner.procs[0]
	runner.mu.Unlock()
	_ = proc.Kill() // the guest powered off: the process is gone and so is its API

	if !ch.WaitShutdown(ctx, id, time.Second) {
		t.Fatal("an exited Cloud Hypervisor was not seen")
	}
	if err := ch.Stop(ctx, id); err != nil {
		t.Fatalf("stop after poweroff: %v", err)
	}
	runner.mu.Lock()
	deletes := runner.deletes
	runner.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("vm.delete sent %d times to an exited process", deletes)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
	if ch.InstanceCount() != 0 {
		t.Fatalf("instances=%d", ch.InstanceCount())
	}
}

// With a SerialSocket the VM is created with its serial port on that socket and
// the console device off, the agent reads the guest's output from before the
// boot, and Stop removes the socket.
func TestStartCapturesTheGuestConsole(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake := &fakeCHRunner{serialOut: "[    0.000000] Linux version 7.0\nVFS: Unable to mount root fs\n"}
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", dir)
	ch.Runner = fake
	ch.ReadyTimeout = 5 * time.Second
	serial := filepath.Join(dir, "serial-sb-1.sock")
	if err := ch.Start(context.Background(), MicroVMConfig{ID: "sb-1", KernelPath: "/k", SerialSocket: serial}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0].Serial == nil || fake.created[0].Serial.Mode != "Socket" ||
		fake.created[0].Serial.Socket != serial || fake.created[0].Console == nil || fake.created[0].Console.Mode != "Off" {
		t.Fatalf("vm.create: %+v", fake.created)
	}
	var tail string
	for i := 0; i < 200; i++ {
		if tail = ch.ConsoleTail("sb-1", 4096); strings.Contains(tail, "Unable to mount root fs") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(tail, "Linux version") || !strings.Contains(tail, "Unable to mount root fs") {
		t.Fatalf("console tail: %q", tail)
	}
	if ch.ConsoleTail("nobody", 100) != "" {
		t.Fatal("a console for a sandbox that is not running")
	}
	if err := ch.Stop(context.Background(), "sb-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(serial); !os.IsNotExist(err) {
		t.Fatalf("the serial socket survived Stop: %v", err)
	}
	if ch.ConsoleTail("sb-1", 100) != "" {
		t.Fatal("a console for a sandbox that was stopped")
	}
}

// Without a SerialSocket the VM config says nothing about serial or console.
func TestStartWithoutASerialSocketLeavesTheConsoleAlone(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", dir)
	ch.Runner = fake
	ch.ReadyTimeout = 5 * time.Second
	if err := ch.Start(context.Background(), MicroVMConfig{ID: "sb-2", KernelPath: "/k"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ch.Stop(context.Background(), "sb-2") }()
	if len(fake.created) != 1 || fake.created[0].Serial != nil || fake.created[0].Console != nil {
		t.Fatalf("vm.create: %+v", fake.created)
	}
}
