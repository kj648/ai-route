package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Self-hosted upstreams: concurrency caps with overflow and queueing, a
// separate first-token timeout and active health checks.

func (h *harness) setProvider(prefix string, mutate func(*store.Provider)) {
	h.t.Helper()
	p := *h.st.Snapshot().Providers[prefix]
	mutate(&p)
	mustNil(h.t, h.st.UpdateProvider(&p))
}

func (h *harness) parallel(n int, path string, body func() map[string]any) []int {
	h.t.Helper()
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := h.post(path, body())
			codes[i] = resp.StatusCode
		}(i)
		time.Sleep(20 * time.Millisecond) // deterministic order: the first one takes the slot
	}
	wg.Wait()
	return codes
}

func TestConcurrencyCapOverflows(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.MaxConcurrency = 1 })
	h.model("coder", "oa/slow", "an/ok")
	codes := h.parallel(2, "/v1/chat/completions", func() map[string]any { return oaReq("coder", false) })
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes: %v", codes)
	}
	var overflow *store.RequestLog
	for _, l := range h.logs() {
		if l.Provider == "an" {
			overflow = l
		}
	}
	if overflow == nil || !overflow.Fallback || len(overflow.Attempts) != 2 || !strings.Contains(overflow.Attempts[0].Error, "at capacity") {
		t.Fatalf("overflow log: %+v", overflow)
	}
	for _, s := range h.gw.Breaker.Status() { // a full target is not a failure
		if s.TotalFailure > 0 {
			t.Fatalf("capacity skip counted as failure: %+v", s)
		}
	}
	if n := h.gw.InFlight()["oa"]; n != 0 {
		t.Fatalf("slot leaked: %d", n)
	}
}

func TestConcurrencyCapQueues(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.MaxConcurrency = 1 })
	h.model("coder", "oa/slow")
	codes := h.parallel(3, "/v1/chat/completions", func() map[string]any { return oaReq("coder", true) })
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d (should have waited for the slot)", i, c)
		}
	}
	if hits := h.mock.Hits("slow"); hits != 3 {
		t.Fatalf("hits: %d", hits)
	}

	// without queueing the caller gets 429 right away
	st := h.st.GetSettings()
	st.QueueTimeoutSeconds = 0
	mustNil(t, h.st.UpdateSettings(st))
	codes = h.parallel(2, "/v1/chat/completions", func() map[string]any { return oaReq("coder", false) })
	if codes[0] != 200 || codes[1] != 429 {
		t.Fatalf("no queue: %v", codes)
	}
	if n := h.gw.InFlight()["oa"]; n != 0 {
		t.Fatalf("slot leaked: %d", n)
	}
}

func TestFirstTokenTimeout(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.FirstTokenTimeoutSeconds = 1 })
	h.model("coder", "oa/slowfirst", "an/ok")
	start := time.Now()
	resp, body := h.post("/v1/chat/completions", oaReq("coder", true))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatalf("stream should switch after the first-token timeout: %d %s", resp.StatusCode, body)
	}
	if d := time.Since(start); d > 1400*time.Millisecond {
		t.Fatalf("waited %s", d)
	}
	l := h.logs()[0]
	if !strings.Contains(l.Attempts[0].Error, "first token timeout after 1s") || l.Attempts[0].HTTPStatus != 504 || len(l.Attempts) != 2 {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
	// non-stream requests keep the normal timeout
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/slowfirst" {
		t.Fatalf("non-stream switched: %s", resp.Header.Get("X-Route-Target"))
	}
}

func TestHealthCheckTakesProviderOutOfRotation(t *testing.T) {
	h := newHarness(t)
	sink := h.alertSink(nil)
	var healthy atomic.Bool
	var probes atomic.Int32
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if r.Header.Get("Authorization") != "Bearer k-oa" {
			http.Error(w, "no auth", 401)
			return
		}
		if !healthy.Load() {
			http.Error(w, "loading model", 503)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(hc.Close)
	h.setProvider("oa", func(p *store.Provider) { p.HealthCheckSeconds = 10; p.HealthCheckURL = hc.URL + "/v1/models" })
	h.model("coder", "oa/ok", "an/ok")

	now := time.Now()
	h.gw.Health.now = func() time.Time { return now }
	check := func() {
		h.gw.Health.CheckDue(context.Background())
		now = now.Add(10 * time.Second)
	}
	check()
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/ok" {
		t.Fatal("one failed check must not take the provider out yet")
	}
	now = now.Add(-5 * time.Second) // only 5s since the last check: not due
	h.gw.Health.CheckDue(context.Background())
	now = now.Add(5 * time.Second)
	if probes.Load() != 1 {
		t.Fatalf("probes: %d", probes.Load())
	}
	check()
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatal("down provider should be skipped")
	}
	alerts := sink.get(h)
	if len(alerts) != 1 || alerts[0]["event"] != "health_down" || !strings.Contains(alerts[0]["text"].(string), "HTTP 503 loading model") {
		t.Fatalf("down alert: %v", alerts)
	}
	check() // still down: no repeated alert
	healthy.Store(true)
	check()
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/ok" {
		t.Fatal("recovered provider should be back in rotation")
	}
	if alerts := sink.get(h); len(alerts) != 2 || alerts[1]["event"] != "health_recovered" {
		t.Fatalf("recovery alert: %v", alerts)
	}
}

func TestHealthURLDefaults(t *testing.T) {
	cases := map[string]*store.Provider{
		"http://gpu:8000/v1/models":         {OpenAIBaseURL: "http://gpu:8000/v1"},
		"http://x/custom":                   {OpenAIBaseURL: "http://gpu:8000/v1", HealthCheckURL: "http://x/custom"},
		"https://api.example.com/v1/models": {AnthropicBaseURL: "https://api.example.com"},
	}
	for want, p := range cases {
		if got := healthURL(p); got != want {
			t.Errorf("healthURL(%+v) = %s, want %s", p, got, want)
		}
	}
}

// Review regressions.

func TestHealthDownClearedWhenChecksTurnedOff(t *testing.T) {
	h := newHarness(t)
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	t.Cleanup(hc.Close)
	h.setProvider("oa", func(p *store.Provider) { p.HealthCheckSeconds = 10; p.HealthCheckURL = hc.URL })
	h.model("coder", "oa/ok", "an/ok")
	now := time.Now()
	h.gw.Health.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		h.gw.Health.CheckDue(context.Background())
		now = now.Add(10 * time.Second)
	}
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatal("setup: oa should be down")
	}
	h.setProvider("oa", func(p *store.Provider) { p.HealthCheckSeconds = 0 })
	h.gw.Health.CheckDue(context.Background())
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/ok" {
		t.Fatal("turning health checks off must bring the provider back")
	}
}

func TestHealthFlapReportsLatestState(t *testing.T) {
	h := newHarness(t)
	sink := h.alertSink(nil)
	var healthy atomic.Bool
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", 503)
		}
	}))
	t.Cleanup(hc.Close)
	h.setProvider("oa", func(p *store.Provider) { p.HealthCheckSeconds = 10; p.HealthCheckURL = hc.URL })
	now := time.Now()
	h.gw.Health.now = func() time.Time { return now }
	check := func() {
		h.gw.Health.CheckDue(context.Background())
		h.gw.Alerts.Wait() // webhooks are sent in the background: keep their order
		now = now.Add(10 * time.Second)
	}
	check()
	check() // down
	healthy.Store(true)
	check() // recovered
	healthy.Store(false)
	check()
	check() // down again, within the silence window
	var events []string
	for _, a := range sink.get(h) {
		events = append(events, a["event"].(string))
	}
	if strings.Join(events, ",") != "health_down,health_recovered,health_down" {
		t.Fatalf("events: %v", events)
	}
}

func TestQueueTimeoutAfterOtherFailuresIs429(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.MaxConcurrency = 1 })
	st := h.st.GetSettings()
	st.QueueTimeoutSeconds = 1
	mustNil(t, h.st.UpdateSettings(st))
	h.model("coder", "oa/slowfirst", "an/fail400")
	var codes [2]int
	var retry string
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := h.post("/v1/chat/completions", oaReq("coder", true))
			codes[i] = resp.StatusCode
			if i == 1 {
				retry = resp.Header.Get("Retry-After")
			}
		}(i)
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 429 || retry != "1" {
		t.Fatalf("codes %v, Retry-After %q", codes, retry)
	}
}
