package egress

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testGuard lets the other proxy tests reach their loopback upstreams. The
// default guard refuses loopback, which is what this file checks.
var testGuard = &DialGuard{AllowLoopback: true}

// allowServer is an allowlist that lets through the host and port of an
// upstream test server, given as a URL or as host:port. A rule without a port
// means 80 and 443, which a test server never listens on.
func allowServer(t *testing.T, addr string) *Allowlist {
	t.Helper()
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	host, portStr, err := net.SplitHostPort(strings.TrimSuffix(addr, "/"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return NewAllowlistFromPolicy("deny-default", []Rule{{HostPattern: host, Port: &port}})
}

func TestDialGuardCheck(t *testing.T) {
	g := &DialGuard{
		AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("10.50.0.0/16"), netip.MustParsePrefix("100.64.1.0/24")},
		Never:      []netip.Prefix{netip.MustParsePrefix("10.200.0.0/16"), netip.MustParsePrefix("10.50.9.0/24")},
		LocalAddrs: func() []netip.Addr {
			return []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}
		},
	}
	for _, tc := range []struct {
		ip      string
		blocked string // substring of the reason; "" means allowed
	}{
		// Public addresses.
		{"93.184.216.34", ""},
		{"2606:2800:220:1:248:1893:25c8:1946", ""},
		// Never, whatever the operator allows.
		{"127.0.0.1", "loopback"},
		{"127.1.2.3", "loopback"},
		{"::1", "loopback"},
		{"::ffff:127.0.0.1", "loopback"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified"},
		{"169.254.169.254", "link-local"},
		{"fe80::1", "link-local"},
		{"224.0.0.251", "multicast"},
		{"ff02::fb", "multicast"},
		{"0.1.2.3", "reserved"},
		{"198.18.0.5", "reserved"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "reserved"},
		{"64:ff9b::a00:1", "reserved"},
		{"fec0::1", "reserved"},
		{"203.0.113.7", "this node"},
		{"2001:db8::7", "this node"},
		{"::ffff:203.0.113.7", "this node"},
		// The guests' network is refused even inside an allowed prefix.
		{"10.200.3.2", "guests"},
		{"10.50.9.4", "guests"},
		// Private space is refused unless the operator opened it.
		{"10.0.0.5", "private"},
		{"172.16.4.4", "private"},
		{"192.168.1.10", "private"},
		{"fd00::5", "private"},
		{"100.64.0.9", "private"},
		{"10.50.1.2", ""},
		{"100.64.1.77", ""},
	} {
		err := g.Check(netip.MustParseAddr(tc.ip))
		switch {
		case tc.blocked == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.ip, err)
		case tc.blocked != "" && err == nil:
			t.Errorf("%s: allowed, want it refused as %q", tc.ip, tc.blocked)
		case err != nil:
			if !errors.Is(err, ErrDestinationBlocked) || !strings.Contains(err.Error(), tc.blocked) {
				t.Errorf("%s: error %q does not say %q", tc.ip, err, tc.blocked)
			}
		}
	}
}

func TestDialGuardLoopbackOnlyForTests(t *testing.T) {
	g := &DialGuard{AllowLoopback: true, LocalAddrs: func() []netip.Addr { return nil }}
	if err := g.Check(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	// Allowing loopback does not open the rest.
	if err := g.Check(netip.MustParseAddr("169.254.169.254")); err == nil {
		t.Fatal("metadata address allowed")
	}
}

func TestParseCIDRs(t *testing.T) {
	got, err := ParseCIDRs(" 10.50.0.0/16, 192.168.1.9 ,fd00::/8 ")
	if err != nil || len(got) != 3 || got[0].String() != "10.50.0.0/16" || got[1].String() != "192.168.1.9/32" || got[2].String() != "fd00::/8" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseCIDRs(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "10.0.0.1/8/9"} {
		if _, err := ParseCIDRs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The dialer checks the address it is about to connect to, after resolving the
// name, so an allowed name that points at the node is refused.
func TestDialGuardRefusesANameThatResolvesToLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	g := &DialGuard{}
	for _, target := range []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port} {
		conn, err := g.DialContext(t.Context(), "tcp", target)
		if err == nil {
			conn.Close()
			t.Fatalf("connected to %s", target)
		}
		if !errors.Is(err, ErrDestinationBlocked) {
			t.Errorf("%s: %v does not wrap ErrDestinationBlocked", target, err)
		}
	}
	// The guard that allows loopback connects.
	conn, err := (&DialGuard{AllowLoopback: true}).DialContext(t.Context(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("loopback-allowing guard: %v", err)
	}
	conn.Close()
	if _, err := g.DialContext(t.Context(), "udp", "127.0.0.1:53"); !errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("udp: %v", err)
	}
}

// The scenario of #98: a tenant rule that names a host the guest controls, and
// the proxy asked to open a connection to the node's own loopback API.
func TestForwardProxyDoesNotReachTheNodeThroughAnAllowedName(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "node-local-api")
	}))
	defer secret.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(secret.URL, "http://"))

	// The default guard (no Guard field), a wildcard-ish rule for `localhost`,
	// which stands for any name an attacker points at 127.0.0.1.
	p := &ForwardProxy{Default: NewAllowlist("localhost", "127.0.0.1"), Enforce: true}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// Plain HTTP through the proxy.
	for _, target := range []string{"http://localhost:" + port + "/", "http://127.0.0.1:" + port + "/"} {
		proxyURL, _ := url.Parse(srv.URL)
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), "node-local-api") {
			t.Fatalf("GET %s through the proxy: %d %q", target, resp.StatusCode, body)
		}
	}

	// CONNECT.
	for _, target := range []string{"localhost:" + port, "127.0.0.1:" + port} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Close()
		if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 403") {
			t.Fatalf("CONNECT %s: %q", target, buf[:n])
		}
	}
}
