package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-route/internal/alert"
	"ai-route/internal/store"
)

// healthFailures is how many consecutive failed checks take a provider out
// of rotation.
const healthFailures = 2

type healthState struct {
	lastCheck time.Time
	failures  int
	alerted   bool // a "down" alert was sent; send "recovered" when it passes
}

// HealthChecker actively probes providers that have HealthCheckSeconds set.
type HealthChecker struct {
	g   *Gateway
	now func() time.Time

	mu     sync.Mutex
	states map[string]*healthState
}

func newHealthChecker(g *Gateway) *HealthChecker {
	return &HealthChecker{g: g, now: time.Now, states: map[string]*healthState{}}
}

// Run checks due providers until ctx is done.
func (h *HealthChecker) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.CheckDue(ctx)
		}
	}
}

// CheckDue probes every enabled provider whose interval has elapsed and
// waits for the probes to finish.
func (h *HealthChecker) CheckDue(ctx context.Context) {
	now := h.now()
	var wg sync.WaitGroup
	for _, p := range h.g.store.Snapshot().Providers {
		if !p.Enabled || p.HealthCheckSeconds <= 0 {
			continue
		}
		h.mu.Lock()
		st, ok := h.states[p.Prefix]
		if !ok {
			st = &healthState{}
			h.states[p.Prefix] = st
		}
		due := now.Sub(st.lastCheck) >= time.Duration(p.HealthCheckSeconds)*time.Second
		if due {
			st.lastCheck = now
		}
		h.mu.Unlock()
		if !due {
			continue
		}
		wg.Add(1)
		go func(p *store.Provider) {
			defer wg.Done()
			h.record(p, h.probe(ctx, p))
		}(p)
	}
	wg.Wait()
}

// healthURL is the probe target: the configured URL or the models endpoint.
func healthURL(p *store.Provider) string {
	switch {
	case p.HealthCheckURL != "":
		return p.HealthCheckURL
	case p.OpenAIBaseURL != "":
		return strings.TrimSuffix(strings.TrimSuffix(p.OpenAIBaseURL, "/chat/completions"), "/") + "/models"
	default:
		return strings.TrimSuffix(anthropicURL(p.AnthropicBaseURL), "/messages") + "/models"
	}
}

// probe returns "" when healthy, otherwise what went wrong.
func (h *HealthChecker) probe(ctx context.Context, p *store.Provider) string {
	timeout := 10 * time.Second
	if d := time.Duration(p.HealthCheckSeconds) * time.Second; d < timeout {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := healthURL(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err.Error()
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		req.Header.Set("x-api-key", p.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "ai-route-healthcheck")
	for k, v := range p.Headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.g.client.Do(req)
	if err != nil {
		return fmt.Sprintf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 != 2 {
		detail := strings.TrimSpace(string(b))
		if strings.HasPrefix(detail, "<") { // an HTML error page says nothing useful
			detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Sprintf("GET %s: HTTP %d %s", target, resp.StatusCode, truncate(detail, 200))
	}
	return ""
}

func (h *HealthChecker) record(p *store.Provider, problem string) {
	h.mu.Lock()
	st := h.states[p.Prefix]
	if problem == "" {
		st.failures = 0
	} else {
		st.failures++
	}
	failures, alerted := st.failures, st.alerted
	h.mu.Unlock()

	switch {
	case problem == "":
		if h.g.Breaker.SetDown(p.Prefix, false, "") {
			log.Printf("health check: %s is back", p.Prefix)
		}
		if alerted {
			h.setAlerted(p.Prefix, false)
			h.g.Alerts.Notify(alert.Alert{Event: alert.EventHealthRecovered, Subject: p.Prefix,
				Title: fmt.Sprintf("供应商 %s 已恢复", p.Prefix), Text: "健康检查通过，已重新加入调度。"})
		}
	case failures >= healthFailures:
		if h.g.Breaker.SetDown(p.Prefix, true, "health check: "+problem) {
			log.Printf("health check: %s is down: %s", p.Prefix, problem)
		}
		if !alerted && h.g.Alerts.Enabled(alert.EventHealthDown) {
			h.setAlerted(p.Prefix, true)
			h.g.Alerts.Notify(alert.Alert{Event: alert.EventHealthDown, Subject: p.Prefix,
				Title: fmt.Sprintf("供应商 %s 健康检查失败", p.Prefix),
				Text:  fmt.Sprintf("连续 %d 次检查失败，已移出调度（只在其他候补都不可用时兜底）：%s", failures, problem)})
		}
	}
}

func (h *HealthChecker) setAlerted(prefix string, v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states[prefix].alerted = v
}

// HealthStatus is the last check result per provider, for the console.
type HealthStatus struct {
	LastCheck int64 `json:"last_check"` // unix ms
	Failures  int   `json:"failures"`
}

func (h *HealthChecker) Status() map[string]HealthStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]HealthStatus, len(h.states))
	for k, st := range h.states {
		out[k] = HealthStatus{LastCheck: st.lastCheck.UnixMilli(), Failures: st.failures}
	}
	return out
}
