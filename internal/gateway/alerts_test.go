package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ai-route/internal/store"
)

// Alerts: upstream 401/402, an exhausted target chain and long cooldowns are
// pushed to the configured webhooks.

type alertSink struct {
	mu     sync.Mutex
	alerts []map[string]any
}

func (h *harness) alertSink(mutate func(*store.AlertConfig)) *alertSink {
	h.t.Helper()
	s := &alertSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		s.mu.Lock()
		s.alerts = append(s.alerts, m)
		s.mu.Unlock()
	}))
	h.t.Cleanup(srv.Close)
	c := store.DefaultAlertConfig()
	c.Webhooks = []store.Webhook{{Type: store.WebhookGeneric, URL: srv.URL, Enabled: true}}
	if mutate != nil {
		mutate(&c)
	}
	mustNil(h.t, h.st.UpdateAlerts(c))
	return s
}

func (s *alertSink) get(h *harness) []map[string]any {
	h.gw.Alerts.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.alerts...)
}

func TestAlertOnAuthFailure(t *testing.T) {
	h := newHarness(t)
	sink := h.alertSink(nil)
	h.model("coder", "oa/fail402", "an/ok")
	for i := 0; i < 2; i++ { // second request: oa is cooling, tried last only if needed
		if resp, body := h.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 200 {
			t.Fatalf("fallback should still serve: %s", body)
		}
	}
	got := sink.get(h)
	if len(got) != 1 {
		t.Fatalf("want 1 alert, got %v", got)
	}
	a := got[0]
	if a["event"] != "auth_failure" || a["subject"] != "oa" || !strings.Contains(a["title"].(string), "欠费") ||
		!strings.Contains(a["text"].(string), "HTTP 402") || !strings.Contains(a["text"].(string), "insufficient balance") {
		t.Fatalf("alert: %v", a)
	}
}

func TestAlertOnAllTargetsFailed(t *testing.T) {
	h := newHarness(t)
	sink := h.alertSink(func(c *store.AlertConfig) { c.OnAuthFailure = false })
	h.model("coder", "oa/fail401", "an/fail500")
	if resp, _ := h.post("/v1/messages", anReq("coder", false)); resp.StatusCode < 400 {
		t.Fatal("expected an error")
	}
	got := sink.get(h)
	if len(got) != 1 || got[0]["event"] != "all_failed" || got[0]["subject"] != "coder" {
		t.Fatalf("alerts: %v", got)
	}
	text := got[0]["text"].(string)
	if !strings.Contains(text, "oa/fail401 HTTP 401") || !strings.Contains(text, "an/fail500 HTTP 500") {
		t.Fatalf("attempts missing: %s", text)
	}

	h.model("empty", "off/ok") // only a disabled provider
	h.post("/v1/messages", anReq("empty", false))
	if got := sink.get(h); len(got) != 2 || got[1]["subject"] != "empty" {
		t.Fatalf("no-target alert: %v", got)
	}
}

func TestAlertOnLongCooldown(t *testing.T) {
	h := newHarness(t)
	sink := h.alertSink(func(c *store.AlertConfig) { c.LongCooldownMinutes = 2; c.OnAllFailed = false })
	h.model("short", "oa/fail500") // soft failures: 60s cooldown, below the threshold
	h.model("long", "an/fail429")  // Retry-After 120s cools the whole plan for 2 minutes
	for i := 0; i < 3; i++ {
		h.post("/v1/chat/completions", oaReq("short", false))
	}
	h.post("/v1/chat/completions", oaReq("long", false))
	got := sink.get(h)
	if len(got) != 1 || got[0]["event"] != "long_cooldown" || got[0]["subject"] != "p:an" ||
		!strings.Contains(got[0]["title"].(string), "整个套餐 an 冷却 2 分钟") {
		t.Fatalf("alerts: %v", got)
	}
}
