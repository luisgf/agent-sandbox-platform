package standalone

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Options is how asp-server was asked to run.
type Options struct {
	// DataDir is where everything lives (default /var/lib/asp).
	DataDir string
	// Listen is where the control plane listens (default 127.0.0.1:8443: the loopback, until the
	// operator says otherwise).
	Listen string
	// TLSSANs are more names or addresses its certificate must be valid for.
	TLSSANs []string
	// NodeID names the node on this host (default: the host name).
	NodeID string
	// NoAgent runs the control plane alone.
	NoAgent bool
	// Profile is ProfileDefault or ProfileLab.
	Profile string
	// ControlPlane and NodeAgent are the programs to run; empty finds asp-control-plane and
	// asp-node-agent next to asp-server, then on the PATH and where the packages put them.
	ControlPlane, NodeAgent string
	// User is the account the control plane runs as when asp-server runs as root (default
	// asp-control-plane, which its package makes); Group may read the administration key
	// (default asp).
	User, Group string
	// CLIConfig is where the configuration of the asp command of this host is left if it is not
	// there yet (default /etc/asp/asp.yaml); "-" leaves none.
	CLIConfig string
	// Env is the environment the programs inherit (default: this process's).
	Env []string
	// Out receives asp-server's log and the output of both programs (default stderr).
	Out io.Writer

	// Seams for the tests.
	Hostname func() (string, error)
	Addrs    func() ([]net.Addr, error)
	Now      func() time.Time
	HasKVM   func() bool
}

func (o *Options) defaults() {
	if o.DataDir == "" {
		o.DataDir = DefaultDataDir
	}
	if o.Listen == "" {
		o.Listen = "127.0.0.1:8443"
	}
	if o.Profile == "" {
		o.Profile = ProfileDefault
	}
	if o.User == "" {
		o.User = "asp-control-plane"
	}
	if o.Group == "" {
		o.Group = "asp"
	}
	if o.CLIConfig == "" {
		o.CLIConfig = "/etc/asp/asp.yaml"
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
	if o.Out == nil {
		o.Out = os.Stderr
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HasKVM == nil {
		o.HasKVM = func() bool {
			fi, err := os.Stat("/dev/kvm")
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		}
	}
}

// Validate refuses what cannot work before anything is made.
func (o *Options) Validate() error {
	if o.Profile != ProfileDefault && o.Profile != ProfileLab {
		return fmt.Errorf("--profile: %q is not %s or %s", o.Profile, ProfileDefault, ProfileLab)
	}
	if _, port, err := net.SplitHostPort(o.Listen); err != nil || port == "" {
		return fmt.Errorf("--listen: %q is not host:port", o.Listen)
	}
	return nil
}

var nodeIDChars = regexp.MustCompile(`[^a-z0-9.-]+`)

// nodeIDFor turns a host name into the id of its node: lower case, letters, digits, dots and
// dashes, and "-node" after it. The host's own name is in the control plane's certificate, and the
// control plane refuses a node that carries a name of its certificate (a node certificate with
// that name could pass for the server), so the id cannot be the bare host name.
func nodeIDFor(hostname string) string {
	id := nodeIDChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(hostname)), "-")
	id = strings.Trim(id, "-.")
	if id == "" {
		return "node"
	}
	return id + "-node"
}

// certificateCN is the common name of the certificate asp-server makes, which is a name too.
const certificateCN = "asp-server"

// checkNodeID refuses an id the control plane would refuse to enrol: any name of its own TLS
// certificate. Better said here, before anything starts, than in the log of a node that retries.
func checkNodeID(id string, names Names) error {
	reserved := append([]string{certificateCN}, names.DNS...)
	for _, ip := range names.IPs {
		reserved = append(reserved, ip.String())
	}
	for _, r := range reserved {
		if strings.EqualFold(id, r) {
			return fmt.Errorf("--node-id %q is a name in the control plane's TLS certificate, which a node may not carry: choose another", id)
		}
	}
	return nil
}

// Prepare makes what a host needs and leaves it where Layout says: the directories, the
// certificate, the administration key, the node token and the secret of the local API. What is
// already there is kept; nothing is replaced but a certificate that does not cover a name.
func Prepare(opts Options) (Prepared, error) {
	opts.defaults()
	if err := opts.Validate(); err != nil {
		return Prepared{}, err
	}
	root, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return Prepared{}, err
	}
	layout := Layout{Root: root}
	hostname, err := opts.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "localhost"
	}
	p := Prepared{Layout: layout, Listen: opts.Listen, NodeID: opts.NodeID}
	if p.NodeID == "" {
		p.NodeID = nodeIDFor(strings.SplitN(hostname, ".", 2)[0])
	}
	if isRoot() {
		if a, ok := lookupUser(opts.User); ok {
			p.ControlPlaneAccount = a
		}
	}

	// The directories. server/ belongs to the control plane's account and can be traversed, not
	// listed, by others: the group that reads the administration key reaches it by its path.
	for _, d := range []struct {
		path string
		perm os.FileMode
	}{
		{layout.Root, 0o755}, {layout.Server(), 0o711},
		{layout.Disks(), 0o700}, {layout.NodeCerts(), 0o700}, {layout.LocalNet(), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.perm); err != nil {
			return Prepared{}, err
		}
		if err := os.Chmod(d.path, d.perm); err != nil {
			return Prepared{}, err
		}
	}
	if opts.Profile == ProfileLab {
		if err := os.MkdirAll(layout.Run(), 0o755); err != nil {
			return Prepared{}, err
		}
	}
	cp := p.ControlPlaneAccount
	if cp.name != "" {
		if err := chown(layout.Server(), cp.uid, cp.gid); err != nil {
			return Prepared{}, err
		}
	}

	names, err := NamesFor(hostOf(opts.Listen), hostname, opts.TLSSANs, opts.Addrs)
	if err != nil {
		return Prepared{}, err
	}
	if err := checkNodeID(p.NodeID, names); err != nil {
		return Prepared{}, err
	}
	replaced, err := EnsureCertificate(layout.TLSCert(), layout.TLSKey(), names, opts.Now())
	if err != nil {
		return Prepared{}, fmt.Errorf("TLS certificate: %w", err)
	}
	p.CertReplaced = replaced
	if cp.name != "" {
		if err := chown(layout.TLSKey(), cp.uid, cp.gid); err != nil {
			return Prepared{}, err
		}
	}
	cert, err := LoadCertificate(layout.TLSCert())
	if err != nil {
		return Prepared{}, err
	}
	p.Fingerprint = Fingerprint(cert)

	if p.AdminKey, _, err = ReadOrCreate(layout.AdminKey(), 0o600, func() string { return RandomHex(24) }); err != nil {
		return Prepared{}, fmt.Errorf("administration key: %w", err)
	}
	// Root, and the members of a group if there is one: the asp command reads it as one of them.
	if gid, ok := lookupGroup(opts.Group); ok && isRoot() {
		if err := chown(layout.AdminKey(), 0, gid); err != nil {
			return Prepared{}, err
		}
		if err := os.Chmod(layout.AdminKey(), 0o640); err != nil {
			return Prepared{}, err
		}
	}
	if p.NodeToken, _, err = ReadOrCreate(layout.NodeToken(), 0o600, func() string { return RandomHex(24) }); err != nil {
		return Prepared{}, fmt.Errorf("node token: %w", err)
	}
	// The secret of the node's local API is read by the node (root) and the control plane.
	if p.AgentToken, _, err = ReadOrCreate(layout.AgentToken(), 0o600, func() string { return RandomHex(32) }); err != nil {
		return Prepared{}, fmt.Errorf("agent token: %w", err)
	}
	if cp.name != "" {
		if err := chown(layout.AgentToken(), 0, cp.gid); err != nil {
			return Prepared{}, err
		}
		if err := os.Chmod(layout.AgentToken(), 0o640); err != nil {
			return Prepared{}, err
		}
	}

	port := portOf(opts.Listen)
	p.LocalURL = "https://" + net.JoinHostPort(localHost(hostOf(opts.Listen)), port)
	p.PublicURL = p.LocalURL
	if !isLoopback(hostOf(opts.Listen)) {
		p.PublicURL = "https://" + net.JoinHostPort(advertisedHost(hostOf(opts.Listen), hostname, opts.TLSSANs), port)
	}
	if opts.Profile == ProfileLab {
		free, err := freePort()
		if err != nil {
			return Prepared{}, err
		}
		p.AgentListen = "127.0.0.1:" + free
	}
	return p, nil
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localHost is the host the processes on this machine use: the loopback when the control plane
// listens on every address.
func localHost(listenHost string) string {
	switch strings.Trim(listenHost, "[]") {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return listenHost
}

// advertisedHost is the name another host is told: the first name the operator gave for the
// certificate, else the address it listens on, else this host's name.
func advertisedHost(listenHost, hostname string, sans []string) string {
	for _, s := range sans {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	switch strings.Trim(listenHost, "[]") {
	case "", "0.0.0.0", "::":
		return hostname
	}
	return listenHost
}

func freePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, nil
}

// CLIConfigText is the configuration of the asp command for this server.
func CLIConfigText(p Prepared) string {
	return "# Written by asp-server: how the asp command on this host reaches the control plane it runs.\n" +
		"control_plane_url: " + p.PublicURL + "\n" +
		"ca_file: " + p.Layout.TLSCert() + "\n" +
		"api_key_file: " + p.Layout.AdminKey() + "\n"
}

// writeCLIConfig leaves the configuration beside the server's files, and at path (the host's
// /etc/asp/asp.yaml) if nobody has put one there. It says where it did.
func writeCLIConfig(p Prepared, path string, log *slog.Logger) {
	text := CLIConfigText(p)
	if err := os.WriteFile(p.Layout.CLIConfig(), []byte(text), 0o644); err != nil {
		log.Warn("cannot write the CLI configuration", "path", p.Layout.CLIConfig(), "error", err)
	}
	if path == "-" || path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		switch {
		case err == nil:
			_, werr := f.WriteString(text)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				log.Warn("cannot write the CLI configuration", "path", path, "error", werr)
				return
			}
			log.Info("the asp command of this host is configured", "path", path)
			return
		case errors.Is(err, fs.ErrExist):
			log.Info("the asp command keeps its configuration", "path", path,
				"for_this_server", "ASP_CONFIG="+p.Layout.CLIConfig())
			return
		}
	}
	log.Info("to use the asp command on this host", "run", "export ASP_CONFIG="+p.Layout.CLIConfig())
}

// binDirs is where the packages put the programs (replaced by the tests).
var binDirs = []string{"/usr/bin", "/usr/local/bin"}

// findBinary resolves a program: the path given, else beside this executable, else the PATH,
// else where the packages put it.
func findBinary(given, name string) (string, error) {
	if given != "" {
		if _, err := os.Stat(given); err != nil {
			return "", fmt.Errorf("%s: %w", given, err)
		}
		return given, nil
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range binDirs {
		cand := filepath.Join(dir, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s not found: install it (the package of the same name), or name it with --%s", name, flagFor(name))
}

func flagFor(name string) string {
	if name == "asp-node-agent" {
		return "node-agent"
	}
	return "control-plane"
}

// waitHealthy waits until the control plane answers /healthz over its own certificate.
func waitHealthy(ctx context.Context, p Prepared) error {
	cert, err := LoadCertificate(p.Layout.TLSCert())
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	hc := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	defer hc.CloseIdleConnections()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.LocalURL+"/healthz", nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Run makes what the host needs, starts the control plane and, once it answers, the node-agent,
// and keeps both running until ctx ends. Then it stops the node-agent and the control plane, in
// that order. It returns nil for a stop that was asked for, and an error for a program that
// refused its settings or could not be started.
func Run(ctx context.Context, opts Options) error {
	opts.defaults()
	log := slog.New(slog.NewTextHandler(opts.Out, nil)).With("component", "asp-server")
	if err := opts.Validate(); err != nil {
		return err
	}
	p, err := Prepare(opts)
	if err != nil {
		return err
	}
	cpBin, err := findBinary(opts.ControlPlane, "asp-control-plane")
	if err != nil {
		return err
	}
	runAgent := !opts.NoAgent
	agentBin := ""
	if runAgent {
		if agentBin, err = findBinary(opts.NodeAgent, "asp-node-agent"); err != nil {
			return err
		}
		if opts.Profile == ProfileDefault && !opts.HasKVM() {
			log.Warn("no /dev/kvm: this host cannot run sandboxes, so it runs the control plane alone. "+
				"Other hosts can join it; --profile lab runs a node without VMs for trying things out", "node", p.NodeID)
			runAgent = false
		}
	}
	if p.CertReplaced {
		log.Warn("the TLS certificate did not cover a name asked for, so a new one was made: nodes that trusted the old one must be given this one",
			"sha256", p.Fingerprint)
	}
	writeCLIConfig(p, opts.CLIConfig, log)
	log.Info("single-host mode", "data_dir", p.Layout.Root, "url", p.PublicURL, "profile", opts.Profile,
		"database", "sqlite", "admin_key", p.Layout.AdminKey(), "tls_sha256", p.Fingerprint)

	cp := &proc{
		name: "control-plane", grace: 40 * time.Second, refusal: 2, out: opts.Out, log: log,
		minBackoff: time.Second, maxBackoff: 30 * time.Second,
		cmd: func() *exec.Cmd {
			c := exec.Command(cpBin)
			c.Dir = p.Layout.Root
			c.Env = ControlPlaneEnv(p, labEnv(opts))
			if a := p.ControlPlaneAccount; a.name != "" {
				c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid)}}
			}
			return c
		},
	}
	agent := &proc{
		name: "node-agent", grace: 40 * time.Second, refusal: 2, out: opts.Out, log: log,
		minBackoff: time.Second, maxBackoff: 30 * time.Second,
		cmd: func() *exec.Cmd {
			c := exec.Command(agentBin)
			c.Dir = p.Layout.Root
			c.Env = NodeAgentEnv(p, opts.Profile, labEnv(opts))
			return c
		},
	}

	// The programs end on their own only when they refuse their settings or cannot be started
	// (proc.run restarts the rest); either ends everything. They stop in order, the node first
	// and then the control plane it reports to, so each has a context of its own.
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	start := func(ctx context.Context, pr *proc) {
		go func() { results <- result{pr.name, pr.run(ctx)} }()
	}
	cpCtx, stopCP := context.WithCancel(context.Background())
	defer stopCP()
	agentCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	running := map[string]bool{cp.name: true}
	start(cpCtx, cp)

	// The node enrolls with the control plane, so it starts when that answers.
	healthy := make(chan error, 1)
	go func() { healthy <- waitHealthy(cpCtx, p) }()

	var first error
	note := func(r result) {
		running[r.name] = false
		if r.err != nil && first == nil {
			first = r.err
			if r.name == agent.name {
				first = fmt.Errorf("%w (sudo asp doctor says what this host lacks)", r.err)
			}
		}
	}
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case r := <-results:
			note(r)
			break wait
		case err := <-healthy:
			healthy = nil
			if err != nil {
				break wait
			}
			log.Info("the control plane answers", "url", p.LocalURL)
			switch {
			case runAgent:
				running[agent.name] = true
				start(agentCtx, agent)
			case opts.NoAgent:
				log.Info("--no-agent: no node runs on this host")
			}
		}
	}

	stopAgent()
	for running[agent.name] {
		note(<-results)
	}
	stopCP()
	for running[cp.name] {
		note(<-results)
	}
	return first
}

// labEnv is the environment both programs inherit. A lab keeps its keys wherever it was told to,
// including a temporary directory, which a production process refuses.
func labEnv(opts Options) []string {
	if opts.Profile != ProfileLab {
		return opts.Env
	}
	return withEnv(opts.Env, nil, map[string]string{"ASP_ALLOW_TMP_KEYS": "1"})
}
