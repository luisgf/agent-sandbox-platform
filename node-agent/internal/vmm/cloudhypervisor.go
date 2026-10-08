package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/rundir"
)

// Cloud Hypervisor REST paths (OpenAPI /api/v1/* over a Unix socket).
const (
	chPathVMMPing  = "/api/v1/vmm.ping"
	chPathVMMInfo  = "/api/v1/vmm.info"
	chPathVMCreate = "/api/v1/vm.create"
	chPathVMBoot   = "/api/v1/vm.boot"
	chPathVMDelete = "/api/v1/vm.delete"
	chPathVMPause  = "/api/v1/vm.pause"
	chPathVMInfo   = "/api/v1/vm.info"

	defaultCHSocketDir    = "/run/asp"
	defaultCHReadyTimeout = 30 * time.Second
)

// CloudHypervisor talks to CH via HTTP over Unix domain sockets.
//
// Modes:
//   - Shared (legacy/debug): APISocket is set → one external CH process; no spawn.
//   - Per-sandbox (default when not dry-run): SocketDir is set and APISocket empty →
//     Start spawns `cloud-hypervisor --api-socket <SocketDir>/ch-{id}.sock`,
//     waits until Ping succeeds, then CreateVM+Boot. Stop deletes the VM, kills
//     the process, and removes the socket and, once the process has exited,
//     the lock file CH keeps next to it.
type CloudHypervisor struct {
	BinaryPath string
	// APISocket is the shared --api-socket path (legacy). Non-empty → shared mode.
	APISocket string
	// SocketDir holds per-sandbox sockets ch-{sandboxID}.sock. Used when APISocket is empty.
	SocketDir string
	// Runner starts CH processes (per-sandbox mode). Nil → DefaultRunner.
	Runner Runner
	// Confine, when set, runs each CH in a transient systemd service of its own
	// with resource limits (per-sandbox mode). Runner is then not used.
	Confine *Confinement
	// ReadyTimeout is how long Start waits for the API socket to accept Ping.
	ReadyTimeout time.Duration
	// LookPath, when set, is asked for the binary before a VM is started, so that a node that has no
	// Cloud Hypervisor fails a start at once and says so, instead of after ReadyTimeout of waiting for a
	// socket nobody will open. A node sets exec.LookPath; tests that fake the process leave it nil.
	LookPath func(string) (string, error)
	Logger   *slog.Logger

	HTTPClient *http.Client // shared-mode / injected client

	mu        sync.Mutex
	instances map[string]*chInstance
	// onExit is told when a Cloud Hypervisor process ends that Stop did not end.
	onExit func(id string, info ExitInfo)
}

type chInstance struct {
	socketPath string
	proc       Process
	client     *http.Client
	// done is closed when proc.Wait returned (watch is its only caller). bootedAt
	// is when the VM was booted.
	done     chan struct{}
	bootedAt time.Time
	// exited is set once WaitShutdown saw the API go away: the process ended on
	// its own after the guest powered off, so Stop has no VM left to delete.
	exited bool
	// console keeps the last of the guest's serial output; serialSocket is its
	// socket, removed at stop. Nil/empty without MicroVMConfig.SerialSocket.
	console      *Console
	serialSocket string
}

// ErrBinaryNotFound means the Cloud Hypervisor binary is not on this node.
var ErrBinaryNotFound = errors.New("cloud-hypervisor is not on this node")

// NewCloudHypervisor constructs a shared-socket client (legacy / --ch-api-socket).
// Does not spawn cloud-hypervisor; the process must already be listening on apiSocket.
func NewCloudHypervisor(binaryPath, apiSocket string) *CloudHypervisor {
	c := &CloudHypervisor{BinaryPath: binaryPath, APISocket: apiSocket}
	c.HTTPClient = unixHTTPClient(apiSocket)
	return c
}

// NewSpawningCloudHypervisor constructs a per-sandbox spawner (--ch-socket-dir).
// Each Start runs cloud-hypervisor with its own API socket under socketDir.
func NewSpawningCloudHypervisor(binaryPath, socketDir string) *CloudHypervisor {
	if socketDir == "" {
		socketDir = defaultCHSocketDir
	}
	return &CloudHypervisor{
		BinaryPath:   binaryPath,
		SocketDir:    socketDir,
		Runner:       DefaultRunner{},
		ReadyTimeout: defaultCHReadyTimeout,
		Logger:       slog.Default(),
		instances:    make(map[string]*chInstance),
	}
}

// NewCloudHypervisorWithClient allows injecting a custom HTTP client (shared-mode tests).
func NewCloudHypervisorWithClient(apiSocket string, client *http.Client) *CloudHypervisor {
	return &CloudHypervisor{APISocket: apiSocket, HTTPClient: client}
}

// APISocketName is the per-sandbox API socket under SocketDir.
func APISocketName(sandboxID string) string {
	return "ch-" + sandboxID + ".sock"
}

// APISocketLockSuffix is appended to an API socket path for the lock file
// Cloud Hypervisor creates next to it (ch-{id}.sock.lock). CH does not remove
// it when it exits, so whoever removes the socket removes the lock too, once
// the process holding it is gone.
const APISocketLockSuffix = ".lock"

// ParseAPISocketName returns the sandbox an APISocketName belongs to.
func ParseAPISocketName(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, "ch-")
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, ".sock")
	return id, ok && id != ""
}

// SpawnedSandbox reports which sandbox a cloud-hypervisor process serves when
// a per-sandbox CloudHypervisor with this socketDir started it: its argv names
// --api-socket socketDir/ch-{id}.sock, or runDir/{id}/api.sock when the VMM runs
// as a user of its own (runDir is empty otherwise). The node-agent reaper uses it
// to find VMs a previous agent process left running. A CH on any other socket (the
// shared --ch-api-socket, another agent's directory) does not match.
func SpawnedSandbox(argv []string, socketDir, runDir string) (string, bool) {
	id, _, ok := SpawnedSocket(argv, socketDir, runDir)
	return id, ok
}

// SpawnedSocket is SpawnedSandbox, and also says which API socket the process
// serves.
func SpawnedSocket(argv []string, socketDir, runDir string) (id, socket string, ok bool) {
	sock, ok := hostproc.FlagValue(argv, "--api-socket")
	if !ok {
		return "", "", false
	}
	// Newer Cloud Hypervisor also takes "path=<socket>[,...]".
	if p, ok := strings.CutPrefix(sock, "path="); ok {
		sock, _, _ = strings.Cut(p, ",")
	}
	if socketDir == "" {
		socketDir = defaultCHSocketDir
	}
	sock = filepath.Clean(sock)
	dir := filepath.Dir(sock)
	if dir == filepath.Clean(socketDir) {
		id, ok := ParseAPISocketName(filepath.Base(sock))
		return id, sock, ok
	}
	if runDir != "" && filepath.Base(sock) == RunAPISocket && filepath.Dir(dir) == filepath.Clean(runDir) {
		return filepath.Base(dir), sock, true
	}
	return "", "", false
}

func (c *CloudHypervisor) sharedMode() bool {
	return c.APISocket != ""
}

// unprivileged is how the VMMs of this driver run as users of their own, or nil.
func (c *CloudHypervisor) unprivileged() *Unprivileged {
	if c.Confine == nil {
		return nil
	}
	return c.Confine.Unprivileged
}

func (c *CloudHypervisor) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *CloudHypervisor) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return DefaultRunner{}
}

func (c *CloudHypervisor) readyTimeout() time.Duration {
	if c.ReadyTimeout > 0 {
		return c.ReadyTimeout
	}
	return defaultCHReadyTimeout
}

func unixHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// Approximate CH OpenAPI request shapes.
type chCpusConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
	MaxVCPUs  int `json:"max_vcpus"`
}

type chMemoryConfig struct {
	Size int64 `json:"size"`
	// Shared is required for vhost-user (virtiofs). Cloud Hypervisor rejects
	// an fs device unless memory is shared or backed by huge pages.
	Shared bool `json:"shared,omitempty"`
}

type chPayloadConfig struct {
	Kernel  string `json:"kernel"`
	Cmdline string `json:"cmdline,omitempty"`
}

type chDiskConfig struct {
	Path      string `json:"path"`
	Readonly  bool   `json:"readonly,omitempty"`
	ImageType string `json:"image_type,omitempty"`
}

type chNetConfig struct {
	Tap string `json:"tap,omitempty"`
}

type chVsockConfig struct {
	CID    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

// chFsConfig is the Cloud Hypervisor virtio-fs device (socket = virtiofsd).
// Only emitted when MicroVMConfig.WorkspaceFSSocket is set.
type chFsConfig struct {
	Tag    string `json:"tag"`
	Socket string `json:"socket"`
}

// chSerialConfig and chConsoleConfig are the guest's serial port and its
// console. With a SerialSocket the serial port goes to a unix socket the agent
// reads (Console) and the console device is off.
type chSerialConfig struct {
	Mode   string `json:"mode"`
	Socket string `json:"socket,omitempty"`
}

type chConsoleConfig struct {
	Mode string `json:"mode"`
}

type chVMConfig struct {
	CPUs    chCpusConfig     `json:"cpus"`
	Memory  chMemoryConfig   `json:"memory"`
	Payload chPayloadConfig  `json:"payload"`
	Disks   []chDiskConfig   `json:"disks,omitempty"`
	Net     []chNetConfig    `json:"net,omitempty"`
	Vsock   *chVsockConfig   `json:"vsock,omitempty"`
	Fs      []chFsConfig     `json:"fs,omitempty"`
	Serial  *chSerialConfig  `json:"serial,omitempty"`
	Console *chConsoleConfig `json:"console,omitempty"`
}

func (c *CloudHypervisor) Ping(ctx context.Context) error {
	if !c.sharedMode() {
		// No shared process; per-sandbox readiness is checked inside Start.
		return nil
	}
	resp, err := c.do(ctx, c.client(), http.MethodGet, chPathVMMPing, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vmm.ping: status %d", resp.StatusCode)
	}
	return nil
}

func (c *CloudHypervisor) Info(ctx context.Context) (map[string]any, error) {
	if !c.sharedMode() {
		return map[string]any{"mode": "per-sandbox", "socket_dir": c.SocketDir}, nil
	}
	resp, err := c.do(ctx, c.client(), http.MethodGet, chPathVMMInfo, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("vmm.info: status %d: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *CloudHypervisor) CreateVM(ctx context.Context, config MicroVMConfig) error {
	return c.createVMWith(ctx, c.client(), config)
}

func (c *CloudHypervisor) createVMWith(ctx context.Context, client *http.Client, config MicroVMConfig) error {
	cpus := config.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	memBytes := int64(config.MemoryMiB) * 1024 * 1024
	if memBytes <= 0 {
		memBytes = 256 * 1024 * 1024
	}
	cmdline := config.Cmdline
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda rw"
	}
	mem := chMemoryConfig{Size: memBytes}
	body := chVMConfig{
		CPUs:    chCpusConfig{BootVCPUs: cpus, MaxVCPUs: cpus},
		Memory:  mem,
		Payload: chPayloadConfig{Kernel: config.KernelPath, Cmdline: cmdline},
	}
	if config.RootFSPath != "" {
		body.Disks = []chDiskConfig{{Path: config.RootFSPath, ImageType: "Raw"}}
	}
	if config.TapDevice != "" {
		body.Net = []chNetConfig{{Tap: config.TapDevice}}
	}
	if config.VsockCID != 0 && config.VsockPath != "" {
		body.Vsock = &chVsockConfig{CID: config.VsockCID, Socket: config.VsockPath}
	}
	if config.SerialSocket != "" {
		body.Serial = &chSerialConfig{Mode: "Socket", Socket: config.SerialSocket}
		body.Console = &chConsoleConfig{Mode: "Off"}
	}
	// virtiofs is attached only when a virtiofsd socket is already listening.
	// The reconciler starts that daemon when workspace_host_path is set and
	// puts the socket here. An fs device without a socket would fail vm.create,
	// so an empty socket omits fs even if a host path was recorded.
	if tag, sock, ok := WorkspaceFS(config); ok {
		body.Memory.Shared = true
		body.Fs = []chFsConfig{{Tag: tag, Socket: sock}}
	} else if strings.TrimSpace(config.WorkspaceHostPath) != "" {
		c.logger().Warn("workspace host path recorded; Cloud Hypervisor virtiofs not attached (no virtiofsd socket)",
			"id", config.ID,
			"host", config.WorkspaceHostPath,
			"guest_mount", WorkspaceGuestMount,
			"tag", WorkspaceVirtiofsTag,
		)
	}
	resp, err := c.do(ctx, client, http.MethodPut, chPathVMCreate, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vm.create: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (c *CloudHypervisor) Boot(ctx context.Context) error {
	return c.bootWith(ctx, c.client())
}

func (c *CloudHypervisor) bootWith(ctx context.Context, client *http.Client) error {
	resp, err := c.do(ctx, client, http.MethodPut, chPathVMBoot, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vm.boot: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (c *CloudHypervisor) Delete(ctx context.Context) error {
	return c.deleteWith(ctx, c.client())
}

func (c *CloudHypervisor) deleteWith(ctx context.Context, client *http.Client) error {
	resp, err := c.do(ctx, client, http.MethodPut, chPathVMDelete, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vm.delete: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// Start implements MicroVM: shared CreateVM+Boot, or per-sandbox spawn+Create+Boot.
func (c *CloudHypervisor) Start(ctx context.Context, config MicroVMConfig) error {
	if !c.sharedMode() {
		return c.startPerSandbox(ctx, config)
	}
	// Nobody reads a console socket in shared mode: leave the console alone.
	config.SerialSocket = ""
	if err := c.CreateVM(ctx, config); err != nil {
		return err
	}
	return c.Boot(ctx)
}

func (c *CloudHypervisor) startPerSandbox(ctx context.Context, config MicroVMConfig) error {
	if config.ID == "" {
		return fmt.Errorf("MicroVMConfig.ID required for per-sandbox CH")
	}
	socketDir := c.SocketDir
	if socketDir == "" {
		socketDir = defaultCHSocketDir
	}

	c.mu.Lock()
	if c.instances == nil {
		c.instances = make(map[string]*chInstance)
	}
	if _, exists := c.instances[config.ID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("sandbox %s already has a CH instance", config.ID)
	}
	c.mu.Unlock()

	if err := rundir.Ensure(socketDir); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	sock := filepath.Join(socketDir, APISocketName(config.ID))
	unpriv := c.unprivileged()
	if unpriv != nil {
		if config.RunDir == "" || config.VsockCID == 0 {
			return fmt.Errorf("a VMM that runs as a user of its own needs the directory and the CID of its VM")
		}
		sock = filepath.Join(config.RunDir, RunAPISocket)
	}
	_ = os.Remove(sock) // drop stale socket

	binary := c.BinaryPath
	if binary == "" {
		binary = "cloud-hypervisor"
	}
	if c.LookPath != nil {
		if _, err := c.LookPath(binary); err != nil {
			return fmt.Errorf("%w: %q is not installed (install Cloud Hypervisor v53, docs/how-to/install-node.md section 2, or point --ch-binary at it; sudo asp doctor says what else is missing)", ErrBinaryNotFound, binary)
		}
	}
	// --seccomp true is Cloud Hypervisor's default; it is spelled out so that a
	// different default in some version cannot turn the filter off.
	args := []string{"--api-socket", sock, "--seccomp", "true"}
	var proc Process
	var err error
	if c.Confine != nil {
		name := binary
		if unpriv != nil {
			// setpriv looks the command up in the unit's PATH, not the agent's.
			if p, err := exec.LookPath(binary); err == nil {
				binary = p
			}
			name, args = unpriv.Wrap(config.VsockCID, binary, args...)
		}
		proc, err = c.Confine.Launcher.Start(c.Confine.Spec(config.ID, config), name, args...)
	} else {
		proc, err = c.runner().Start(binary, args...)
	}
	if err != nil {
		return fmt.Errorf("spawn cloud-hypervisor: %w", err)
	}

	client := unixHTTPClient(sock)
	if err := c.waitReady(ctx, client); err != nil {
		c.cleanupFailed(proc, sock)
		return fmt.Errorf("wait for CH API on %s: %w", sock, err)
	}

	if err := c.createVMWith(ctx, client, config); err != nil {
		c.cleanupFailed(proc, sock)
		c.removeSerial(config.SerialSocket)
		return err
	}
	// Read the console from before the first instruction: connect now, boot next.
	var console *Console
	if config.SerialSocket != "" {
		console = NewConsole(ConsoleBytes)
		console.Attach(config.SerialSocket, 15*time.Second)
	}
	if err := c.bootWith(ctx, client); err != nil {
		_ = c.deleteWith(ctx, client)
		c.cleanupFailed(proc, sock)
		if console != nil {
			console.Close()
		}
		c.removeSerial(config.SerialSocket)
		return err
	}

	inst := &chInstance{socketPath: sock, proc: proc, client: client, console: console, serialSocket: config.SerialSocket,
		done: make(chan struct{}), bootedAt: time.Now()}
	c.mu.Lock()
	c.instances[config.ID] = inst
	c.mu.Unlock()
	go c.watch(config.ID, inst)
	c.logger().Info("CH spawned", "sandbox_id", config.ID, "socket", sock, "pid", proc.Pid())
	return nil
}

func (c *CloudHypervisor) cleanupFailed(proc Process, sock string) {
	if proc != nil {
		_ = proc.Kill()
		_ = proc.Wait()
	}
	_ = os.Remove(sock)
	_ = os.Remove(sock + APISocketLockSuffix)
}

func (c *CloudHypervisor) waitReady(ctx context.Context, client *http.Client) error {
	deadline := time.Now().Add(c.readyTimeout())
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("%w (last: %v)", err, lastErr)
			}
			return err
		}
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		resp, err := c.do(pingCtx, client, http.MethodGet, chPathVMMPing, nil)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("vmm.ping: status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s: %v", c.readyTimeout(), lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *CloudHypervisor) Stop(ctx context.Context, id string) error {
	if !c.sharedMode() {
		return c.stopPerSandbox(ctx, id)
	}
	return c.Delete(ctx)
}

func (c *CloudHypervisor) stopPerSandbox(ctx context.Context, id string) error {
	c.mu.Lock()
	inst, ok := c.instances[id]
	gone := false
	if ok {
		delete(c.instances, id)
		gone = inst.exited
	}
	c.mu.Unlock()
	if !ok {
		// Idempotent: nothing tracked (already stopped or never started).
		return nil
	}

	var errs []error
	if !gone {
		if err := c.deleteWith(ctx, inst.client); err != nil {
			errs = append(errs, fmt.Errorf("vm.delete: %w", err))
		}
	}
	exited := true
	if inst.proc != nil {
		if err := inst.proc.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
		select {
		case <-inst.done:
		case <-time.After(3 * time.Second):
			exited = false
			errs = append(errs, fmt.Errorf("process wait timed out"))
		case <-ctx.Done():
			exited = false
			errs = append(errs, ctx.Err())
		}
	}
	if err := os.Remove(inst.socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove socket: %w", err))
	}
	// The lock belongs to a CH that may still run until Wait returns; the
	// host cleanup of the next agent start removes it otherwise.
	if exited {
		if err := os.Remove(inst.socketPath + APISocketLockSuffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove socket lock: %w", err))
		}
	}
	if inst.console != nil {
		inst.console.Close()
	}
	c.removeSerial(inst.serialSocket)
	c.logger().Info("CH stopped", "sandbox_id", id, "socket", inst.socketPath)
	if len(errs) > 0 {
		return fmt.Errorf("stop %s: %v", id, errs)
	}
	return nil
}

// WaitShutdown implements Shutdowner for per-sandbox mode: it polls vm.info
// until the VM is in state Shutdown or the API is unreachable, which is how a
// Cloud Hypervisor whose guest powered off looks once its process has exited.
func (c *CloudHypervisor) WaitShutdown(ctx context.Context, id string, grace time.Duration) bool {
	client := c.client()
	var inst *chInstance
	if !c.sharedMode() {
		c.mu.Lock()
		inst = c.instances[id]
		c.mu.Unlock()
		if inst == nil {
			return true // nothing tracked: nothing running
		}
		client = inst.client
	}
	deadline := time.Now().Add(grace)
	for {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		state, err := c.vmState(callCtx, client)
		cancel()
		if err != nil {
			if inst != nil {
				c.mu.Lock()
				inst.exited = true
				c.mu.Unlock()
			}
			return true
		}
		if state == "Shutdown" {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// vmState is the "state" of vm.info: Created, Running, Shutdown, Paused.
func (c *CloudHypervisor) vmState(ctx context.Context, client *http.Client) (string, error) {
	resp, err := c.do(ctx, client, http.MethodGet, chPathVMInfo, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("vm.info: status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	var info struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", fmt.Errorf("vm.info: %w", err)
	}
	return info.State, nil
}

func (c *CloudHypervisor) Pause(ctx context.Context, id string) error {
	client := c.client()
	if !c.sharedMode() {
		c.mu.Lock()
		inst, ok := c.instances[id]
		c.mu.Unlock()
		if !ok {
			return fmt.Errorf("pause: unknown sandbox %s", id)
		}
		client = inst.client
	}
	resp, err := c.do(ctx, client, http.MethodPut, chPathVMPause, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vm.pause: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// SocketPath returns the API socket path for a tracked sandbox (tests / ops).
func (c *CloudHypervisor) SocketPath(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if inst, ok := c.instances[id]; ok {
		return inst.socketPath
	}
	return ""
}

// apiSocketPath is where the Cloud Hypervisor of sandbox id serves its API, whether
// or not this process tracks it.
func (c *CloudHypervisor) apiSocketPath(id string) string {
	dir := c.SocketDir
	if dir == "" {
		dir = defaultCHSocketDir
	}
	return filepath.Join(dir, APISocketName(id))
}

// InstanceCount returns how many per-sandbox CH processes are tracked (tests).
func (c *CloudHypervisor) InstanceCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.instances)
}

func (c *CloudHypervisor) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	if c.APISocket != "" {
		c.HTTPClient = unixHTTPClient(c.APISocket)
		return c.HTTPClient
	}
	return unixHTTPClient("")
}

func (c *CloudHypervisor) do(ctx context.Context, client *http.Client, method, path string, payload any) (*http.Response, error) {
	if client == nil {
		client = c.client()
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	// Host is ignored by the unix dialer; CH docs use http://localhost/...
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return client.Do(req)
}

var _ MicroVM = (*CloudHypervisor)(nil)
var _ VMM = (*CloudHypervisor)(nil)
var _ Shutdowner = (*CloudHypervisor)(nil)

// removeSerial removes the console socket Cloud Hypervisor left, if any.
func (c *CloudHypervisor) removeSerial(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// ConsoleTail implements ConsoleReader: what the guest of sandbox id last wrote
// to its serial console, up to max bytes. Empty for a sandbox with no console
// capture or one that is not running.
func (c *CloudHypervisor) ConsoleTail(id string, max int) string {
	c.mu.Lock()
	inst := c.instances[id]
	c.mu.Unlock()
	if inst == nil || inst.console == nil {
		return ""
	}
	return inst.console.Tail(max)
}

// ExitInfo says how a Cloud Hypervisor process ended and how long it ran.
type ExitInfo struct {
	// Err is what waiting for the process returned: nil for a clean exit, else
	// the exit status or the signal that killed it (OOM kill included).
	Err error
	// Lived is how long it ran since the VM booted.
	Lived time.Duration
}

// ExitNotifier is implemented by VMMs that tell when a VM's process ended on its
// own: a crash, an OOM kill, a guest that powered itself off.
type ExitNotifier interface {
	// SetExitHandler registers fn, called once for each process that exits
	// without Stop having asked it to. It runs on its own goroutine.
	SetExitHandler(fn func(id string, info ExitInfo))
}

// SetExitHandler implements ExitNotifier.
func (c *CloudHypervisor) SetExitHandler(fn func(id string, info ExitInfo)) {
	c.mu.Lock()
	c.onExit = fn
	c.mu.Unlock()
}

// watch waits for the process of one instance, the only caller of its Wait. When
// it ends while the instance is still tracked, nothing asked it to: the handler
// is told. Stop removes the instance before it kills the process, so a process
// that Stop ended is never reported.
func (c *CloudHypervisor) watch(id string, inst *chInstance) {
	err := inst.proc.Wait()
	close(inst.done)
	c.mu.Lock()
	// The API is gone with the process: a later Stop has no VM left to delete.
	inst.exited = true
	tracked := c.instances[id] == inst
	fn := c.onExit
	c.mu.Unlock()
	if !tracked {
		return
	}
	info := ExitInfo{Err: err, Lived: time.Since(inst.bootedAt)}
	// Info: the guest powering itself off is how a graceful stop ends its VM. The
	// reconciler warns about the ones that are a crash.
	c.logger().Info("cloud-hypervisor exited on its own", "sandbox_id", id, "lived", info.Lived.Round(time.Millisecond), "error", err)
	if fn != nil {
		fn(id, info)
	}
}

// Adopter is implemented by VMMs that can take over a VM an earlier agent process
// started and that kept running: its service is not bound to the agent's.
type Adopter interface {
	// Alive says, as an error, why the VM of sandbox id cannot be taken over: its
	// service is not active, or its API (apiSocket, "" for the node's socket
	// directory) does not answer, or the VM is not running.
	Alive(ctx context.Context, id, apiSocket string) error
	// Adopt takes the VM over: from now on it is supervised and reported as if
	// this process had started it.
	Adopt(ctx context.Context, id string, vm AdoptedVM) error
}

// AdoptedVM is what an agent needs to know about a VM it did not start.
type AdoptedVM struct {
	// APISocket is where the VMM serves its API. Empty is the socket of that
	// sandbox in the node's socket directory.
	APISocket string
	// SerialSocket is the socket its console is served on ("" for none).
	SerialSocket string
	// BootedAt is when it was booted.
	BootedAt time.Time
}

// Alive implements Adopter.
func (c *CloudHypervisor) Alive(ctx context.Context, id, apiSocket string) error {
	if c.sharedMode() {
		return errors.New("a shared Cloud Hypervisor runs one VM and is not adopted")
	}
	if c.Confine == nil {
		return errors.New("VMs that are children of the agent end with it: only confined VMs (their own systemd service) can be adopted")
	}
	if !c.Confine.Launcher.Active(UnitName(id)) {
		return fmt.Errorf("its service %s.service is not active", UnitName(id))
	}
	if apiSocket == "" {
		apiSocket = c.apiSocketPath(id)
	}
	state, err := c.vmState(ctx, unixHTTPClient(apiSocket))
	if err != nil {
		return fmt.Errorf("its API does not answer: %w", err)
	}
	if state != "Running" && state != "Paused" {
		return fmt.Errorf("the VM is %s", state)
	}
	return nil
}

// Adopt implements Adopter.
func (c *CloudHypervisor) Adopt(ctx context.Context, id string, vm AdoptedVM) error {
	if err := c.Alive(ctx, id, vm.APISocket); err != nil {
		return err
	}
	c.mu.Lock()
	if c.instances == nil {
		c.instances = make(map[string]*chInstance)
	}
	if _, exists := c.instances[id]; exists {
		c.mu.Unlock()
		return fmt.Errorf("sandbox %s already has a CH instance", id)
	}
	c.mu.Unlock()

	sock := vm.APISocket
	if sock == "" {
		sock = c.apiSocketPath(id)
	}
	serialSocket, bootedAt := vm.SerialSocket, vm.BootedAt
	var console *Console
	if serialSocket != "" {
		// CH serves its serial port on this socket; a client that connects reads the
		// guest's output from then on (what the earlier agent saw is gone).
		console = NewConsole(ConsoleBytes)
		console.Attach(serialSocket, 5*time.Second)
	}
	inst := &chInstance{socketPath: sock, proc: c.Confine.Launcher.Adopt(UnitName(id)), client: unixHTTPClient(sock),
		console: console, serialSocket: serialSocket, done: make(chan struct{}), bootedAt: bootedAt}
	c.mu.Lock()
	c.instances[id] = inst
	c.mu.Unlock()
	go c.watch(id, inst)
	c.logger().Info("CH adopted", "sandbox_id", id, "socket", sock, "pid", inst.proc.Pid(), "running_for", time.Since(bootedAt).Round(time.Second))
	return nil
}
