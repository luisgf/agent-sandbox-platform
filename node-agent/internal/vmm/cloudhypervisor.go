package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
)

// Cloud Hypervisor REST paths (OpenAPI /api/v1/* over a Unix socket).
const (
	chPathVMMPing  = "/api/v1/vmm.ping"
	chPathVMMInfo  = "/api/v1/vmm.info"
	chPathVMCreate = "/api/v1/vm.create"
	chPathVMBoot   = "/api/v1/vm.boot"
	chPathVMDelete = "/api/v1/vm.delete"
	chPathVMPause  = "/api/v1/vm.pause"

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
	// ReadyTimeout is how long Start waits for the API socket to accept Ping.
	ReadyTimeout time.Duration
	Logger       *slog.Logger

	HTTPClient *http.Client // shared-mode / injected client

	mu        sync.Mutex
	instances map[string]*chInstance
}

type chInstance struct {
	socketPath string
	proc       Process
	client     *http.Client
}

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
// --api-socket socketDir/ch-{id}.sock. The node-agent reaper uses it to find
// VMs a previous agent process left running. A CH on any other socket (the
// shared --ch-api-socket, another agent's directory) does not match.
func SpawnedSandbox(argv []string, socketDir string) (string, bool) {
	sock, ok := hostproc.FlagValue(argv, "--api-socket")
	if !ok {
		return "", false
	}
	// Newer Cloud Hypervisor also takes "path=<socket>[,...]".
	if p, ok := strings.CutPrefix(sock, "path="); ok {
		sock, _, _ = strings.Cut(p, ",")
	}
	if socketDir == "" {
		socketDir = defaultCHSocketDir
	}
	if filepath.Clean(filepath.Dir(sock)) != filepath.Clean(socketDir) {
		return "", false
	}
	return ParseAPISocketName(filepath.Base(sock))
}

func (c *CloudHypervisor) sharedMode() bool {
	return c.APISocket != ""
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

type chVMConfig struct {
	CPUs    chCpusConfig    `json:"cpus"`
	Memory  chMemoryConfig  `json:"memory"`
	Payload chPayloadConfig `json:"payload"`
	Disks   []chDiskConfig  `json:"disks,omitempty"`
	Net     []chNetConfig   `json:"net,omitempty"`
	Vsock   *chVsockConfig  `json:"vsock,omitempty"`
	Fs      []chFsConfig    `json:"fs,omitempty"`
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

	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	sock := filepath.Join(socketDir, APISocketName(config.ID))
	_ = os.Remove(sock) // drop stale socket

	binary := c.BinaryPath
	if binary == "" {
		binary = "cloud-hypervisor"
	}
	proc, err := c.runner().Start(binary, "--api-socket", sock)
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
		return err
	}
	if err := c.bootWith(ctx, client); err != nil {
		_ = c.deleteWith(ctx, client)
		c.cleanupFailed(proc, sock)
		return err
	}

	c.mu.Lock()
	c.instances[config.ID] = &chInstance{socketPath: sock, proc: proc, client: client}
	c.mu.Unlock()
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
	if ok {
		delete(c.instances, id)
	}
	c.mu.Unlock()
	if !ok {
		// Idempotent: nothing tracked (already stopped or never started).
		return nil
	}

	var errs []error
	if err := c.deleteWith(ctx, inst.client); err != nil {
		errs = append(errs, fmt.Errorf("vm.delete: %w", err))
	}
	exited := true
	if inst.proc != nil {
		if err := inst.proc.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
		done := make(chan struct{})
		go func() {
			_ = inst.proc.Wait()
			close(done)
		}()
		select {
		case <-done:
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
	c.logger().Info("CH stopped", "sandbox_id", id, "socket", inst.socketPath)
	if len(errs) > 0 {
		return fmt.Errorf("stop %s: %v", id, errs)
	}
	return nil
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
