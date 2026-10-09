package gateway

import (
	"strconv"
	"strings"

	"ai-route/internal/metrics"
	"ai-route/internal/store"
	"ai-route/internal/version"
)

// Prometheus metrics for the public API. Label values are bounded by the
// configuration (models, providers, keys), never by request content.
var (
	mRequests = metrics.NewCounter("ai_route_requests_total",
		"Requests by public model, provider that answered, inbound protocol and HTTP status.",
		"model", "provider", "inbound", "status")
	mLatency = metrics.NewHistogram("ai_route_request_duration_seconds",
		"Request duration including retries and fallbacks.",
		[]float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}, "model", "provider")
	mTTFB = metrics.NewHistogram("ai_route_ttfb_seconds",
		"Time to the first byte of the response.",
		[]float64{0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 20, 30, 60}, "model", "provider")
	mTokens = metrics.NewCounter("ai_route_tokens_total",
		"Tokens by kind: input (including cached), cached (cache reads) and output.",
		"model", "provider", "kind")
	mCost = metrics.NewCounter("ai_route_cost_total",
		"Cost in the logged currency, from unit prices or the upstream's own figure.",
		"model", "provider", "currency")
	mAttempts = metrics.NewCounter("ai_route_upstream_attempts_total",
		"Upstream attempts by target and result (ok, error, skipped).",
		"provider", "target", "result")
	mFallbacks = metrics.NewCounter("ai_route_fallbacks_total",
		"Requests answered by a target other than the first in the routing order.", "model")
	mRejected = metrics.NewCounter("ai_route_rejected_total",
		"Requests refused before reaching an upstream, by API key and status (401, 402, 403, 404, 429).",
		"key", "status")
)

func (g *Gateway) registerGauges() {
	metrics.NewGaugeFunc("ai_route_build_info", "Gateway version.", []string{"version"},
		func() []metrics.Sample { return []metrics.Sample{{Labels: []string{version.Version}, Value: 1}} })
	metrics.NewGaugeFunc("ai_route_breaker_open", "1 while a provider or target is cooling down or failing its health check.",
		[]string{"kind", "name"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range g.Breaker.Status() {
				v := 0.0
				if s.Open {
					v = 1
				}
				out = append(out, metrics.Sample{Labels: []string{s.Kind, s.Name}, Value: v})
			}
			return out
		})
	metrics.NewGaugeFunc("ai_route_provider_inflight", "In-flight requests on providers with a concurrency cap.",
		[]string{"provider"}, func() []metrics.Sample {
			var out []metrics.Sample
			for p, n := range g.InFlight() {
				out = append(out, metrics.Sample{Labels: []string{p}, Value: float64(n)})
			}
			return out
		})
	metrics.NewGaugeFunc("ai_route_log_dropped_total", "Request log entries lost because the log queue stayed full.", nil,
		func() []metrics.Sample { return []metrics.Sample{{Value: float64(g.store.DroppedLogs())}} })
}

// log records a finished request in the metrics and queues it for the
// request log.
func (g *Gateway) log(e *store.RequestLog) {
	g.observe(e)
	g.store.AddLog(e)
}

func (g *Gateway) observe(e *store.RequestLog) {
	model, provider := e.PublicModel, e.Provider
	if model == "" {
		model = "(none)"
	}
	if provider == "" {
		provider = "(none)"
	}
	status := strconv.Itoa(e.HTTPStatus)
	mRequests.Inc(model, provider, e.Inbound, status)
	if e.Provider == "" && e.HTTPStatus >= 400 && len(e.Attempts) == 0 {
		mRejected.Inc(e.KeyName, status)
	}
	if e.LatencyMs > 0 || e.Success {
		mLatency.Observe(float64(e.LatencyMs)/1000, model, provider)
	}
	if e.TTFBMs > 0 {
		mTTFB.Observe(float64(e.TTFBMs)/1000, model, provider)
	}
	if e.InputTokens > 0 {
		mTokens.Add(float64(e.InputTokens), model, provider, "input")
	}
	if e.CachedTokens > 0 {
		mTokens.Add(float64(e.CachedTokens), model, provider, "cached")
	}
	if e.OutputTokens > 0 {
		mTokens.Add(float64(e.OutputTokens), model, provider, "output")
	}
	if e.Cost > 0 && e.Currency != "" {
		mCost.Add(e.Cost, model, provider, e.Currency)
	}
	if e.Fallback {
		mFallbacks.Inc(model)
	}
	for _, a := range e.Attempts {
		prefix, _ := splitTarget(a.Target)
		result := "error"
		switch {
		case a.HTTPStatus == 200 && (a.Error == "" || !strings.HasPrefix(a.Error, "HTTP")):
			result = "ok"
		case strings.Contains(a.Error, "at capacity"):
			result = "skipped"
		}
		mAttempts.Inc(prefix, a.Target, result)
	}
}
