// Package metrics is a small Prometheus-compatible registry: counters,
// histograms and callback gauges with labels, exposed in the text format
// at /metrics. It is hand-rolled to keep the gateway free of dependencies.
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

// Counter is a monotonically increasing float per label set.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

// Histogram counts observations per label set into cumulative buckets.
type Histogram struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// Sample is one gauge value with its label values.
type Sample struct {
	Labels []string
	Value  float64
}

// GaugeFunc produces gauge samples at scrape time.
type GaugeFunc struct {
	name, help string
	labels     []string
	fn         func() []Sample
}

type registry struct {
	mu         sync.Mutex
	counters   []*Counter
	histograms []*Histogram
	gauges     []*GaugeFunc
}

var reg registry

// NewCounter registers a counter with the given label names.
func NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labels, values: map[string]float64{}}
	reg.mu.Lock()
	reg.counters = append(reg.counters, c)
	reg.mu.Unlock()
	return c
}

// NewHistogram registers a histogram with the given upper bounds (sorted).
func NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	h := &Histogram{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histSeries{}}
	reg.mu.Lock()
	reg.histograms = append(reg.histograms, h)
	reg.mu.Unlock()
	return h
}

// NewGaugeFunc registers a gauge whose samples are computed at scrape time.
// Registering a name again replaces the callback (a gateway created later,
// e.g. in tests, takes over the gauge).
func NewGaugeFunc(name, help string, labels []string, fn func() []Sample) *GaugeFunc {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, g := range reg.gauges {
		if g.name == name {
			g.help, g.labels, g.fn = help, labels, fn
			return g
		}
	}
	g := &GaugeFunc{name: name, help: help, labels: labels, fn: fn}
	reg.gauges = append(reg.gauges, g)
	return g
}

// Add increases the counter for the label values by v.
func (c *Counter) Add(v float64, labelValues ...string) {
	k := key(labelValues)
	c.mu.Lock()
	c.values[k] += v
	c.mu.Unlock()
}

// Inc increases the counter for the label values by one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Observe records one value.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	k := key(labelValues)
	h.mu.Lock()
	s, ok := h.series[k]
	if !ok {
		s = &histSeries{counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
	h.mu.Unlock()
}

// key joins label values with a separator that cannot appear in them
// after escaping.
func key(values []string) string { return strings.Join(values, "\x00") }

func splitKey(k string) []string {
	if k == "" {
		return nil
	}
	return strings.Split(k, "\x00")
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		Write(w)
	})
}

// Write renders every registered metric.
func Write(w io.Writer) {
	reg.mu.Lock()
	counters, hists, gauges := reg.counters, reg.histograms, reg.gauges
	reg.mu.Unlock()
	var b strings.Builder
	for _, c := range counters {
		c.mu.Lock()
		keys := sortedKeys(c.values)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s%s %s\n", c.name, labelSet(c.labels, splitKey(k)), fmtFloat(c.values[k]))
		}
		c.mu.Unlock()
	}
	for _, h := range hists {
		h.mu.Lock()
		keys := make([]string, 0, len(h.series))
		for k := range h.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
		for _, k := range keys {
			s := h.series[k]
			vals := splitKey(k)
			var cum uint64
			for i, ub := range h.buckets {
				cum += s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", h.name, labelSet(append(append([]string{}, h.labels...), "le"), append(append([]string{}, vals...), fmtFloat(ub))), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", h.name, labelSet(append(append([]string{}, h.labels...), "le"), append(append([]string{}, vals...), "+Inf")), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", h.name, labelSet(h.labels, vals), fmtFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", h.name, labelSet(h.labels, vals), s.count)
		}
		h.mu.Unlock()
	}
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, s := range g.fn() {
			fmt.Fprintf(&b, "%s%s %s\n", g.name, labelSet(g.labels, s.Labels), fmtFloat(s.Value))
		}
	}
	_, _ = io.WriteString(w, b.String())
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func labelSet(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escape(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func fmtFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case f == math.Trunc(f) && math.Abs(f) < 1e15:
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
