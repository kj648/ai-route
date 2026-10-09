package gateway

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Per-key limits: RPM, TPM and monthly budget are enforced before routing,
// rejected requests are logged and never reach an upstream.

func (h *harness) limitedKey(k store.APIKey) string {
	h.t.Helper()
	k.Name, k.Enabled = "limited", true
	mustNil(h.t, h.st.CreateKey(&k))
	return k.Key
}

func (h *harness) postAs(key, path string, body any) (int, string, string) {
	h.t.Helper()
	resp, out := h.postWith(path, body, map[string]string{"Authorization": "Bearer " + key})
	return resp.StatusCode, resp.Header.Get("Retry-After"), out
}

func TestKeyRPMLimit(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	key := h.limitedKey(store.APIKey{RPM: 2})
	for i := 0; i < 2; i++ {
		if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	code, retry, body := h.postAs(key, "/v1/messages", anReq("coder", false))
	if code != 429 || !strings.Contains(body, "rate_limit_error") || !strings.Contains(body, "2 requests per minute") {
		t.Fatalf("third request: %d %s", code, body)
	}
	if n, err := strconv.Atoi(retry); err != nil || n < 1 || n > 61 {
		t.Fatalf("Retry-After %q", retry)
	}
	if hits := h.mock.Hits("ok"); hits != 2 {
		t.Fatalf("rejected request reached the upstream: %d hits", hits)
	}
	l := h.logs()[0]
	// rejected before the body is read: no model recorded, nothing sent upstream
	if l.HTTPStatus != 429 || l.KeyName != "limited" || l.RequestedModel != "" || l.Inbound != "anthropic" || l.Provider != "" {
		t.Fatalf("rejection log: %+v", l)
	}
	// other keys are not affected
	if resp, body := h.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 200 {
		t.Fatalf("unlimited key: %s", body)
	}
}

func TestKeyTPMLimit(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok") // 10 + 5 tokens per request
	key := h.limitedKey(store.APIKey{TPM: 20})
	for i := 0; i < 2; i++ { // 15 tokens used after the first, still under 20
		if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", true)); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 429 || !strings.Contains(body, "20 tokens per minute") {
		t.Fatalf("over TPM: %d %s", code, body)
	}
}

func TestKeyMonthlyBudget(t *testing.T) {
	h := newHarness(t)
	oa := *h.st.Snapshot().Providers["oa"]
	oa.Prices = map[string]store.Price{"*": {Input: 100_000, Output: 0}} // 10 input tokens = ¥1 per request
	mustNil(t, h.st.UpdateProvider(&oa))
	h.model("coder", "oa/ok")
	key := h.limitedKey(store.APIKey{MonthlyBudget: 1.5})
	for i := 0; i < 2; i++ {
		if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	code, retry, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false))
	if code != 402 || retry != "" || !strings.Contains(body, "insufficient_quota") || !strings.Contains(body, "2.00 / 1.50 CNY") {
		t.Fatalf("over budget: %d %q %s", code, retry, body)
	}
	if code, _, body := h.postAs(key, "/v1/messages", anReq("coder", false)); code != 402 || !strings.Contains(body, "billing_error") {
		t.Fatalf("anthropic error type: %d %s", code, body)
	}

	// a fresh limiter (e.g. after a restart) reloads the spend from the logs
	l := NewLimiter(h.st)
	k := h.st.Snapshot().Keys[key]
	if rej := l.Admit(k); rej == nil || rej.status != 402 {
		t.Fatalf("restarted limiter admitted an over-budget key: %+v", rej)
	}
	// budgets are in the display currency: in USD the same spend is far below 1.5
	settings := h.st.GetSettings()
	settings.Currency, settings.USDToCNY = store.CurrencyUSD, 7
	mustNil(t, h.st.UpdateSettings(settings))
	l.Forget()
	if rej := l.Admit(k); rej != nil {
		t.Fatalf("2 CNY is under 1.5 USD: %+v", rej)
	}
}

func TestLimiterWindowSlides(t *testing.T) {
	h := newHarness(t)
	l := NewLimiter(h.st)
	// whole seconds: shared (PostgreSQL) windows count per second
	t0 := time.Now().Truncate(time.Second)
	l.now = func() time.Time { return t0 }
	k := &store.APIKey{ID: 99, RPM: 1, TPM: 100}
	if rej := l.Admit(k); rej != nil {
		t.Fatal(rej)
	}
	l.Record(k, &store.RequestLog{InputTokens: 80, OutputTokens: 30})

	l.now = func() time.Time { return t0.Add(30 * time.Second) }
	rej := l.Admit(k)
	if rej == nil || rej.status != 429 || rej.retryAfter != 30*time.Second {
		t.Fatalf("within the window: %+v", rej)
	}
	k.RPM = 0 // only TPM left: 110 tokens used >= 100
	if rej := l.Admit(k); rej == nil || !strings.Contains(rej.msg, "token") || rej.retryAfter != 30*time.Second {
		t.Fatalf("TPM within the window: %+v", rej)
	}

	k.RPM = 1
	l.now = func() time.Time { return t0.Add(61 * time.Second) }
	if rej := l.Admit(k); rej != nil {
		t.Fatalf("after the window: %+v", rej)
	}
}

func TestBudgetNeverDoubleCountsAcrossReload(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.Prices = map[string]store.Price{"*": {Input: 100_000}} }) // ¥1 per request
	h.model("coder", "oa/ok")
	key := h.limitedKey(store.APIKey{MonthlyBudget: 100})
	k := h.st.Snapshot().Keys[key]
	h.gw.Limiter.Admit(k) // loads 0
	for i := 0; i < 5; i++ {
		h.postAs(key, "/v1/chat/completions", oaReq("coder", false))
	}
	h.gw.Limiter.Forget() // force a reload: the logged total replaces the cached sum
	h.gw.Limiter.Admit(k)
	for i := 0; i < 3; i++ {
		h.postAs(key, "/v1/chat/completions", oaReq("coder", false))
	}
	h.gw.Limiter.mu.Lock()
	spend := h.gw.Limiter.keys[k.ID].spend
	h.gw.Limiter.mu.Unlock()
	if spend != 8 {
		t.Fatalf("spend %v, want 8", spend)
	}
}
