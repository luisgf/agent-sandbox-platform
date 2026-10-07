// Package metrics is a small Prometheus-format metrics registry: counters, gauges
// and histograms with labels, collectors that read their values at scrape time, and
// the text exposition format 0.0.4 over HTTP. It has no dependency on purpose: the
// platform's modules depend on little beyond the standard library, and a client
// library would be the largest part of the control plane's dependency tree.
//
// control-plane/internal/metrics and node-agent/internal/metrics are the same file;
// a test in node-agent fails when they differ.
//
// Every method of a metric is a no-op on a nil receiver, so a component that was
// given no registry (a test, a dry run) needs no checks.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// MaxSeries bounds the label combinations of one metric. A label that follows
// something a caller can choose would otherwise grow the process without limit;
// observations for a series past the bound are dropped and counted
// (asp_metrics_dropped_series_total).
const MaxSeries = 1000

// Label is one label of a Sample.
type Label struct{ Name, Value string }

// Sample is one value a Collector reports at scrape time.
type Sample struct {
	Name   string
	Help   string
	Type   string // "gauge" or "counter"
	Labels []Label
	Value  float64
}

// Collector reports samples when the registry is scraped. It must be quick and
// must not hold locks the scraped components need for long.
type Collector func() []Sample

// Registry holds metrics and collectors. The zero value is not usable: NewRegistry.
type Registry struct {
	mu         sync.Mutex
	names      map[string]bool
	metrics    []metric
	collectors []Collector
	dropped    uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

type metric interface {
	describe() (name, help, typ string)
	collect(emit func(suffix string, labels []Label, v float64))
}

func (r *Registry) register(m metric) {
	name, _, _ := m.describe()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic("metrics: " + name + " registered twice")
	}
	r.names[name] = true
	r.metrics = append(r.metrics, m)
}

// AddCollector adds a function whose samples are read at every scrape.
func (r *Registry) AddCollector(c Collector) {
	if r == nil || c == nil {
		return
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
}

func (r *Registry) noteDropped() {
	r.mu.Lock()
	r.dropped++
	r.mu.Unlock()
}

// --- the shared bookkeeping of a labelled metric

type vec struct {
	reg    *Registry
	name   string
	help   string
	typ    string
	labels []string
	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	value  float64
	// histograms
	counts []uint64
	sum    float64
	count  uint64
}

func newVec(r *Registry, name, help, typ string, labels []string) *vec {
	return &vec{reg: r, name: name, help: help, typ: typ, labels: labels, series: map[string]*series{}}
}

// get returns the series for lv, creating it; nil when lv does not match the
// label names or the metric already has MaxSeries series.
func (v *vec) get(lv []string, buckets int) *series {
	if len(lv) != len(v.labels) {
		return nil
	}
	key := strings.Join(lv, "\xff")
	s, ok := v.series[key]
	if ok {
		return s
	}
	if len(v.series) >= MaxSeries {
		v.reg.noteDropped()
		return nil
	}
	s = &series{values: append([]string(nil), lv...)}
	if buckets > 0 {
		s.counts = make([]uint64, buckets)
	}
	v.series[key] = s
	return s
}

func (v *vec) labelPairs(s *series) []Label {
	out := make([]Label, len(v.labels))
	for i, n := range v.labels {
		out[i] = Label{n, s.values[i]}
	}
	return out
}

// --- counter

// Counter is a monotonically increasing value per label combination.
type Counter struct{ v *vec }

// Counter registers a counter. Its name should end in _total.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	if r == nil {
		return nil
	}
	c := &Counter{v: newVec(r, name, help, "counter", labels)}
	r.register(c)
	return c
}

// Inc adds 1 to the series for the given label values.
func (c *Counter) Inc(lv ...string) { c.Add(1, lv...) }

// Add adds delta (ignored when negative) to the series for the given label values.
func (c *Counter) Add(delta float64, lv ...string) {
	if c == nil || delta < 0 {
		return
	}
	c.v.mu.Lock()
	defer c.v.mu.Unlock()
	if s := c.v.get(lv, 0); s != nil {
		s.value += delta
	}
}

func (c *Counter) describe() (string, string, string) { return c.v.name, c.v.help, "counter" }
func (c *Counter) collect(emit func(string, []Label, float64)) {
	c.v.mu.Lock()
	defer c.v.mu.Unlock()
	for _, s := range c.v.series {
		emit("", c.v.labelPairs(s), s.value)
	}
}

// --- gauge

// Gauge is a value that goes up and down, per label combination.
type Gauge struct{ v *vec }

// Gauge registers a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	if r == nil {
		return nil
	}
	g := &Gauge{v: newVec(r, name, help, "gauge", labels)}
	r.register(g)
	return g
}

// Set sets the series for the given label values.
func (g *Gauge) Set(value float64, lv ...string) {
	if g == nil {
		return
	}
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	if s := g.v.get(lv, 0); s != nil {
		s.value = value
	}
}

// Add adds delta (it may be negative) to the series for the given label values.
func (g *Gauge) Add(delta float64, lv ...string) {
	if g == nil {
		return
	}
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	if s := g.v.get(lv, 0); s != nil {
		s.value += delta
	}
}

func (g *Gauge) describe() (string, string, string) { return g.v.name, g.v.help, "gauge" }
func (g *Gauge) collect(emit func(string, []Label, float64)) {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	for _, s := range g.v.series {
		emit("", g.v.labelPairs(s), s.value)
	}
}

// --- histogram

// DefBuckets suit requests that take milliseconds to minutes.
var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// Histogram counts observations into cumulative buckets, per label combination.
type Histogram struct {
	v       *vec
	buckets []float64
}

// Histogram registers a histogram with the given upper bounds (DefBuckets when nil).
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if r == nil {
		return nil
	}
	if buckets == nil {
		buckets = DefBuckets
	}
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	h := &Histogram{v: newVec(r, name, help, "histogram", labels), buckets: b}
	r.register(h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(value float64, lv ...string) {
	if h == nil || math.IsNaN(value) {
		return
	}
	h.v.mu.Lock()
	defer h.v.mu.Unlock()
	s := h.v.get(lv, len(h.buckets))
	if s == nil {
		return
	}
	s.sum += value
	s.count++
	for i, ub := range h.buckets {
		if value <= ub {
			s.counts[i]++
		}
	}
}

func (h *Histogram) describe() (string, string, string) { return h.v.name, h.v.help, "histogram" }
func (h *Histogram) collect(emit func(string, []Label, float64)) {
	h.v.mu.Lock()
	defer h.v.mu.Unlock()
	for _, s := range h.v.series {
		base := h.v.labelPairs(s)
		for i, ub := range h.buckets {
			emit("_bucket", append(append([]Label(nil), base...), Label{"le", formatFloat(ub)}), float64(s.counts[i]))
		}
		emit("_bucket", append(append([]Label(nil), base...), Label{"le", "+Inf"}), float64(s.count))
		emit("_sum", base, s.sum)
		emit("_count", base, float64(s.count))
	}
}

// --- exposition

type line struct {
	suffix string
	labels []Label
	value  float64
}

type family struct {
	name, help, typ string
	lines           []line
}

func (r *Registry) gather() []*family {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	cs := append([]Collector(nil), r.collectors...)
	dropped := r.dropped
	r.mu.Unlock()

	fams := map[string]*family{}
	get := func(name, help, typ string) *family {
		f, ok := fams[name]
		if !ok {
			f = &family{name: name, help: help, typ: typ}
			fams[name] = f
		}
		return f
	}
	for _, m := range ms {
		name, help, typ := m.describe()
		f := get(name, help, typ)
		m.collect(func(suffix string, labels []Label, v float64) {
			f.lines = append(f.lines, line{suffix, labels, v})
		})
	}
	for _, c := range cs {
		for _, s := range c() {
			f := get(s.Name, s.Help, s.Type)
			f.lines = append(f.lines, line{"", s.Labels, s.Value})
		}
	}
	f := get("asp_metrics_dropped_series_total", "Observations dropped because a metric reached its series bound.", "counter")
	f.lines = append(f.lines, line{"", nil, float64(dropped)})

	out := make([]*family, 0, len(fams))
	for _, f := range fams {
		sort.SliceStable(f.lines, func(i, j int) bool { return lineKey(f.lines[i]) < lineKey(f.lines[j]) })
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// lineKey orders lines by label values, with a histogram's buckets in order.
func lineKey(l line) string {
	var b strings.Builder
	for _, lb := range l.labels {
		if lb.Name == "le" {
			continue
		}
		b.WriteString(lb.Name + "=" + lb.Value + "\xff")
	}
	for _, lb := range l.labels {
		if lb.Name == "le" {
			if lb.Value == "+Inf" {
				b.WriteString("\x01" + "zzz")
			} else {
				f, _ := strconv.ParseFloat(lb.Value, 64)
				fmt.Fprintf(&b, "\x01%020.6f", f)
			}
		}
	}
	return b.String() + "\x02" + l.suffix
}

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	var total int64
	write := func(s string) error {
		n, err := io.WriteString(w, s)
		total += int64(n)
		return err
	}
	for _, f := range r.gather() {
		if err := write("# HELP " + f.name + " " + escapeHelp(f.help) + "\n# TYPE " + f.name + " " + f.typ + "\n"); err != nil {
			return total, err
		}
		for _, l := range f.lines {
			var b strings.Builder
			b.WriteString(f.name + l.suffix)
			if len(l.labels) > 0 {
				b.WriteByte('{')
				for i, lb := range l.labels {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(lb.Name + `="` + escapeLabel(lb.Value) + `"`)
				}
				b.WriteByte('}')
			}
			b.WriteString(" " + formatFloat(l.value) + "\n")
			if err := write(b.String()); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// Handler serves the registry on a scrape.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if req.Method == http.MethodHead {
			return
		}
		_, _ = r.WriteTo(w)
	})
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}
