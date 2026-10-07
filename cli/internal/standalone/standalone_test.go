package standalone

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for asp-control-plane and asp-node-agent: Run starts it
// with the environment it would give them, and the role is whichever of the two settings it finds.
func TestMain(m *testing.M) {
	if os.Getenv("STANDALONE_HELPER") == "1" {
		os.Exit(helper())
	}
	os.Exit(m.Run())
}

// helper plays a program Run starts. It notes in STANDALONE_MARKS when it starts and stops, so
// the test sees the order.
func helper() int {
	marks := os.Getenv("STANDALONE_MARKS")
	mark := func(what string) {
		f, err := os.OpenFile(filepath.Join(marks, "marks"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%s\n", what)
			f.Close()
		}
	}
	term := make(chan os.Signal, 1)
	signalNotify(term)
	if addr := os.Getenv("ASP_LISTEN_ADDR"); addr != "" { // the control plane
		if os.Getenv("STANDALONE_CP_REFUSES") == "1" {
			fmt.Fprintln(os.Stderr, "refusing: a setting is wrong")
			return 2
		}
		cert, err := tls.LoadX509KeyPair(os.Getenv("ASP_TLS_CERT"), os.Getenv("ASP_TLS_KEY"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
		srv := &http.Server{Addr: addr, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
		go srv.ListenAndServeTLS("", "")
		mark("control-plane started")
		<-term
		mark("control-plane stopped")
		return 0
	}
	mark("node-agent started")
	<-term
	mark("node-agent stopped")
	return 0
}

func signalNotify(c chan os.Signal) { notifyTerm(c) }

func helperOptions(t *testing.T, dir string, extra ...string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return Options{
		DataDir:      filepath.Join(dir, "asp"),
		Listen:       addr,
		ControlPlane: exe,
		NodeAgent:    exe,
		Profile:      ProfileLab,
		User:         "no-such-user",
		Group:        "no-such-group",
		CLIConfig:    "-",
		Env:          append(append(os.Environ(), "STANDALONE_HELPER=1", "STANDALONE_MARKS="+dir), extra...),
		Out:          &syncBuffer{},
		Hostname:     func() (string, error) { return "testhost.example", nil },
		Addrs:        func() ([]net.Addr, error) { return nil, nil },
	}
}

// syncBuffer is a log that two goroutines write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func readMarks(t *testing.T, dir string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "marks"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ";"))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func marksHave(dir string, want ...string) func() bool {
	return func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "marks"))
		text := string(b)
		for _, w := range want {
			if !strings.Contains(text, w) {
				return false
			}
		}
		return true
	}
}

func TestNodeIDFor(t *testing.T) {
	for in, want := range map[string]string{
		"Node1": "node1", "my_host": "my-host", "  ": "node", "ncc1701d.example": "ncc1701d.example", "-x-": "x",
	} {
		if got := nodeIDFor(in); got != want {
			t.Errorf("nodeIDFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadOrCreateAgreesBetweenCreators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	var wg sync.WaitGroup
	got := make([]string, 12)
	made := make([]bool, 12)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, created, err := ReadOrCreate(path, 0o600, func() string { return RandomHex(16) })
			if err != nil {
				t.Error(err)
			}
			got[i], made[i] = v, created
		}(i)
	}
	wg.Wait()
	creators := 0
	for i := range got {
		if got[i] != got[0] || got[i] == "" {
			t.Fatalf("creator %d read %q, the first read %q", i, got[i], got[0])
		}
		if made[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d creators made the file", creators)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	// An empty file is made again, not kept.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, created, err := ReadOrCreate(path, 0o600, func() string { return "fresh" }); err != nil || !created || v != "fresh" {
		t.Fatalf("empty file: %q %v %v", v, created, err)
	}
}

func TestNamesForLoopbackWildcardAndOperatorNames(t *testing.T) {
	addrs := func() ([]net.Addr, error) {
		_, a, _ := net.ParseCIDR("192.0.2.7/24")
		a.IP = net.ParseIP("192.0.2.7")
		_, ll, _ := net.ParseCIDR("169.254.1.1/16")
		ll.IP = net.ParseIP("169.254.1.1")
		return []net.Addr{a, ll}, nil
	}
	n, err := NamesFor("127.0.0.1", "Box.Example", []string{"asp.example", "198.51.100.9"}, addrs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.DNS, ",") != "asp.example,box.example,localhost" {
		t.Errorf("dns %v", n.DNS)
	}
	hasIP := func(want string) bool {
		for _, ip := range n.IPs {
			if ip.String() == want {
				return true
			}
		}
		return false
	}
	if !hasIP("127.0.0.1") || !hasIP("::1") || !hasIP("198.51.100.9") || hasIP("192.0.2.7") {
		t.Errorf("a loopback listener names the loopback and the operator's addresses only: %v", n.IPs)
	}
	// Listening on every address names every address of the host (not the link-local ones).
	w, err := NamesFor("0.0.0.0", "box", nil, addrs)
	if err != nil {
		t.Fatal(err)
	}
	found, ll := false, false
	for _, ip := range w.IPs {
		found = found || ip.String() == "192.0.2.7"
		ll = ll || ip.String() == "169.254.1.1"
	}
	if !found || ll {
		t.Errorf("wildcard: %v", w.IPs)
	}
}

func TestEnsureCertificateKeepsOneThatCoversAndReplacesOneThatDoesNot(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	now := time.Now()
	names, _ := NamesFor("127.0.0.1", "box", nil, func() ([]net.Addr, error) { return nil, nil })
	replaced, err := EnsureCertificate(cert, key, names, now)
	if err != nil || replaced {
		t.Fatalf("first: replaced=%v err=%v", replaced, err)
	}
	first, err := LoadCertificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	// The same names: kept.
	if replaced, err = EnsureCertificate(cert, key, names, now); err != nil || replaced {
		t.Fatalf("same names: replaced=%v err=%v", replaced, err)
	}
	same, _ := LoadCertificate(cert)
	if Fingerprint(same) != Fingerprint(first) {
		t.Fatal("a certificate that covers the names was replaced")
	}
	// A name more: replaced, and said.
	more, _ := NamesFor("127.0.0.1", "box", []string{"asp.example"}, func() ([]net.Addr, error) { return nil, nil })
	if replaced, err = EnsureCertificate(cert, key, more, now); err != nil || !replaced {
		t.Fatalf("new name: replaced=%v err=%v", replaced, err)
	}
	next, _ := LoadCertificate(cert)
	if Fingerprint(next) == Fingerprint(first) || !more.Covers(next, now) {
		t.Fatal("the new certificate does not cover the name")
	}
	// The certificate is good for a TLS client that trusts it, by name and by address.
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if pair.Leaf == nil && len(pair.Certificate) == 0 {
		t.Fatal("no certificate in the pair")
	}
	// One that expires within a month is not kept.
	if more.Covers(next, now.Add(10*365*24*time.Hour)) {
		t.Error("an expiring certificate counts as covering")
	}
}

func TestPrepareMakesTheHostOnceAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		DataDir: filepath.Join(dir, "asp"), Listen: "127.0.0.1:18443", Profile: ProfileDefault,
		User: "no-such-user", Group: "no-such-group",
		Hostname: func() (string, error) { return "Box.Example", nil },
		Addrs:    func() ([]net.Addr, error) { return nil, nil },
	}
	p, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeID != "box" || p.LocalURL != "https://127.0.0.1:18443" || p.PublicURL != p.LocalURL {
		t.Errorf("ids and urls: %+v", p)
	}
	if len(p.AdminKey) != 48 || len(p.NodeToken) != 48 || len(p.AgentToken) != 64 || p.AdminKey == p.NodeToken {
		t.Errorf("secrets: %d %d %d", len(p.AdminKey), len(p.NodeToken), len(p.AgentToken))
	}
	l := p.Layout
	for path, want := range map[string]os.FileMode{
		l.Root: 0o755, l.Server(): 0o711, l.Disks(): 0o700, l.NodeCerts(): 0o700, l.LocalNet(): 0o700,
		l.AdminKey(): 0o600, l.NodeToken(): 0o600, l.AgentToken(): 0o600, l.TLSKey(): 0o600, l.TLSCert(): 0o644,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", path, fi.Mode().Perm(), want)
		}
	}

	again, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.AdminKey != p.AdminKey || again.NodeToken != p.NodeToken || again.AgentToken != p.AgentToken ||
		again.Fingerprint != p.Fingerprint || again.CertReplaced {
		t.Errorf("a second Prepare changed things: %+v vs %+v", again, p)
	}
	opts.TLSSANs = []string{"asp.example"}
	third, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !third.CertReplaced || third.Fingerprint == p.Fingerprint || third.AdminKey != p.AdminKey {
		t.Errorf("a new name: replaced=%v, keys kept=%v", third.CertReplaced, third.AdminKey == p.AdminKey)
	}
}

func TestPrepareAdvertisesTheNameAnotherHostUses(t *testing.T) {
	dir := t.TempDir()
	base := Options{DataDir: filepath.Join(dir, "a"), Listen: "0.0.0.0:8443", User: "x", Group: "x",
		Hostname: func() (string, error) { return "box.example", nil },
		Addrs:    func() ([]net.Addr, error) { return nil, nil }}
	p, err := Prepare(base)
	if err != nil {
		t.Fatal(err)
	}
	if p.LocalURL != "https://127.0.0.1:8443" || p.PublicURL != "https://box.example:8443" {
		t.Errorf("wildcard: %s %s", p.LocalURL, p.PublicURL)
	}
	base.DataDir, base.TLSSANs = filepath.Join(dir, "b"), []string{"asp.example"}
	if p, err = Prepare(base); err != nil || p.PublicURL != "https://asp.example:8443" {
		t.Errorf("with a name: %v %v", p.PublicURL, err)
	}
	base.DataDir, base.Listen, base.TLSSANs = filepath.Join(dir, "c"), "[::]:9443", nil
	if p, err = Prepare(base); err != nil || p.PublicURL != "https://box.example:9443" {
		t.Errorf("ipv6 wildcard: %v %v", p.PublicURL, err)
	}
	for _, bad := range []string{"8443", "", "host:"} {
		base.Listen = bad
		if bad == "" {
			continue // the default
		}
		if _, err := Prepare(base); err == nil {
			t.Errorf("--listen %q was accepted", bad)
		}
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestControlPlaneEnvironment(t *testing.T) {
	p := Prepared{Layout: Layout{Root: "/var/lib/asp"}, Listen: "127.0.0.1:8443", AdminKey: "adminkey", NodeToken: "nodetoken"}
	env := envMap(ControlPlaneEnv(p, []string{"PATH=/bin", "ASP_LISTEN_ADDR=0.0.0.0:1", "ASP_BOOTSTRAP_API_KEY=other"}))
	want := map[string]string{
		"ASP_LISTEN_ADDR": "127.0.0.1:8443", "ASP_TLS_CERT": "/var/lib/asp/server/tls.crt", "ASP_TLS_KEY": "/var/lib/asp/server/tls.key",
		"ASP_CLIENT_CA": "/var/lib/asp/server/ca.crt", "ASP_CA_CERT": "/var/lib/asp/server/ca.crt", "ASP_CA_KEY": "/var/lib/asp/server/ca.key",
		"ASP_OIDC_KEY": "/var/lib/asp/server/oidc-key.pem", "ASP_ATTEST_KEY": "/var/lib/asp/server/attest-key.pem",
		"ASP_BOOTSTRAP_API_KEY": "adminkey", "ASP_NODE_BOOTSTRAP_TOKEN": "nodetoken", "ASP_AGENT_TOKEN_FILE": "/var/lib/asp/agent.token",
		"ASP_DATABASE_URL": "sqlite:///var/lib/asp/server/asp.db", "PATH": "/bin",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// A Postgres the operator pointed it at stays.
	env = envMap(ControlPlaneEnv(p, []string{"ASP_DATABASE_URL=postgres://asp@db/asp"}))
	if env["ASP_DATABASE_URL"] != "postgres://asp@db/asp" {
		t.Errorf("the operator's database was replaced: %q", env["ASP_DATABASE_URL"])
	}
}

func TestNodeAgentEnvironment(t *testing.T) {
	p := Prepared{Layout: Layout{Root: "/var/lib/asp"}, LocalURL: "https://127.0.0.1:8443", NodeID: "box", NodeToken: "nodetoken"}
	env := envMap(NodeAgentEnv(p, ProfileDefault, nil))
	for k, v := range map[string]string{
		"ASP_CONTROL_PLANE_URL": "https://127.0.0.1:8443", "ASP_CONTROL_PLANE_CA": "/var/lib/asp/server/tls.crt",
		"ASP_NODE_ID": "box", "ASP_ENROLL": "1", "ASP_NODE_BOOTSTRAP_TOKEN": "nodetoken", "ASP_MTLS": "1",
		"ASP_CERT_DIR": "/var/lib/asp/node-certs", "ASP_DISK_DIR": "/var/lib/asp/disks", "ASP_LOCAL_NET_KEY_DIR": "/var/lib/asp/local-net",
		"ASP_AGENT_TOKEN_FILE": "/var/lib/asp/agent.token", "ASP_RECONCILE": "1",
		"ASP_CH_SOCKET_DIR": "/run/asp", "ASP_TAP_AUTO": "1", "ASP_HOST_VSOCK": "1", "ASP_EGRESS_ENFORCE": "1",
	} {
		if env[k] != v {
			t.Errorf("default %s = %q, want %q", k, env[k], v)
		}
	}
	if env["ASP_DRY_RUN"] != "" {
		t.Error("the default profile runs VMs")
	}
	// A site's value for what has a default stays; what asp-server decides does not move.
	env = envMap(NodeAgentEnv(p, ProfileDefault, []string{"ASP_CH_SOCKET_DIR=/run/mine", "ASP_NODE_ID=other"}))
	if env["ASP_CH_SOCKET_DIR"] != "/run/mine" || env["ASP_NODE_ID"] != "box" {
		t.Errorf("precedence: %q %q", env["ASP_CH_SOCKET_DIR"], env["ASP_NODE_ID"])
	}
	p.AgentListen = "127.0.0.1:19999"
	lab := envMap(NodeAgentEnv(p, ProfileLab, nil))
	if lab["ASP_DRY_RUN"] != "1" || lab["ASP_CH_SOCKET_DIR"] != "/var/lib/asp/run" || lab["ASP_AGENT_LISTEN"] != "127.0.0.1:19999" ||
		lab["ASP_EGRESS_ENFORCE"] != "" || lab["ASP_TAP_AUTO"] != "" {
		t.Errorf("lab: %v", lab)
	}
}

func TestCLIConfigIsLeftOnceAndNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	p, err := Prepare(Options{DataDir: filepath.Join(dir, "asp"), Listen: "127.0.0.1:8443", User: "x", Group: "x",
		Hostname: func() (string, error) { return "box", nil }, Addrs: func() ([]net.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	log := newLogger(out)
	hostPath := filepath.Join(dir, "etc", "asp", "asp.yaml")
	writeCLIConfig(p, hostPath, log)
	b, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"control_plane_url: https://127.0.0.1:8443", "ca_file: " + p.Layout.TLSCert(), "api_key_file: " + p.Layout.AdminKey()} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), p.AdminKey) {
		t.Error("the key itself is in a file everyone can read")
	}
	if _, err := os.Stat(p.Layout.CLIConfig()); err != nil {
		t.Errorf("the config beside the server's files: %v", err)
	}
	// Somebody's own file is not replaced.
	if err := os.WriteFile(hostPath, []byte("tenant: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCLIConfig(p, hostPath, log)
	if b, _ := os.ReadFile(hostPath); string(b) != "tenant: mine\n" {
		t.Errorf("an existing config was replaced:\n%s", b)
	}
	if !strings.Contains(out.String(), "ASP_CONFIG="+p.Layout.CLIConfig()) {
		t.Errorf("it does not say how to use the server's own config:\n%s", out.String())
	}
}

func TestAProcessIsRestartedAndStoppedAndKilled(t *testing.T) {
	out := &syncBuffer{}
	log := newLogger(out)
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")

	// Dies at once: restarted with a backoff, until the context ends.
	restarting := &proc{name: "flaky", grace: time.Second, out: out, log: log, minBackoff: 10 * time.Millisecond, maxBackoff: 20 * time.Millisecond,
		cmd: func() *exec.Cmd {
			return exec.Command("sh", "-c", "echo run >> "+counter+"; echo hello from flaky; exit 1")
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- restarting.run(ctx) }()
	waitFor(t, "three runs", func() bool {
		b, _ := os.ReadFile(counter)
		return strings.Count(string(b), "run") >= 3
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a stop that was asked for is an error: %v", err)
	}
	if !strings.Contains(out.String(), "[flaky] hello from flaky") {
		t.Errorf("the output of the process is not in the log with its name:\n%s", out.String())
	}

	// Refuses its settings: not restarted.
	refusing := &proc{name: "picky", grace: time.Second, refusal: 2, out: out, log: log, minBackoff: time.Millisecond, maxBackoff: time.Millisecond,
		cmd: func() *exec.Cmd { return exec.Command("sh", "-c", "echo bad setting >&2; exit 2") }}
	err := refusing.run(context.Background())
	var refused errRefused
	if !asRefused(err, &refused) || refused.code != 2 || !strings.Contains(out.String(), "[picky] bad setting") {
		t.Fatalf("a refusal: %v", err)
	}

	// Ignores the polite request: killed after its grace.
	stubborn := &proc{name: "stubborn", grace: 200 * time.Millisecond, out: out, log: log, minBackoff: time.Millisecond, maxBackoff: time.Millisecond,
		cmd: func() *exec.Cmd {
			return exec.Command("sh", "-c", "trap '' TERM; echo ready; while :; do sleep 0.05; done")
		}}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- stubborn.run(ctx) }()
	waitFor(t, "the process to be up", func() bool { return strings.Contains(out.String(), "[stubborn] ready") })
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a process that ignores SIGTERM was never killed")
	}
	if took := time.Since(started); took < 150*time.Millisecond {
		t.Errorf("it was killed before its grace: %v", took)
	}

	// A program that is not there is an error, not a restart loop.
	missing := &proc{name: "missing", grace: time.Second, out: out, log: log, minBackoff: time.Millisecond, maxBackoff: time.Millisecond,
		cmd: func() *exec.Cmd { return exec.Command(filepath.Join(dir, "no-such-program")) }}
	if err := missing.run(context.Background()); err == nil {
		t.Error("a missing program was not an error")
	}
}

func asRefused(err error, target *errRefused) bool {
	e, ok := err.(errRefused)
	if ok {
		*target = e
	}
	return ok
}

// Run makes the host, starts the control plane, starts the node when the control plane
// answers, and stops them in the other order.
func TestRunStartsTheNodeAfterTheControlPlaneAndStopsItFirst(t *testing.T) {
	dir := t.TempDir()
	opts := helperOptions(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()

	waitFor(t, "both programs", marksHave(dir, "control-plane started", "node-agent started"))
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v\nlog:\n%s", err, opts.Out.(*syncBuffer).String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the stop")
	}
	marks := strings.Join(readMarks(t, dir), " ")
	order := []string{"control-plane started", "node-agent started", "node-agent stopped", "control-plane stopped"}
	text := func() string { b, _ := os.ReadFile(filepath.Join(dir, "marks")); return string(b) }()
	last := -1
	for _, o := range order {
		i := strings.Index(text, o)
		if i < 0 || i < last {
			t.Fatalf("order of events (%s): want %v\n%s", marks, order, text)
		}
		last = i
	}
	log := opts.Out.(*syncBuffer).String()
	if !strings.Contains(log, "the control plane answers") || !strings.Contains(log, "single-host mode") {
		t.Errorf("the log does not tell what happened:\n%s", log)
	}
}

func TestRunStopsWhenTheControlPlaneRefusesItsSettings(t *testing.T) {
	dir := t.TempDir()
	opts := helperOptions(t, dir, "STANDALONE_CP_REFUSES=1")
	err := Run(context.Background(), opts)
	var refused errRefused
	if !asRefused(err, &refused) || refused.name != "control-plane" {
		t.Fatalf("Run returned %v", err)
	}
	if strings.Contains(string(mustRead(filepath.Join(dir, "marks"))), "node-agent started") {
		t.Error("the node was started with no control plane")
	}
}

func TestRunWithNoAgentStartsOnlyTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	opts := helperOptions(t, dir)
	opts.NoAgent = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	waitFor(t, "the control plane", marksHave(dir, "control-plane started"))
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustRead(filepath.Join(dir, "marks"))), "node-agent") {
		t.Error("--no-agent started a node")
	}
	if !strings.Contains(opts.Out.(*syncBuffer).String(), "--no-agent") {
		t.Error("it does not say that no node runs")
	}
}

func TestADefaultHostWithoutKVMRunsTheControlPlaneAlone(t *testing.T) {
	dir := t.TempDir()
	opts := helperOptions(t, dir)
	opts.Profile = ProfileDefault
	opts.HasKVM = func() bool { return false }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	waitFor(t, "the control plane", marksHave(dir, "control-plane started"))
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustRead(filepath.Join(dir, "marks"))), "node-agent") {
		t.Error("a node was started on a host with no KVM")
	}
	if log := opts.Out.(*syncBuffer).String(); !strings.Contains(log, "no /dev/kvm") || !strings.Contains(log, "--profile lab") {
		t.Errorf("it does not say why, or what to do:\n%s", log)
	}
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func TestFindBinary(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "asp-control-plane")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findBinary(prog, "asp-control-plane"); err != nil || got != prog {
		t.Errorf("named: %q %v", got, err)
	}
	if _, err := findBinary(filepath.Join(dir, "nope"), "asp-control-plane"); err == nil {
		t.Error("a named program that is not there was accepted")
	}
	t.Setenv("PATH", dir)
	if got, err := findBinary("", "asp-control-plane"); err != nil || got != prog {
		t.Errorf("on the PATH: %q %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	old := binDirs
	binDirs = []string{t.TempDir()}
	defer func() { binDirs = old }()
	if _, err := findBinary("", "asp-node-agent"); err == nil || !strings.Contains(err.Error(), "--node-agent") {
		t.Errorf("not found should name the flag: %v", err)
	}
}

func TestWaitHealthyWaitsForTheCertificateToBeAnswered(t *testing.T) {
	dir := t.TempDir()
	p, err := Prepare(Options{DataDir: filepath.Join(dir, "asp"), Listen: "127.0.0.1:0", User: "x", Group: "x",
		Hostname: func() (string, error) { return "box", nil }, Addrs: func() ([]net.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.LocalURL = "https://" + ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := waitHealthy(ctx, p); err == nil {
		t.Fatal("nothing listens and it was healthy")
	}
	pair, err := tls.LoadX509KeyPair(p.Layout.TLSCert(), p.Layout.TLSKey())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Addr: strings.TrimPrefix(p.LocalURL, "https://"), TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })}
	go srv.ListenAndServeTLS("", "")
	defer srv.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := waitHealthy(ctx2, p); err != nil {
		t.Fatalf("a control plane answering over its own certificate: %v", err)
	}
}
