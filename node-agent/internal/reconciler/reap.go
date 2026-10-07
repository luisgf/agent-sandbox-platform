package reconciler

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// ReapConfig says where a previous node-agent process kept per-sandbox state
// on this host. VMs are not adopted across restarts: the control plane fails
// them as node_agent_restarted when the new process registers, so whatever the
// old process left belongs to nobody, and Reap removes it.
type ReapConfig struct {
	// SocketDir is --ch-socket-dir. Cloud Hypervisor and virtiofsd processes
	// are matched by the sockets they serve here, and the per-sandbox sockets
	// and links are removed: ch-{id}.sock, vsock-{id}.sock and its
	// {port} hybrid listeners, virtiofs-{id}.sock, ssh-agent-{id}.sock. So
	// are the files those processes leave next to their sockets:
	// ch-{id}.sock.lock and virtiofs-{id}.sock.pid.
	SocketDir string
	// DiskDir holds the rootfs copies (rootfs-{id}.img). Empty skips them. The
	// node-agent leaves it empty: it keeps the disks of stopped sandboxes
	// (ADR-0012) and removes the ones nobody owns itself (Reconciler.gcDisks).
	DiskDir string
	// Keep are paths never removed even when named like a leftover: the base
	// rootfs, the shared --ch-api-socket, the bridge and identity sockets.
	Keep []string
	// Report logs what would be removed and removes nothing.
	Report bool

	// Procs lists and signals host processes. Nil skips processes.
	Procs hostproc.Table
	// SysClassNet is /sys/class/net. Empty skips TAP and WireGuard devices,
	// which are host-wide: one node-agent per host.
	SysClassNet string
	// Tap deletes TAP devices. Nil runs ip(8), without SoftFail.
	Tap *tap.Manager
	// LocalNet clears local-net tunnels, their policy routing and node keys.
	// Nil skips them.
	LocalNet *localnet.Host
	// Grace is how long processes get between SIGTERM and SIGKILL (default 5s).
	Grace  time.Duration
	Logger *slog.Logger
}

// ReapReport is what Reap found, and removed unless ReapConfig.Report.
type ReapReport struct {
	Processes []ReapedProcess
	// LocalNet are sandbox ids, or 8-character short ids for a WireGuard
	// device without a node key.
	LocalNet []string
	Taps     []string
	// Sockets are the sockets, links, and lock and pid files in SocketDir.
	Sockets []string
	Disks   []string
	// Err joins every leftover it could not list, stop or remove.
	Err error
}

// ReapedProcess is a Cloud Hypervisor or virtiofsd process of a previous agent.
type ReapedProcess struct {
	hostproc.Proc
	Kind      string // "cloud-hypervisor" or "virtiofsd"
	SandboxID string
	Socket    string // the socket it serves in SocketDir
}

const defaultReapGrace = 5 * time.Second

// Reap removes what a previous node-agent process left on this host, in
// dependency order: processes first (a VM holds its TAP and its disk), then
// local-net tunnels, TAP devices, sockets and rootfs copies. It only touches
// names this agent gives a sandbox the control plane created (a lowercase
// UUID), of the file type it creates. One failure does not stop the rest.
//
// The caller must hold the socket-dir lock (LockSocketDir): otherwise the VMs
// of an agent that is still running would look like leftovers.
func Reap(ctx context.Context, cfg ReapConfig) ReapReport {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	msg := "removing leftover of a previous node-agent"
	if cfg.Report {
		msg = "leftover of a previous node-agent (report only, kept)"
	}
	socketDir := cfg.SocketDir
	if socketDir == "" {
		socketDir = "/run/asp"
	}
	keep := map[string]bool{}
	for _, p := range cfg.Keep {
		if p != "" {
			keep[filepath.Clean(p)] = true
		}
	}
	var rep ReapReport
	var errs []error
	fail := func(what string, err error) {
		if err = absent(err); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}

	// Sockets of processes that outlived SIGKILL. The lock or pid file next
	// to such a socket is still the process's own and stays.
	running := map[string]bool{}
	if cfg.Procs != nil {
		procs, err := orphanProcesses(cfg.Procs, socketDir, keep)
		fail("list processes", err)
		stop := make([]hostproc.Proc, 0, len(procs))
		for _, p := range procs {
			log.Info(msg, "kind", p.Kind, "pid", p.PID, "sandbox_id", p.SandboxID)
			stop = append(stop, p.Proc)
		}
		rep.Processes = procs
		if !cfg.Report && len(stop) > 0 {
			grace := cfg.Grace
			if grace <= 0 {
				grace = defaultReapGrace
			}
			fail("stop processes", hostproc.Terminate(ctx, cfg.Procs, stop, grace))
			for _, p := range procs {
				if cfg.Procs.Alive(p.Proc) {
					running[p.Socket] = true
				}
			}
		}
	}

	if cfg.LocalNet != nil {
		ids, err := localNetLeftovers(cfg.LocalNet, cfg.SysClassNet)
		fail("list local-net state", err)
		for _, id := range ids {
			log.Info(msg, "kind", "local-net", "sandbox_id", id, "iface", localnet.IfacePrefix+localnet.ShortID(id))
			if !cfg.Report {
				fail("clear local-net "+id, cfg.LocalNet.Clear(id))
			}
		}
		rep.LocalNet = ids
	}

	if cfg.SysClassNet != "" {
		names, err := tap.Devices(cfg.SysClassNet)
		fail("list TAP devices", err)
		mgr := cfg.Tap
		if mgr == nil {
			mgr = &tap.Manager{Logger: log}
		}
		for _, name := range names {
			if short, _ := strings.CutPrefix(name, tap.DevicePrefix); !isShortID(short) {
				continue
			}
			log.Info(msg, "kind", "tap", "tap", name)
			rep.Taps = append(rep.Taps, name)
			if !cfg.Report {
				fail("delete TAP "+name, mgr.Delete(name))
			}
		}
	}

	socks, err := leftoverSockets(socketDir, keep, running)
	fail("list "+socketDir, err)
	// Sockets, links and runtime files are small and many (up to eight per
	// sandbox): debug unless the operator asked for the list.
	sockLevel := slog.LevelDebug
	if cfg.Report {
		sockLevel = slog.LevelInfo
	}
	for _, p := range socks {
		kind := "socket"
		switch mode, _ := socketDirKind(filepath.Base(p)); mode {
		case fs.ModeSymlink:
			kind = "link"
		case 0:
			kind = "file"
		}
		log.Log(ctx, sockLevel, msg, "kind", kind, "path", p)
		if !cfg.Report {
			fail("remove "+p, os.Remove(p))
		}
	}
	rep.Sockets = socks

	if cfg.DiskDir != "" {
		disks, err := leftoverDisks(cfg.DiskDir, keep)
		fail("list "+cfg.DiskDir, err)
		for _, p := range disks {
			log.Info(msg, "kind", "rootfs", "path", p)
			if !cfg.Report {
				fail("remove "+p, os.Remove(p))
			}
		}
		rep.Disks = disks
	}

	rep.Err = errors.Join(errs...)
	summary := "host cleanup done"
	if cfg.Report {
		summary = "host cleanup report (nothing removed)"
	}
	log.Info(summary, "socket_dir", socketDir,
		"processes", len(rep.Processes), "local_net", len(rep.LocalNet), "taps", len(rep.Taps),
		"sockets", len(rep.Sockets), "disks", len(rep.Disks), "errors", rep.Err != nil)
	return rep
}

// orphanProcesses finds the Cloud Hypervisor and virtiofsd processes serving a
// per-sandbox socket in socketDir. This process and Keep sockets are skipped.
func orphanProcesses(t hostproc.Table, socketDir string, keep map[string]bool) ([]ReapedProcess, error) {
	all, err := t.List()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []ReapedProcess
	for _, p := range all {
		if p.PID == self {
			continue
		}
		if id, ok := vmm.SpawnedSandbox(p.Argv, socketDir); ok {
			sock := filepath.Join(socketDir, vmm.APISocketName(id))
			if isSandboxID(id) && !keep[sock] {
				out = append(out, ReapedProcess{Proc: p, Kind: "cloud-hypervisor", SandboxID: id, Socket: sock})
			}
			continue
		}
		sock, ok := virtiofs.SocketPathOf(p.Argv)
		if !ok || filepath.Clean(filepath.Dir(sock)) != filepath.Clean(socketDir) {
			continue
		}
		if id, ok := between(filepath.Base(sock), virtiofsPrefix, ".sock"); ok && isSandboxID(id) {
			sock = filepath.Join(socketDir, filepath.Base(sock))
			out = append(out, ReapedProcess{Proc: p, Kind: "virtiofsd", SandboxID: id, Socket: sock})
		}
	}
	return out, nil
}

// localNetLeftovers returns the sandboxes whose tunnel state may still be
// installed: every node key (a blackholed session has a key but no device)
// and every allocation (table, port, /30 to release), then WireGuard devices
// without either, by short id.
func localNetLeftovers(h *localnet.Host, sysClassNet string) ([]string, error) {
	keyIDs, err := h.KeyIDs()
	errs := []error{absent(err)}
	if h.Alloc != nil {
		keyIDs = append(keyIDs, h.Alloc.IDs()...)
	}
	var ids []string
	shorts := map[string]bool{}
	seen := map[string]bool{}
	for _, id := range keyIDs {
		if isSandboxID(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
			shorts[localnet.ShortID(id)] = true
		}
	}
	if sysClassNet != "" {
		ifaces, err := localnet.Ifaces(sysClassNet)
		errs = append(errs, absent(err))
		for _, name := range ifaces {
			short, _ := strings.CutPrefix(name, localnet.IfacePrefix)
			if isShortID(short) && !shorts[short] {
				ids = append(ids, short)
				shorts[short] = true
			}
		}
	}
	return ids, errors.Join(errs...)
}

// leftoverSockets lists the per-sandbox sockets and links in socketDir, and
// the lock and pid files next to them. A name of another file type (a
// directory, a regular file named like a socket) is not ours and stays. A lock
// or pid file stays with its socket when that is kept or its process running.
func leftoverSockets(socketDir string, keep, running map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(socketDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		path := filepath.Join(socketDir, e.Name())
		want, ok := socketDirKind(e.Name())
		if !ok || e.Type() != want || keep[path] {
			continue
		}
		if sock, ok := runtimeFileOf(e.Name()); ok {
			if sock = filepath.Join(socketDir, sock); keep[sock] || running[sock] {
				continue
			}
		}
		out = append(out, path)
	}
	return out, nil
}

// socketDirKind maps a per-sandbox name in SocketDir to the file type left
// there: 0 for the regular lock and pid files of runtimeFileOf. Global sockets
// (the SSH bridge, identity, host-vsock) and node-agent.lock never match.
func socketDirKind(name string) (fs.FileMode, bool) {
	if sock, ok := runtimeFileOf(name); ok {
		_, ours := socketDirKind(sock)
		return 0, ours
	}
	if id, ok := vmm.ParseAPISocketName(name); ok {
		return fs.ModeSocket, isSandboxID(id)
	}
	// {muxer}_{port} (hostvsock.HybridGuestPath) next to vsock-{id}.sock.
	if muxer, port, ok := strings.Cut(name, ".sock_"); ok {
		id, ok := between(muxer+".sock", vsockPrefix, ".sock")
		return fs.ModeSocket, ok && isDigits(port) && isSandboxID(id)
	}
	for _, k := range []struct {
		prefix string
		mode   fs.FileMode
	}{
		{vsockPrefix, fs.ModeSocket},
		{virtiofsPrefix, fs.ModeSocket},
		{serialPrefix, fs.ModeSocket},
		{sshAgentPrefix, fs.ModeSymlink},
	} {
		if id, ok := between(name, k.prefix, ".sock"); ok {
			return k.mode, isSandboxID(id)
		}
	}
	return 0, false
}

// runtimeFileOf returns the socket name a file in SocketDir sits next to when
// it is one the process serving that socket creates and does not remove on
// exit: Cloud Hypervisor's ch-{id}.sock.lock, virtiofsd's
// virtiofs-{id}.sock.pid.
func runtimeFileOf(name string) (string, bool) {
	if sock, ok := strings.CutSuffix(name, vmm.APISocketLockSuffix); ok {
		_, ok = vmm.ParseAPISocketName(sock)
		return sock, ok
	}
	if sock, ok := strings.CutSuffix(name, virtiofs.PIDFileSuffix); ok {
		_, ok = between(sock, virtiofsPrefix, ".sock")
		return sock, ok
	}
	return "", false
}

// leftoverDisks lists the rootfs copies in diskDir. The base image stays even
// if it lives there under such a name (Keep, compared as files).
func leftoverDisks(diskDir string, keep map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(diskDir)
	if err != nil {
		return nil, err
	}
	var kept []fs.FileInfo
	for p := range keep {
		if fi, err := os.Stat(p); err == nil {
			kept = append(kept, fi)
		}
	}
	var out []string
	for _, e := range entries {
		id, ok := between(e.Name(), rootfsPrefix, ".img")
		if !ok || !isSandboxID(id) || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(diskDir, e.Name())
		if keep[path] || sameFileAsAny(path, kept) {
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

func sameFileAsAny(path string, infos []fs.FileInfo) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	for _, k := range infos {
		if os.SameFile(fi, k) {
			return true
		}
	}
	return false
}

// absent treats a missing directory as an empty one (no /proc on macOS, no
// key dir before the first local-net session) and a file already gone as
// removed.
func absent(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// between returns what name holds between prefix and suffix.
func between(name, prefix, suffix string) (string, bool) {
	s, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(s, suffix)
}

// isSandboxID reports whether s is named the way the control plane names
// sandboxes: a lowercase UUID. The reaper touches nothing else, so a file or
// device of another tool that only shares a prefix stays.
func isSandboxID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isLowerHex(s[i]) {
				return false
			}
		}
	}
	return true
}

// isShortID reports whether s is the first 8 characters of a sandbox id, as
// in TAP (asp-*) and WireGuard (wg-asp-*) device names.
func isShortID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	return true
}

func isLowerHex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' }

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
