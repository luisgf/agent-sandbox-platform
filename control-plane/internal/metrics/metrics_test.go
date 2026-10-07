package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCounterGaugeExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("asp_things_total", "Things done.", "kind", "result")
	g := r.Gauge("asp_level", "A level.")
	c.Inc("a", "ok")
	c.Inc("a", "ok")
	c.Add(2.5, "b", "failed")
	c.Add(-1, "b", "failed") // a counter never goes down
	g.Set(7)
	g.Add(-2)

	want := `# HELP asp_level A level.
# TYPE asp_level gauge
asp_level 5
# HELP asp_metrics_dropped_series_total Observations dropped because a metric reached its series bound.
# TYPE asp_metrics_dropped_series_total counter
asp_metrics_dropped_series_total 0
# HELP asp_things_total Things done.
# TYPE asp_things_total counter
asp_things_total{kind="a",result="ok"} 2
asp_things_total{kind="b",result="failed"} 2.5
`
	if got := render(t, r); got != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", got, want)
	}
}

func TestHistogramExposition(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("asp_took_seconds", "How long.", []float64{1, 0.1}, "route")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(3, "x")
	got := render(t, r)
	for _, want := range []string{
		"# TYPE asp_took_seconds histogram\n",
		`asp_took_seconds_bucket{route="x",le="0.1"} 1` + "\n",
		`asp_took_seconds_bucket{route="x",le="1"} 2` + "\n",
		`asp_took_seconds_bucket{route="x",le="+Inf"} 3` + "\n",
		`asp_took_seconds_sum{route="x"} 3.55` + "\n",
		`asp_took_seconds_count{route="x"} 3` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Buckets come out in order, the +Inf one last, before the sum and the count.
	if strings.Index(got, `le="0.1"`) > strings.Index(got, `le="1"`) || strings.Index(got, `le="1"`) > strings.Index(got, `le="+Inf"`) ||
		strings.Index(got, `le="+Inf"`) > strings.Index(got, "_sum") {
		t.Errorf("buckets out of order:\n%s", got)
	}
}

func TestLabelAndHelpEscaping(t *testing.T) {
	r := NewRegistry()
	r.Counter("asp_esc_total", "Back\\slash and\nnewline.", "v").Inc("a\"b\\c\nd")
	got := render(t, r)
	if !strings.Contains(got, `# HELP asp_esc_total Back\\slash and\nnewline.`) {
		t.Errorf("help not escaped:\n%s", got)
	}
	if !strings.Contains(got, `asp_esc_total{v="a\"b\\c\nd"} 1`) {
		t.Errorf("label value not escaped:\n%s", got)
	}
}

// A metric used wrongly never takes the process down: it is a no-op.
func TestNilAndMismatchedUseIsHarmless(t *testing.T) {
	var c *Counter
	var g *Gauge
	var h *Histogram
	var r *Registry
	c.Inc("x")
	g.Set(1)
	h.Observe(1)
	r.AddCollector(func() []Sample { return nil })
	if r.Counter("x", "") != nil || r.Gauge("x", "") != nil || r.Histogram("x", "", nil) != nil {
		t.Fatal("a nil registry should give nil metrics")
	}

	reg := NewRegistry()
	cc := reg.Counter("asp_m_total", "m", "a", "b")
	cc.Inc("only-one")
	cc.Inc("one", "two", "three")
	if strings.Contains(render(t, reg), "asp_m_total{") {
		t.Fatal("an observation with the wrong number of labels was recorded")
	}
}

func TestSeriesAreBounded(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("asp_wide_total", "w", "id")
	for i := 0; i < MaxSeries+50; i++ {
		c.Inc(strconv.Itoa(i))
	}
	got := render(t, r)
	if n := strings.Count(got, "asp_wide_total{"); n != MaxSeries {
		t.Fatalf("%d series, want the bound %d", n, MaxSeries)
	}
	if !strings.Contains(got, "asp_metrics_dropped_series_total 50\n") {
		t.Fatalf("drops not counted:\n%s", got[len(got)-300:])
	}
	// A series that exists keeps counting.
	c.Inc("0")
	if !strings.Contains(render(t, r), `asp_wide_total{id="0"} 2`) {
		t.Fatal("an existing series stopped counting at the bound")
	}
}

func TestCollectorsAreReadAtScrape(t *testing.T) {
	r := NewRegistry()
	n := 1.0
	r.AddCollector(func() []Sample {
		return []Sample{
			{Name: "asp_live", Help: "Live.", Type: "gauge", Labels: []Label{{"node", "b"}}, Value: n},
			{Name: "asp_live", Help: "Live.", Type: "gauge", Labels: []Label{{"node", "a"}}, Value: n + 1},
		}
	})
	first := render(t, r)
	n = 10
	second := render(t, r)
	if !strings.Contains(first, `asp_live{node="a"} 2`) || !strings.Contains(second, `asp_live{node="a"} 11`) {
		t.Fatalf("collector not re-read:\n%s\n%s", first, second)
	}
	// One HELP and TYPE for the family, lines sorted by label.
	if strings.Count(second, "# TYPE asp_live gauge") != 1 || strings.Index(second, `node="a"`) > strings.Index(second, `node="b"`) {
		t.Fatalf("family layout:\n%s", second)
	}
}

func TestRegisteringANameTwicePanics(t *testing.T) {
	r := NewRegistry()
	r.Counter("asp_dup_total", "d")
	defer func() {
		if recover() == nil {
			t.Fatal("a second registration of the same name was accepted")
		}
	}()
	r.Gauge("asp_dup_total", "d")
}

func TestHandler(t *testing.T) {
	r := NewRegistry()
	r.Counter("asp_h_total", "h").Inc()
	h := r.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || !strings.Contains(rr.Body.String(), "asp_h_total 1") {
		t.Fatalf("GET: %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rr.Code)
	}
}

func TestRuntimeCollector(t *testing.T) {
	r := NewRegistry()
	r.AddCollector(RuntimeCollector(time.Unix(1700000000, 0)))
	got := render(t, r)
	for _, want := range []string{"go_goroutines ", "go_memstats_alloc_bytes ", "go_gc_cycles_total ", "process_start_time_seconds 1.7e+09\n", "process_cpu_seconds_total "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9100": true, "localhost:9100": true, "[::1]:9100": true, "127.1.2.3:9": true,
		":9100": false, "0.0.0.0:9100": false, "10.0.0.5:9100": false, "[::]:9100": false,
		"example.org:9100": false, "garbage": false, "": false,
	} {
		if got := Loopback(addr); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestServeRefusesANonLoopbackAddress(t *testing.T) {
	if err := Serve(t.Context(), "metrics", ":0", false, http.NotFoundHandler()); err == nil {
		t.Fatal("an unauthenticated endpoint was bound on every interface")
	}
}

func TestServeServesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRegistry()
	r.Counter("asp_served_total", "s").Inc()
	// Find a free loopback port, then ask Serve to bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if err := Serve(ctx, "metrics", addr, false, r.Mux()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "asp_served_total 1") {
		t.Fatalf("scrape: %d %s", resp.StatusCode, body)
	}
	resp, err = http.Get("http://" + addr + "/other")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/other: %d", resp.StatusCode)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/metrics"); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the listener outlived its context")
}

func TestPprofHandler(t *testing.T) {
	rr := httptest.NewRecorder()
	PprofHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "goroutine") {
		t.Fatalf("pprof index: %d", rr.Code)
	}
}
