package doctor

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TestedCloudHypervisor is the major version of Cloud Hypervisor this code was
// written and tested against: its REST API and its options move between majors.
const TestedCloudHypervisor = 53

// NFTTable is the nftables table the egress redirect owns.
const NFTTable = "asp_egress"

// Config is what the checks look at: the node's configuration, resolved the way the
// agent resolves it.
type Config struct {
	NodeID string
	// DryRun: no VM is started, so the host needs none of the virtualization.
	DryRun bool

	GuestKernel string
	GuestRootFS string

	DiskDir string
	// DiskMinFreeMiB is --disk-min-free-mib, resolved; 0 does not check.
	DiskMinFreeMiB int64

	CHBinary     string
	VirtiofsdBin string

	// Confine is --vm-confine: auto, on or off.
	Confine string

	TapAuto      bool
	HostVsock    bool
	HostVsockDir string

	// EgressRedirect says the node is meant to force guests through its egress proxy
	// with nftables (the redirect is on, in enforce mode or soft).
	EgressRedirect bool
	// EgressProxyListen is where the proxy listens (":8888").
	EgressProxyListen string
	// NFTMode is enforce or soft.
	NFTMode string

	// ControlPlane checks that the control plane answers and takes the node's
	// credential. Nil skips the check.
	ControlPlane func(ctx context.Context) error
}

// Standard is the list of checks of a node, in the order they are shown.
func Standard(h Host, c Config) []Check {
	return []Check{
		{"privileges", func(ctx context.Context) Result { return checkPrivileges(h, c) }},
		{"kvm", func(ctx context.Context) Result { return checkKVM(h, c) }},
		{"cloud-hypervisor", func(ctx context.Context) Result { return checkCloudHypervisor(ctx, h, c) }},
		{"virtiofsd", func(ctx context.Context) Result { return checkVirtiofsd(ctx, h, c) }},
		{"guest-kernel", func(ctx context.Context) Result { return checkImage(h, c, "kernel", c.GuestKernel, true) }},
		{"guest-rootfs", func(ctx context.Context) Result { return checkImage(h, c, "base image", c.GuestRootFS, false) }},
		{"disk-dir", func(ctx context.Context) Result { return checkDiskDir(h, c) }},
		{"systemd", func(ctx context.Context) Result { return checkSystemd(h, c) }},
		{"tap", func(ctx context.Context) Result { return checkTap(ctx, h, c) }},
		{"vsock", func(ctx context.Context) Result { return checkVsock(h, c) }},
		{"nft", func(ctx context.Context) Result { return checkNFT(ctx, h, c) }},
		{"ip-forward", func(ctx context.Context) Result { return checkIPForward(h) }},
		{"time", func(ctx context.Context) Result { return checkTime(ctx, h) }},
		{"control-plane", func(ctx context.Context) Result { return checkControlPlane(ctx, c) }},
	}
}

func checkPrivileges(h Host, c Config) Result {
	switch {
	case h.GOOS != "linux":
		return Result{Status: Fail, Detail: "the node-agent runs sandboxes on Linux only (this is " + h.GOOS + ")"}
	case h.Euid == 0:
		return Result{Status: OK, Detail: "running as root"}
	case c.DryRun:
		return Result{Status: Skip, Detail: "dry-run: no TAP, nftables or unit is needed"}
	}
	return Result{Status: Fail, Detail: "not root: TAP devices, nftables rules, systemd units and virtiofsd need root",
		Fix: "run the node-agent (and this check) as root, e.g. through its systemd unit"}
}

func checkKVM(h Host, c Config) Result {
	if c.DryRun {
		return Result{Status: Skip, Detail: "dry-run: no VM is started"}
	}
	fi, err := h.Stat("/dev/kvm")
	if err != nil {
		return Result{Status: Fail, Detail: "/dev/kvm does not exist",
			Fix: "enable VT-x/AMD-V in the firmware and load kvm_intel or kvm_amd; inside a VM, enable nested virtualization on the host that runs it"}
	}
	if err := h.OpenRW("/dev/kvm"); err != nil {
		return Result{Status: Fail, Detail: "cannot open /dev/kvm read-write: " + err.Error(),
			Fix: "run as root or as a member of the group that owns it (usually kvm)"}
	}
	return Result{Status: OK, Detail: fmt.Sprintf("/dev/kvm opens read-write (%s)", fi.Mode().Perm())}
}

var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion pulls "53.0" out of "cloud-hypervisor v53.0" or "virtiofsd 1.13.0".
func parseVersion(out string) (major int, text string, ok bool) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, "", false
	}
	major, _ = strconv.Atoi(m[1])
	return major, strings.TrimPrefix(m[0], "v"), true
}

func checkCloudHypervisor(ctx context.Context, h Host, c Config) Result {
	if c.DryRun {
		return Result{Status: Skip, Detail: "dry-run: no VM is started"}
	}
	path, err := h.LookPath(c.CHBinary)
	if err != nil {
		return Result{Status: Fail, Detail: fmt.Sprintf("%q not found", c.CHBinary),
			Fix: "install cloud-hypervisor v" + strconv.Itoa(TestedCloudHypervisor) + " (docs/bare-metal-ch.md section 2.1), or point --ch-binary at it"}
	}
	out, err := h.Run(ctx, path, "--version")
	if err != nil {
		return Result{Status: Fail, Detail: path + " --version failed: " + firstLine(out, err), Fix: "the binary must run on this host (architecture, libraries)"}
	}
	major, text, ok := parseVersion(out)
	switch {
	case !ok:
		return Result{Status: Warn, Detail: path + " says " + strconv.Quote(firstLine(out, nil)) + ", which is not a version I know how to read"}
	case major != TestedCloudHypervisor:
		return Result{Status: Warn, Detail: fmt.Sprintf("%s is v%s; the agent is tested with v%d, whose REST API and options differ between majors", path, text, TestedCloudHypervisor),
			Fix: "install cloud-hypervisor v" + strconv.Itoa(TestedCloudHypervisor)}
	}
	return Result{Status: OK, Detail: fmt.Sprintf("%s v%s", path, text)}
}

func checkVirtiofsd(ctx context.Context, h Host, c Config) Result {
	if c.DryRun {
		return Result{Status: Skip, Detail: "dry-run: no workspace is shared"}
	}
	path, err := h.LookPath(c.VirtiofsdBin)
	if err != nil {
		return Result{Status: Warn, Detail: fmt.Sprintf("%q not found: a sandbox with a workspace cannot start", c.VirtiofsdBin),
			Fix: "install the Rust virtiofsd (not the QEMU one) or point --virtiofsd-bin at it"}
	}
	out, err := h.Run(ctx, path, "--version")
	if err != nil {
		return Result{Status: Warn, Detail: path + " --version failed: " + firstLine(out, err), Fix: "install the Rust virtiofsd"}
	}
	return Result{Status: OK, Detail: path + " " + firstLine(out, nil)}
}

// checkImage looks at the kernel or the base image every sandbox boots from. Its digest
// is checked against a SHA256SUMS file next to it when there is one; the kernel is
// always hashed (it is small), the base image only to compare (it is gigabytes).
func checkImage(h Host, c Config, what, path string, alwaysHash bool) Result {
	if c.DryRun {
		return Result{Status: Skip, Detail: "dry-run: nothing is booted"}
	}
	fi, err := h.Stat(path)
	if err != nil {
		return Result{Status: Fail, Detail: fmt.Sprintf("the guest %s %s is missing", what, path),
			Fix: "build or download the guest image (images/guest, scripts/build-guest-rootfs.sh) and point --guest-kernel / --guest-rootfs at it"}
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return Result{Status: Fail, Detail: fmt.Sprintf("the guest %s %s is not a non-empty file", what, path)}
	}
	want, listed := expectedDigest(h, path)
	size := fmt.Sprintf("%d MiB", fi.Size()>>20)
	if !listed && !alwaysHash {
		return Result{Status: OK, Detail: fmt.Sprintf("%s, %s (no SHA256SUMS next to it to compare with)", path, size)}
	}
	got, err := h.SHA256(path)
	if err != nil {
		return Result{Status: Warn, Detail: fmt.Sprintf("%s: cannot read it to hash it: %v", path, err)}
	}
	switch {
	case listed && got != want:
		return Result{Status: Fail, Detail: fmt.Sprintf("%s is %s, SHA256SUMS says %s", path, got, want),
			Fix: "the file is not the one that was released: download it again"}
	case listed:
		return Result{Status: OK, Detail: fmt.Sprintf("%s, %s, matches SHA256SUMS", path, size)}
	}
	return Result{Status: OK, Detail: fmt.Sprintf("%s, %s, %s", path, size, got)}
}

// expectedDigest reads the digest a SHA256SUMS in the file's directory gives it.
func expectedDigest(h Host, path string) (digest string, listed bool) {
	b, err := h.ReadFile(filepath.Join(filepath.Dir(path), "SHA256SUMS"))
	if err != nil {
		return "", false
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	name := filepath.Base(path)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return "sha256:" + strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func checkDiskDir(h Host, c Config) Result {
	if c.DryRun || c.DiskDir == "" {
		return Result{Status: Skip, Detail: "no per-sandbox disks"}
	}
	dir := c.DiskDir
	if _, err := h.Stat(dir); err != nil {
		// Not created yet: the agent makes it. Free space is the parent's.
		for dir != "/" && dir != "." {
			dir = filepath.Dir(dir)
			if _, err := h.Stat(dir); err == nil {
				break
			}
		}
	} else if err := h.Writable(dir); err != nil {
		return Result{Status: Fail, Detail: fmt.Sprintf("cannot create a file in %s: %v", dir, err), Fix: "the agent writes one disk per sandbox there: give it room, or point --disk-dir elsewhere"}
	}
	free, ok := h.DiskFreeMiB(dir)
	if !ok {
		return Result{Status: Warn, Detail: "cannot read the free space of " + dir}
	}
	switch {
	case c.DiskMinFreeMiB > 0 && free < c.DiskMinFreeMiB:
		return Result{Status: Fail, Detail: fmt.Sprintf("%d MiB free in %s, less than --disk-min-free-mib (%d): the node refuses to start or resume sandboxes", free, dir, c.DiskMinFreeMiB),
			Fix: "free space, delete stopped sandboxes (asp session rm), or lower --disk-min-free-mib"}
	case c.DiskMinFreeMiB > 0 && free < 2*c.DiskMinFreeMiB:
		return Result{Status: Warn, Detail: fmt.Sprintf("%d MiB free in %s, close to --disk-min-free-mib (%d)", free, dir, c.DiskMinFreeMiB)}
	}
	return Result{Status: OK, Detail: fmt.Sprintf("%d MiB free in %s", free, dir)}
}

func checkSystemd(h Host, c Config) Result {
	if c.DryRun || c.Confine == "off" {
		return Result{Status: Skip, Detail: "VMs are not confined in units of their own (--vm-confine=off or dry-run)"}
	}
	problem := func(detail, fix string) Result {
		if c.Confine == "on" {
			return Result{Status: Fail, Detail: detail + ", and --vm-confine=on refuses to run without it", Fix: fix}
		}
		return Result{Status: Warn, Detail: detail + ": VMs run as children of the agent, without a cgroup of their own and without surviving its restart", Fix: fix}
	}
	if _, err := h.Stat("/run/systemd/system"); err != nil {
		return problem("systemd is not the init system", "run the agent on a host with systemd, or set --vm-confine=off")
	}
	if _, err := h.LookPath("systemd-run"); err != nil {
		return problem("systemd-run not found", "install systemd")
	}
	if b, err := h.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err != nil || !strings.Contains(string(b), "memory") {
		return problem("cgroup v2 with the memory controller is not available", "boot with the unified cgroup hierarchy (the default on current distributions)")
	}
	return Result{Status: OK, Detail: "systemd-run and cgroup v2: each VM gets a unit of its own"}
}

func checkTap(ctx context.Context, h Host, c Config) Result {
	if c.DryRun || !c.TapAuto {
		return Result{Status: Skip, Detail: "the agent does not create TAP devices (--tap-auto is off or dry-run)"}
	}
	if _, err := h.LookPath("ip"); err != nil {
		return Result{Status: Fail, Detail: "ip (iproute2) not found", Fix: "install iproute2"}
	}
	const dev = "aspdoctor0"
	if out, err := h.Run(ctx, "ip", "tuntap", "add", "dev", dev, "mode", "tap"); err != nil {
		return Result{Status: Fail, Detail: "cannot create a TAP device: " + firstLine(out, err), Fix: "run as root (CAP_NET_ADMIN) on a kernel with the tun module (modprobe tun)"}
	}
	_, _ = h.Run(ctx, "ip", "link", "delete", dev)
	return Result{Status: OK, Detail: "a TAP device can be created and removed"}
}

func checkVsock(h Host, c Config) Result {
	if c.DryRun || !c.HostVsock {
		return Result{Status: Skip, Detail: "--host-vsock is off"}
	}
	if c.HostVsockDir != "" {
		return Result{Status: Skip, Detail: "guest-to-host services use unix sockets under " + c.HostVsockDir + " (--host-vsock-dir)"}
	}
	if _, err := h.Stat("/dev/vsock"); err != nil {
		return Result{Status: Fail, Detail: "/dev/vsock does not exist: --host-vsock listens on AF_VSOCK ports 26501 and 26502",
			Fix: "modprobe vsock (and vhost_vsock); or use --host-vsock-dir for unix sockets (labs)"}
	}
	return Result{Status: OK, Detail: "/dev/vsock is there"}
}

func checkNFT(ctx context.Context, h Host, c Config) Result {
	if c.DryRun {
		return Result{Status: Skip, Detail: "dry-run: no rules are applied"}
	}
	if _, err := h.LookPath("nft"); err != nil {
		if c.EgressRedirect {
			return Result{Status: Fail, Detail: "nft not found: the egress redirect cannot be applied", Fix: "install nftables"}
		}
		return Result{Status: Skip, Detail: "nft not found, and the node does not force guests through its proxy"}
	}
	out, err := h.Run(ctx, "nft", "list", "table", "ip", NFTTable)
	present := err == nil
	if !c.EgressRedirect {
		if present {
			return Result{Status: Warn, Detail: "the table " + NFTTable + " is installed but this node does not enforce egress: a leftover of another agent? Its rules outlive the agent and apply to every asp-* TAP of the host",
				Fix: "nft delete table ip " + NFTTable + " (and ip6), or start the agent with the egress proxy so it owns the table"}
		}
		return Result{Status: Skip, Detail: "this node does not force guests through an egress proxy (no --egress-proxy-listen)"}
	}
	if !present {
		return Result{Status: Fail, Detail: "the table " + NFTTable + " is not applied: guests are not forced through the proxy (the agent applies it when it starts in --nft-egress-mode=enforce)",
			Fix: "restart the agent; if it started, read its log for the nft error"}
	}
	var missing []string
	for _, want := range []string{"chain antispoof", "chain prerouting", "chain input", "chain forward"} {
		if !strings.Contains(out, want) {
			missing = append(missing, want)
		}
	}
	if port := portOf(c.EgressProxyListen); port != "" && !strings.Contains(out, port) {
		missing = append(missing, "the proxy port "+port)
	}
	if len(missing) > 0 {
		return Result{Status: Fail, Detail: "the table " + NFTTable + " lacks " + strings.Join(missing, ", "), Fix: "restart the agent so it applies its rules again"}
	}
	mode := c.NFTMode
	if mode == "" {
		mode = "enforce"
	}
	return Result{Status: OK, Detail: "the table " + NFTTable + " is applied (mode " + mode + ")"}
}

func portOf(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return ""
	}
	return addr[i+1:]
}

func checkIPForward(h Host) Result {
	b, err := h.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return Result{Status: Skip, Detail: "cannot read ip_forward"}
	}
	v := strings.TrimSpace(string(b))
	if v == "1" {
		return Result{Status: OK, Detail: "ip_forward is 1 (local-net tunnels need it; the egress redirect does not)"}
	}
	return Result{Status: OK, Detail: "ip_forward is " + v + ": guests reach the network only through the egress proxy (local-net tunnels need it set to 1)"}
}

func checkTime(ctx context.Context, h Host) Result {
	if _, err := h.LookPath("timedatectl"); err != nil {
		return Result{Status: Skip, Detail: "timedatectl not found"}
	}
	out, err := h.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if err != nil {
		return Result{Status: Skip, Detail: "timedatectl failed: " + firstLine(out, err)}
	}
	if strings.TrimSpace(out) == "yes" {
		return Result{Status: OK, Detail: "the host clock is synchronized (guests follow it through the PTP device)"}
	}
	return Result{Status: Warn, Detail: "the host clock is not synchronized: guests follow it, and certificates, tokens and logs depend on it",
		Fix: "enable an NTP client (systemd-timesyncd or chrony)"}
}

func checkControlPlane(ctx context.Context, c Config) Result {
	if c.ControlPlane == nil {
		return Result{Status: Skip, Detail: "not checked"}
	}
	if err := c.ControlPlane(ctx); err != nil {
		return Result{Status: Fail, Detail: "the control plane does not take this node: " + err.Error(),
			Fix: "check --control-plane-url, the CA, the node certificate (asp node list) or the API key"}
	}
	return Result{Status: OK, Detail: "the control plane answers and takes the node's credential"}
}

// firstLine is the first line of a command's output, or the error when it said nothing.
func firstLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	if out == "" && err != nil {
		return err.Error()
	}
	return out
}
