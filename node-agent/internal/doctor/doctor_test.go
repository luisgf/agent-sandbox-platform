package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeHost is a machine the tests describe: which files there are, which commands
// are installed and what they print.
type fakeHost struct {
	euid     int
	files    map[string]int64  // path → size; a directory has size -1
	contents map[string]string // path → what ReadFile returns
	openErr  map[string]error
	bins     map[string]string // command → path
	runs     map[string]func(args []string) (string, error)
	calls    []string
	free     int64
	freeOK   bool
	digests  map[string]string
	hashed   []string
	writeErr error
}

func newFakeHost() *fakeHost {
	return &fakeHost{euid: 0, files: map[string]int64{}, contents: map[string]string{}, openErr: map[string]error{},
		bins: map[string]string{}, runs: map[string]func([]string) (string, error){}, free: 100000, freeOK: true, digests: map[string]string{}}
}

type fakeInfo struct {
	name string
	size int64
}

func (f fakeInfo) Name() string { return f.name }
func (f fakeInfo) Size() int64  { return f.size }
func (f fakeInfo) Mode() fs.FileMode {
	if f.size < 0 {
		return fs.ModeDir | 0o755
	}
	return 0o660
}
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.size < 0 }
func (f fakeInfo) Sys() any           { return nil }

func (f *fakeHost) host() Host {
	return Host{
		GOOS: "linux", Euid: f.euid,
		Stat: func(p string) (os.FileInfo, error) {
			if size, ok := f.files[p]; ok {
				return fakeInfo{p, size}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
		},
		ReadFile: func(p string) ([]byte, error) {
			if c, ok := f.contents[p]; ok {
				return []byte(c), nil
			}
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		},
		LookPath: func(name string) (string, error) {
			if p, ok := f.bins[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		OpenRW: func(p string) error { return f.openErr[p] },
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			if fn, ok := f.runs[name]; ok {
				return fn(args)
			}
			return "", nil
		},
		DiskFreeMiB: func(string) (int64, bool) { return f.free, f.freeOK },
		SHA256: func(p string) (string, error) {
			f.hashed = append(f.hashed, p)
			d, ok := f.digests[p]
			if !ok {
				return "", errors.New("unreadable")
			}
			return d, nil
		},
		Writable: func(string) error { return f.writeErr },
	}
}

func baseConfig() Config {
	return Config{
		NodeID: "n1", GuestKernel: "/opt/sandbox/vmlinux", GuestRootFS: "/opt/sandbox/rootfs.img",
		DiskDir: "/var/lib/asp/disks", DiskMinFreeMiB: 3000, CHBinary: "cloud-hypervisor", VirtiofsdBin: "virtiofsd",
		Confine: "auto", TapAuto: true,
	}
}

func run(t *testing.T, c Check) Result {
	t.Helper()
	return runOne(context.Background(), c, time.Second)
}

func find(t *testing.T, h Host, c Config, name string) Check {
	t.Helper()
	for _, ch := range Standard(h, c) {
		if ch.Name == name {
			return ch
		}
	}
	t.Fatalf("no check named %s", name)
	return Check{}
}

func want(t *testing.T, got Result, status Status, detail string) {
	t.Helper()
	if got.Status != status || !strings.Contains(got.Detail, detail) {
		t.Fatalf("got %s %q, want %s containing %q", got.Status, got.Detail, status, detail)
	}
	if (status == Fail || status == Warn) && got.Fix == "" && got.Name != "" {
		// A problem says how to fix it, except the ones that are only information.
		t.Logf("note: %s has no fix text", got.Name)
	}
}

func TestPrivileges(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "privileges")), OK, "root")
	f.euid = 1000
	got := run(t, find(t, f.host(), c, "privileges"))
	want(t, got, Fail, "not root")
	if got.Fix == "" {
		t.Error("a failure with no fix")
	}
	c.DryRun = true
	want(t, run(t, find(t, f.host(), c, "privileges")), Skip, "dry-run")
	h := f.host()
	h.GOOS = "darwin"
	want(t, run(t, find(t, h, baseConfig(), "privileges")), Fail, "Linux only")
}

func TestKVM(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "kvm")), Fail, "does not exist")
	f.files["/dev/kvm"] = 0
	f.openErr["/dev/kvm"] = errors.New("permission denied")
	want(t, run(t, find(t, f.host(), c, "kvm")), Fail, "permission denied")
	delete(f.openErr, "/dev/kvm")
	want(t, run(t, find(t, f.host(), c, "kvm")), OK, "opens read-write")
	c.DryRun = true
	delete(f.files, "/dev/kvm")
	want(t, run(t, find(t, f.host(), c, "kvm")), Skip, "dry-run")
}

func TestCloudHypervisor(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "cloud-hypervisor")), Fail, "not found")
	f.bins["cloud-hypervisor"] = "/usr/local/bin/cloud-hypervisor"
	f.runs["/usr/local/bin/cloud-hypervisor"] = func([]string) (string, error) { return "exec format error", errors.New("exit 126") }
	want(t, run(t, find(t, f.host(), c, "cloud-hypervisor")), Fail, "--version failed")
	f.runs["/usr/local/bin/cloud-hypervisor"] = func([]string) (string, error) { return "something else", nil }
	want(t, run(t, find(t, f.host(), c, "cloud-hypervisor")), Warn, "not a version")
	f.runs["/usr/local/bin/cloud-hypervisor"] = func([]string) (string, error) { return "cloud-hypervisor v48.0", nil }
	got := run(t, find(t, f.host(), c, "cloud-hypervisor"))
	want(t, got, Warn, "v48.0")
	if !strings.Contains(got.Fix, "v53") {
		t.Errorf("fix: %q", got.Fix)
	}
	f.runs["/usr/local/bin/cloud-hypervisor"] = func([]string) (string, error) { return "cloud-hypervisor v53.0", nil }
	want(t, run(t, find(t, f.host(), c, "cloud-hypervisor")), OK, "v53.0")
}

func TestVirtiofsd(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	got := run(t, find(t, f.host(), c, "virtiofsd"))
	want(t, got, Warn, "workspace cannot start")
	f.bins["virtiofsd"] = "/usr/libexec/virtiofsd"
	f.runs["/usr/libexec/virtiofsd"] = func([]string) (string, error) { return "virtiofsd 1.13.0", nil }
	want(t, run(t, find(t, f.host(), c, "virtiofsd")), OK, "virtiofsd 1.13.0")
	f.runs["/usr/libexec/virtiofsd"] = func([]string) (string, error) { return "usage: virtiofsd -o source=", errors.New("exit 1") }
	want(t, run(t, find(t, f.host(), c, "virtiofsd")), Warn, "--version failed")
}

func TestGuestImages(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), Fail, "is missing")
	f.files["/opt/sandbox/vmlinux"] = 0
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), Fail, "non-empty")
	f.files["/opt/sandbox/vmlinux"] = 70 << 20
	f.digests["/opt/sandbox/vmlinux"] = "sha256:aaaa"
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), OK, "sha256:aaaa")

	// The base image is gigabytes: it is hashed only to compare.
	f.files["/opt/sandbox/rootfs.img"] = 1536 << 20
	f.hashed = nil
	want(t, run(t, find(t, f.host(), c, "guest-rootfs")), OK, "no SHA256SUMS")
	if len(f.hashed) != 0 {
		t.Fatalf("hashed the base image with nothing to compare it with: %v", f.hashed)
	}
	f.contents["/opt/sandbox/SHA256SUMS"] = "aaaa  vmlinux\nbbbb *rootfs.img\n"
	f.digests["/opt/sandbox/rootfs.img"] = "sha256:bbbb"
	got := run(t, find(t, f.host(), c, "guest-rootfs"))
	want(t, got, OK, "matches SHA256SUMS")
	f.digests["/opt/sandbox/rootfs.img"] = "sha256:cccc"
	want(t, run(t, find(t, f.host(), c, "guest-rootfs")), Fail, "SHA256SUMS says sha256:bbbb")
	f.digests["/opt/sandbox/vmlinux"] = "sha256:aaaa"
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), OK, "matches SHA256SUMS")
	delete(f.digests, "/opt/sandbox/rootfs.img")
	want(t, run(t, find(t, f.host(), c, "guest-rootfs")), Warn, "cannot read it")
	c.DryRun = true
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), Skip, "dry-run")
}

func TestDiskDir(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	f.files["/var/lib/asp/disks"] = -1
	want(t, run(t, find(t, f.host(), c, "disk-dir")), OK, "100000 MiB free")
	f.free = 5000
	want(t, run(t, find(t, f.host(), c, "disk-dir")), Warn, "close to")
	f.free = 2000
	got := run(t, find(t, f.host(), c, "disk-dir"))
	want(t, got, Fail, "refuses to start or resume")
	if !strings.Contains(got.Fix, "asp session rm") {
		t.Errorf("fix: %q", got.Fix)
	}
	f.writeErr = errors.New("read-only file system")
	want(t, run(t, find(t, f.host(), c, "disk-dir")), Fail, "read-only file system")
	f.writeErr = nil
	// Not created yet: the space is the parent's.
	delete(f.files, "/var/lib/asp/disks")
	f.files["/var/lib/asp"] = -1
	f.free = 90000
	want(t, run(t, find(t, f.host(), c, "disk-dir")), OK, "in /var/lib/asp")
	f.freeOK = false
	want(t, run(t, find(t, f.host(), c, "disk-dir")), Warn, "cannot read the free space")
	c.DiskMinFreeMiB = 0
	f.freeOK, f.free = true, 1
	want(t, run(t, find(t, f.host(), c, "disk-dir")), OK, "1 MiB free")
}

func TestSystemd(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	got := run(t, find(t, f.host(), c, "systemd"))
	want(t, got, Warn, "not the init system")
	c.Confine = "on"
	want(t, run(t, find(t, f.host(), c, "systemd")), Fail, "refuses to run without it")
	f.files["/run/systemd/system"] = -1
	want(t, run(t, find(t, f.host(), c, "systemd")), Fail, "systemd-run not found")
	f.bins["systemd-run"] = "/usr/bin/systemd-run"
	want(t, run(t, find(t, f.host(), c, "systemd")), Fail, "cgroup v2")
	f.contents["/sys/fs/cgroup/cgroup.controllers"] = "cpu io"
	want(t, run(t, find(t, f.host(), c, "systemd")), Fail, "cgroup v2")
	f.contents["/sys/fs/cgroup/cgroup.controllers"] = "cpuset cpu io memory pids"
	want(t, run(t, find(t, f.host(), c, "systemd")), OK, "a unit of its own")
	c.Confine = "off"
	want(t, run(t, find(t, f.host(), c, "systemd")), Skip, "--vm-confine=off")
}

func TestTap(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "tap")), Fail, "ip (iproute2) not found")
	f.bins["ip"] = "/usr/sbin/ip"
	f.runs["ip"] = func(args []string) (string, error) {
		if args[0] == "tuntap" {
			return "ioctl(TUNSETIFF): Operation not permitted", errors.New("exit 1")
		}
		return "", nil
	}
	want(t, run(t, find(t, f.host(), c, "tap")), Fail, "Operation not permitted")
	f.runs["ip"] = func([]string) (string, error) { return "", nil }
	f.calls = nil
	want(t, run(t, find(t, f.host(), c, "tap")), OK, "created and removed")
	if len(f.calls) != 2 || !strings.Contains(f.calls[0], "tuntap add dev aspdoctor0") || !strings.Contains(f.calls[1], "link delete aspdoctor0") {
		t.Fatalf("the probe device was not removed: %v", f.calls)
	}
	c.TapAuto = false
	want(t, run(t, find(t, f.host(), c, "tap")), Skip, "--tap-auto")
}

func TestVsock(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "vsock")), Skip, "--host-vsock is off")
	c.HostVsock = true
	want(t, run(t, find(t, f.host(), c, "vsock")), Fail, "/dev/vsock does not exist")
	f.files["/dev/vsock"] = 0
	want(t, run(t, find(t, f.host(), c, "vsock")), OK, "/dev/vsock")
	c.HostVsockDir = "/run/asp/hv"
	delete(f.files, "/dev/vsock")
	want(t, run(t, find(t, f.host(), c, "vsock")), Skip, "unix sockets")
}

const completeTable = `table ip asp_egress {
	chain antispoof { type filter hook prerouting priority raw; policy accept; }
	chain prerouting { type nat hook prerouting priority dstnat; ip saddr 10.200.0.0/16 tcp dport { 80, 443 } redirect to :8888 }
	chain input { type filter hook input priority filter; }
	chain forward { type filter hook forward priority filter; }
}`

func TestNFT(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	// A node that does not enforce egress, with no nft: nothing to say.
	want(t, run(t, find(t, f.host(), c, "nft")), Skip, "does not force")
	c.EgressRedirect, c.EgressProxyListen, c.NFTMode = true, ":8888", "enforce"
	want(t, run(t, find(t, f.host(), c, "nft")), Fail, "nft not found")
	f.bins["nft"] = "/usr/sbin/nft"
	f.runs["nft"] = func([]string) (string, error) { return "Error: No such file or directory", errors.New("exit 1") }
	want(t, run(t, find(t, f.host(), c, "nft")), Fail, "not applied")
	f.runs["nft"] = func([]string) (string, error) { return completeTable, nil }
	want(t, run(t, find(t, f.host(), c, "nft")), OK, "mode enforce")
	f.runs["nft"] = func([]string) (string, error) {
		return strings.Replace(completeTable, "chain antispoof", "chain other", 1), nil
	}
	want(t, run(t, find(t, f.host(), c, "nft")), Fail, "chain antispoof")
	f.runs["nft"] = func([]string) (string, error) { return strings.ReplaceAll(completeTable, "8888", "9999"), nil }
	want(t, run(t, find(t, f.host(), c, "nft")), Fail, "proxy port 8888")

	// A table nobody here asked for: the leftover that dropped a guest's replies.
	c.EgressRedirect = false
	f.runs["nft"] = func([]string) (string, error) { return completeTable, nil }
	got := run(t, find(t, f.host(), c, "nft"))
	want(t, got, Warn, "leftover")
	if !strings.Contains(got.Fix, "nft delete table ip asp_egress") {
		t.Errorf("fix: %q", got.Fix)
	}
	f.runs["nft"] = func([]string) (string, error) { return "Error: No such file or directory", errors.New("exit 1") }
	want(t, run(t, find(t, f.host(), c, "nft")), Skip, "does not force")
	c.DryRun = true
	want(t, run(t, find(t, f.host(), c, "nft")), Skip, "dry-run")
}

func TestIPForwardAndTime(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "ip-forward")), Skip, "cannot read")
	f.contents["/proc/sys/net/ipv4/ip_forward"] = "0\n"
	want(t, run(t, find(t, f.host(), c, "ip-forward")), OK, "only through the egress proxy")
	f.contents["/proc/sys/net/ipv4/ip_forward"] = "1\n"
	want(t, run(t, find(t, f.host(), c, "ip-forward")), OK, "ip_forward is 1")

	want(t, run(t, find(t, f.host(), c, "time")), Skip, "timedatectl not found")
	f.bins["timedatectl"] = "/usr/bin/timedatectl"
	f.runs["timedatectl"] = func([]string) (string, error) { return "yes", nil }
	want(t, run(t, find(t, f.host(), c, "time")), OK, "synchronized")
	f.runs["timedatectl"] = func([]string) (string, error) { return "no", nil }
	want(t, run(t, find(t, f.host(), c, "time")), Warn, "not synchronized")
	f.runs["timedatectl"] = func([]string) (string, error) { return "Failed to connect to bus", errors.New("exit 1") }
	want(t, run(t, find(t, f.host(), c, "time")), Skip, "timedatectl failed")
}

func TestControlPlane(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	want(t, run(t, find(t, f.host(), c, "control-plane")), Skip, "not checked")
	c.ControlPlane = func(context.Context) error { return errors.New("401 unauthorized") }
	want(t, run(t, find(t, f.host(), c, "control-plane")), Fail, "401 unauthorized")
	c.ControlPlane = func(context.Context) error { return nil }
	want(t, run(t, find(t, f.host(), c, "control-plane")), OK, "takes the node's credential")
}

// A check that panics, that never answers or that answers nothing does not take the
// report down with it.
func TestRunIsRobust(t *testing.T) {
	checks := []Check{
		{"boom", func(context.Context) Result { panic("kaput") }},
		{"silent", func(context.Context) Result { return Result{} }},
		{"slow", func(ctx context.Context) Result { <-ctx.Done(); return Result{Status: OK, Detail: "done"} }},
		{"fine", func(context.Context) Result { return Result{Status: OK, Detail: "ok"} }},
	}
	rep := Run(context.Background(), "n1", checks, 30*time.Millisecond)
	if len(rep.Results) != 4 {
		t.Fatalf("results: %+v", rep.Results)
	}
	want(t, rep.Results[0], Fail, "kaput")
	want(t, rep.Results[1], Fail, "no answer")
	want(t, rep.Results[2], Warn, "took longer")
	want(t, rep.Results[3], OK, "ok")
	if !rep.Failed() {
		t.Fatal("a failed check did not fail the report")
	}
	n := rep.Counts()
	if n[Fail] != 2 || n[Warn] != 1 || n[OK] != 1 {
		t.Fatalf("counts %v", n)
	}
	if Run(context.Background(), "", []Check{checks[3]}, 0).Failed() {
		t.Fatal("a clean report failed")
	}
	// Warnings and skipped checks do not fail a report: only a failure does.
	if (Report{Results: []Result{{Name: "a", Status: Warn}, {Name: "b", Status: Skip}, {Name: "c", Status: OK}}}).Failed() {
		t.Fatal("warnings failed the report")
	}
}

func TestTextAndJSON(t *testing.T) {
	rep := Report{NodeID: "n1", Results: []Result{
		{Name: "kvm", Status: OK, Detail: "/dev/kvm opens read-write"},
		{Name: "virtiofsd", Status: Warn, Detail: "not found", Fix: "install it"},
		{Name: "nft", Status: Fail, Detail: "table missing", Fix: "restart the agent"},
		{Name: "vsock", Status: Skip, Detail: "off"},
	}}
	txt := rep.Text()
	for _, line := range []string{"node doctor (n1)", "ok    kvm", "warn  virtiofsd", "fix: install it", "fail  nft", "fix: restart the agent", "1 ok, 1 warn, 1 fail, 1 skipped"} {
		if !strings.Contains(txt, line) {
			t.Errorf("text lacks %q:\n%s", line, txt)
		}
	}
	// A fix is shown only under a problem.
	if strings.Contains(Report{Results: []Result{{Name: "a", Status: OK, Fix: "never shown"}}}.Text(), "never shown") {
		t.Error("a fix under an ok line")
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || !slices.Equal(back.Results, rep.Results) || back.NodeID != "n1" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if !strings.Contains(string(b), `"status":"fail"`) {
		t.Errorf("json: %s", b)
	}
}

// The order of the report is the order an operator reads: privileges first, the
// control plane last, every check present once.
func TestStandardChecksAreTheOnesTheIssueNames(t *testing.T) {
	var names []string
	for _, c := range Standard(newFakeHost().host(), baseConfig()) {
		names = append(names, c.Name)
	}
	for _, need := range []string{"kvm", "vsock", "nft", "virtiofsd", "cloud-hypervisor", "disk-dir", "guest-kernel", "guest-rootfs", "tap", "ip-forward", "time", "control-plane"} {
		if !slices.Contains(names, need) {
			t.Errorf("no check %q in %v", need, names)
		}
	}
	if names[0] != "privileges" || names[len(names)-1] != "control-plane" {
		t.Errorf("order: %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("%q twice", n)
		}
		seen[n] = true
	}
}

// A release installed by asp image pull sits in a directory of its own with its SHA256SUMS, and
// /opt/sandbox holds links to it.
func TestGuestImagesFollowLinksToTheirSums(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	f.files["/opt/sandbox/vmlinux"] = 70 << 20
	f.files["/var/lib/asp/images/0.1.0/vmlinux"] = 70 << 20
	f.contents["/var/lib/asp/images/0.1.0/SHA256SUMS"] = "aaaa  vmlinux\n"
	f.digests["/opt/sandbox/vmlinux"] = "sha256:aaaa"
	h := f.host()
	h.EvalSymlinks = func(p string) (string, error) {
		if p == "/opt/sandbox/vmlinux" {
			return "/var/lib/asp/images/0.1.0/vmlinux", nil
		}
		return p, nil
	}
	want(t, run(t, find(t, h, c, "guest-kernel")), OK, "matches SHA256SUMS")
	f.digests["/opt/sandbox/vmlinux"] = "sha256:bbbb"
	want(t, run(t, find(t, h, c, "guest-kernel")), Fail, "refuses to boot from it")
}

func TestGuestVerifyModesInTheDoctor(t *testing.T) {
	f := newFakeHost()
	c := baseConfig()
	f.files["/opt/sandbox/vmlinux"] = 70 << 20
	f.digests["/opt/sandbox/vmlinux"] = "sha256:aaaa"

	c.GuestVerify = "on"
	got := run(t, find(t, f.host(), c, "guest-kernel"))
	want(t, got, Fail, "--guest-verify=on")
	if !strings.Contains(got.Fix, "asp image pull") {
		t.Errorf("fix: %q", got.Fix)
	}
	f.contents["/opt/sandbox/SHA256SUMS"] = "aaaa  vmlinux\n"
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), OK, "matches SHA256SUMS")

	// A mismatch with --guest-verify=off is a warning: the node boots from it anyway.
	c.GuestVerify = "off"
	f.contents["/opt/sandbox/SHA256SUMS"] = "bbbb  vmlinux\n"
	want(t, run(t, find(t, f.host(), c, "guest-kernel")), Warn, "boots from it anyway")
}
