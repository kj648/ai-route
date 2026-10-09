package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	c := NewCounter("t_requests_total", "requests", "model", "status")
	c.Inc("coder", "200")
	c.Inc("coder", "200")
	c.Add(0.5, "fast", "5\"xx")
	h := NewHistogram("t_latency_seconds", "latency", []float64{0.1, 1}, "model")
	h.Observe(0.05, "coder")
	h.Observe(0.5, "coder")
	h.Observe(5, "coder")
	NewGaugeFunc("t_open", "open", []string{"name"}, func() []Sample { return []Sample{{Labels: []string{"kimi"}, Value: 1}} })

	var b strings.Builder
	Write(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE t_requests_total counter\n",
		`t_requests_total{model="coder",status="200"} 2` + "\n",
		`t_requests_total{model="fast",status="5\"xx"} 0.5` + "\n",
		"# TYPE t_latency_seconds histogram\n",
		`t_latency_seconds_bucket{model="coder",le="0.1"} 1` + "\n",
		`t_latency_seconds_bucket{model="coder",le="1"} 2` + "\n",
		`t_latency_seconds_bucket{model="coder",le="+Inf"} 3` + "\n",
		`t_latency_seconds_sum{model="coder"} 5.55` + "\n",
		`t_latency_seconds_count{model="coder"} 3` + "\n",
		"# TYPE t_open gauge\n",
		`t_open{name="kimi"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
