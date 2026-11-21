// Package metrics is a minimal, dependency-free implementation of the
// Prometheus text exposition format (version 0.0.4): counters with labels,
// gauges computed on scrape, and fixed-bucket histograms.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []writer
}

type writer interface{ write(b *strings.Builder) }

func (r *Registry) add(w writer) {
	r.mu.Lock()
	r.metrics = append(r.metrics, w)
	r.mu.Unlock()
}

// WriteTo writes every metric in the text format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	ms := append([]writer(nil), r.metrics...)
	r.mu.Unlock()
	var b strings.Builder
	for _, m := range ms {
		m.write(&b)
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func header(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, strings.ReplaceAll(help, "\n", " "), name, typ)
}

// escape escapes a label value: backslash, double quote and newline.
func escape(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func labelPairs(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + `="` + escape(values[i]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// CounterVec is a family of monotonically increasing counters.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.RWMutex
	series     map[string]*counterSeries
}

type counterSeries struct {
	values []string
	n      atomic.Uint64
}

// NewCounterVec registers a counter family with the given label names.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, series: map[string]*counterSeries{}}
	r.add(c)
	return c
}

// Add increments the series identified by values (one per label) by n.
func (c *CounterVec) Add(n uint64, values ...string) {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", c.name, len(c.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	c.mu.RLock()
	s := c.series[key]
	c.mu.RUnlock()
	if s == nil {
		c.mu.Lock()
		if s = c.series[key]; s == nil {
			s = &counterSeries{values: append([]string(nil), values...)}
			c.series[key] = s
		}
		c.mu.Unlock()
	}
	s.n.Add(n)
}

// Inc increments the series identified by values by one.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Value returns the current count of a series (0 if absent).
func (c *CounterVec) Value(values ...string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s := c.series[strings.Join(values, "\xff")]; s != nil {
		return s.n.Load()
	}
	return 0
}

func (c *CounterVec) write(b *strings.Builder) {
	header(b, c.name, c.help, "counter")
	c.mu.RLock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := c.series[k]
		fmt.Fprintf(b, "%s%s %d\n", c.name, labelPairs(c.labels, s.values), s.n.Load())
	}
	c.mu.RUnlock()
}

// GaugeFunc is a gauge whose value is computed at scrape time.
type GaugeFunc struct {
	name, help string
	fn         func() float64
}

// NewGaugeFunc registers a gauge reading fn on every scrape.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) *GaugeFunc {
	g := &GaugeFunc{name: name, help: help, fn: fn}
	r.add(g)
	return g
}

func (g *GaugeFunc) write(b *strings.Builder) {
	header(b, g.name, g.help, "gauge")
	fmt.Fprintf(b, "%s %s\n", g.name, formatFloat(g.fn()))
}

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	name, help string
	bounds     []float64
	counts     []atomic.Uint64 // per bucket, non-cumulative; last is +Inf
	count      atomic.Uint64
	sumBits    atomic.Uint64
}

// NewHistogram registers a histogram with the given upper bounds, which
// must be sorted ascending; +Inf is implicit.
func (r *Registry) NewHistogram(name, help string, bounds []float64) *Histogram {
	if !sort.Float64sAreSorted(bounds) {
		panic("metrics: histogram bounds must be sorted")
	}
	h := &Histogram{name: name, help: help, bounds: append([]float64(nil), bounds...),
		counts: make([]atomic.Uint64, len(bounds)+1)}
	r.add(h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	h.counts[i].Add(1)
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		if h.sumBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 { return h.count.Load() }

func (h *Histogram) write(b *strings.Builder) {
	header(b, h.name, h.help, "histogram")
	var cum uint64
	for i, bound := range h.bounds {
		cum += h.counts[i].Load()
		fmt.Fprintf(b, "%s_bucket{le=\"%s\"} %d\n", h.name, formatFloat(bound), cum)
	}
	cum += h.counts[len(h.bounds)].Load()
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", h.name, cum)
	fmt.Fprintf(b, "%s_sum %s\n", h.name, formatFloat(math.Float64frombits(h.sumBits.Load())))
	fmt.Fprintf(b, "%s_count %d\n", h.name, cum)
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
