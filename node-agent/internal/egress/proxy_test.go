package egress

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowlistCheck(t *testing.T) {
	a := NewAllowlist("api.github.com", "*.example.com")
	cases := []struct {
		host    string
		allowed bool
	}{
		{"api.github.com", true},
		{"API.GitHub.com", true},
		{"https://api.github.com/path", true},
		{"evil.com", false},
		{"foo.example.com", true},
		{"example.com", true},
		{"notexample.com", false},
		{"", false},
	}
	for _, tc := range cases {
		err := a.Check(tc.host)
		if tc.allowed && err != nil {
			t.Fatalf("host %q: want allow, got %v", tc.host, err)
		}
		if !tc.allowed && err != ErrDenied {
			t.Fatalf("host %q: want ErrDenied, got %v", tc.host, err)
		}
	}
}

func TestAllowlistGlobGithub(t *testing.T) {
	a := NewAllowlist("*.github.com")
	if err := a.Check("api.github.com"); err != nil {
		t.Fatal(err)
	}
	if err := a.Check("github.com"); err != nil {
		t.Fatal(err)
	}
	if err := a.Check("evil.com"); err != ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestAllowlistExact(t *testing.T) {
	a := NewAllowlist("registry.npmjs.org")
	if err := a.Check("registry.npmjs.org"); err != nil {
		t.Fatal(err)
	}
	if err := a.Check("npmjs.org"); err != ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestAllowlistPort(t *testing.T) {
	p443 := 443
	a := NewAllowlistFromPolicy("deny-default", []Rule{
		{HostPattern: "api.example.com", Port: &p443},
	})
	if err := a.CheckHostPort("api.example.com", 443); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckHostPort("api.example.com", 80); err != ErrDenied {
		t.Fatalf("port 80 should deny, got %v", err)
	}
	if err := a.CheckHostPort("api.example.com", 0); err != nil {
		t.Fatalf("port 0 should allow host match, got %v", err)
	}
}

func TestAllowAllMode(t *testing.T) {
	a := NewAllowlistFromPolicy("allow-all", nil)
	if err := a.Check("anything.example"); err != nil {
		t.Fatal(err)
	}
}

func TestDenyDefaultEmpty(t *testing.T) {
	a := NewAllowlistFromPolicy("deny-default", nil)
	if err := a.Check("anything.example"); err != ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestForwarderStub(t *testing.T) {
	a := NewAllowlist("allowed.test")
	f := &ForwarderStub{Allowlist: a}
	if err := f.AllowDial("allowed.test", 443); err != nil {
		t.Fatal(err)
	}
	if err := f.AllowDial("blocked.test", 443); err != ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestParseAllowlistJSON(t *testing.T) {
	al, err := ParseAllowlistJSON(`{"mode":"deny-default","rules":[{"host_pattern":"allowed.test","enabled":true}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := al.Check("allowed.test"); err != nil {
		t.Fatal(err)
	}
	if err := al.Check("blocked.test"); err != ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestForwardProxyAllowDeny(t *testing.T) {
	al := NewAllowlist("allowed.test")
	p := &ForwardProxy{Default: al, Enforce: true}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "blocked.test:443", nil)
	req.Host = "blocked.test:443"
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("deny CONNECT: want 403 got %d body=%s", rr.Code, rr.Body.String())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-body"))
	}))

	p2 := &ForwardProxy{
		Default: NewAllowlist("127.0.0.1"),
		Enforce: true,
	}
	url := "http://" + ln.Addr().String() + "/hello"
	req2 := httptest.NewRequest(http.MethodGet, url, nil)
	rr2 := httptest.NewRecorder()
	p2.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("allow HTTP: want 200 got %d body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "ok-body") {
		t.Fatalf("body=%q", rr2.Body.String())
	}

	req3 := httptest.NewRequest(http.MethodGet, "http://evil.example/", nil)
	rr3 := httptest.NewRecorder()
	p2.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusForbidden {
		t.Fatalf("deny HTTP: want 403 got %d", rr3.Code)
	}
}

func TestForwardProxyHeaderAllowlist(t *testing.T) {
	p := &ForwardProxy{
		Default: NewAllowlistFromPolicy("deny-default", nil),
		Enforce: true,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("via-header"))
	}))
	url := "http://" + ln.Addr().String() + "/"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(AllowlistHeader, `{"mode":"deny-default","rules":[{"host_pattern":"127.0.0.1","enabled":true}]}`)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPolicyCache(t *testing.T) {
	c := &PolicyCache{}
	c.Set(NewAllowlist("cached.test"))
	al := c.Get()
	if al == nil || al.Check("cached.test") != nil {
		t.Fatal("cache miss")
	}
}

func TestForwardProxyDenyNonHTTPScheme(t *testing.T) {
	p := &ForwardProxy{Default: NewAllowlist("evil"), Enforce: true}
	req := httptest.NewRequest(http.MethodGet, "ftp://evil/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rr.Code)
	}
}

func TestForwardProxyRateLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	p := &ForwardProxy{
		Default:   NewAllowlist("127.0.0.1"),
		Enforce:   true,
		RateLimit: NewTokenBucket(1, 1),
	}
	url := "http://" + ln.Addr().String() + "/"
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("first want 200 got %d", rr.Code)
	}
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, url, nil))
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second want 429 got %d", rr2.Code)
	}
}

func TestForwardProxyWithoutMITM(t *testing.T) {
	// Default path: MITM off; plain HTTP forward still works.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain"))
	}))
	p := &ForwardProxy{Default: NewAllowlist("127.0.0.1"), Enforce: true}
	url := "http://" + ln.Addr().String() + "/"
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "plain") {
		t.Fatalf("got %d %q", rr.Code, rr.Body.String())
	}
}

func TestTokenBucket(t *testing.T) {
	b := NewTokenBucket(100, 2)
	if !b.Allow("a") || !b.Allow("a") {
		t.Fatal("burst")
	}
	if b.Allow("a") {
		t.Fatal("should empty")
	}
	if !b.Allow("b") {
		t.Fatal("other key")
	}
}
