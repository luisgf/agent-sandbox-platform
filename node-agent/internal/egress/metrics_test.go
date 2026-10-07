package egress

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
)

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Every decision of the proxy is counted under the tenant of the sandbox that
// asked, and the bytes it carried both ways.
func TestProxyCountsDecisionsByTenantAndBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("r"), 3000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	reg := metrics.NewRegistry()
	cache := &PolicyCache{}
	cache.Bind("sb-1", netip.MustParsePrefix("127.0.0.0/8")) // the test client is on loopback
	cache.Set("sb-1", allowServer(t, upstream.URL))
	cache.SetTenant("sb-1", "acme")
	p := &ForwardProxy{Guard: testGuard, Cache: cache, Enforce: true, Metrics: NewMetrics(reg)}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Post(upstream.URL+"/ok", "text/plain", strings.NewReader(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	resp, err = client.Get("http://denied.example/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied host: %d", resp.StatusCode)
	}

	out := scrape(t, reg)
	for _, want := range []string{
		`asp_agent_egress_requests_total{decision="allow",reason="http",tenant="acme"} 1`,
		`asp_agent_egress_requests_total{decision="deny",reason="allowlist",tenant="acme"} 1`,
		`asp_agent_egress_bytes_total{direction="from_upstream"} 3000`,
		`asp_agent_egress_bytes_total{direction="to_upstream"} 100`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A source that is not a sandbox is counted as tenant "none", whatever its address:
// the label cannot be made to grow by whoever sends the traffic.
func TestProxyCountsUnknownSourcesUnderNone(t *testing.T) {
	reg := metrics.NewRegistry()
	p := &ForwardProxy{Guard: testGuard, Default: NewAllowlistFromPolicy("deny-default", nil), Enforce: true, Metrics: NewMetrics(reg)}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://nope.example/", nil)
	req.RemoteAddr = "203.0.113.9:4444"
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status %d", rr.Code)
	}
	if out := scrape(t, reg); !strings.Contains(out, `decision="deny",reason="allowlist",tenant="none"} 1`) || strings.Contains(out, "203.0.113.9") {
		t.Fatalf("metrics:\n%s", out)
	}
}

func TestDNSSinkCountsQueriesByDecision(t *testing.T) {
	reg := metrics.NewRegistry()
	s := newTestSink(fakeSinkLookup)
	s.Metrics = NewMetrics(reg)
	s.Cache = &PolicyCache{}
	s.Cache.Bind("sb-1", netip.MustParsePrefix("192.0.2.0/24"))
	s.Cache.SetTenant("sb-1", "acme")
	s.Cache.Set("sb-1", NewAllowlist("v4only.example", "flaky.example", "gone.example"))
	s.answer(sinkQuery(1, "v4only.example", dnsTypeA), sinkGuest)  // allow
	s.answer(sinkQuery(2, "evil.example", dnsTypeA), sinkGuest)    // deny
	s.answer(sinkQuery(3, "flaky.example", dnsTypeA), sinkGuest)   // error (the resolver timed out)
	s.answer(sinkQuery(4, "evil.example", dnsTypeAAAA), sinkGuest) // deny
	out := scrape(t, reg)
	for _, want := range []string{
		`asp_agent_egress_dns_queries_total{decision="allow",tenant="acme"} 1`,
		`asp_agent_egress_dns_queries_total{decision="deny",tenant="acme"} 2`,
		`asp_agent_egress_dns_queries_total{decision="error",tenant="acme"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNilMetricsCountNothing(t *testing.T) {
	var m *Metrics
	m.request("allow", "http", "t")
	m.add("to_upstream", 5)
	m.query("allow", "t")
	if tenantLabel(nil, "x") != "none" {
		t.Fatal("no cache, no tenant")
	}
}

func TestPolicyCacheTenant(t *testing.T) {
	c := &PolicyCache{}
	c.SetTenant("sb", "acme")
	if c.TenantOf("sb") != "acme" || c.TenantOf("other") != "" {
		t.Fatal("tenant lookup")
	}
	c.Forget("sb")
	if c.TenantOf("sb") != "" {
		t.Fatal("Forget keeps the tenant")
	}
	var nilCache *PolicyCache
	nilCache.SetTenant("sb", "x")
	if nilCache.TenantOf("sb") != "" {
		t.Fatal("nil cache")
	}
}
