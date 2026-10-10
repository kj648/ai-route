package gateway

import (
	"testing"
	"time"

	"ai-route/internal/store"
)

func setQuotas(t *testing.T, h *harness, prefix string, qs ...store.Quota) {
	t.Helper()
	p := *h.st.Snapshot().Providers[prefix]
	p.APIKey, p.Quotas = "", qs
	mustNil(t, h.st.UpdateProvider(&p))
}

func TestQuotaMovesProviderBehindOthers(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/plan-model", "both/paygo-model")
	setQuotas(t, h, "oa", store.Quota{Period: "5h", Requests: 2})
	h.gw.Quotas.Refresh()
	for i := 0; i < 2; i++ {
		if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if h.mock.Hits("plan-model") != 2 || h.mock.Hits("paygo-model") != 0 {
		t.Fatalf("hits plan=%d paygo=%d", h.mock.Hits("plan-model"), h.mock.Hits("paygo-model"))
	}
	// counted in memory right away, and again from the logs on a recount
	if !h.gw.Quotas.Over("oa") {
		t.Fatal("quota not used up after 2 requests")
	}
	h.gw.Quotas.Refresh()
	st := h.gw.Quotas.Status()["oa"]
	if len(st) != 1 || st[0].UsedRequests != 2 || !st[0].Over {
		t.Fatalf("status %+v", st)
	}
	h.post("/v1/chat/completions", oaReq("coder", false))
	if h.mock.Hits("plan-model") != 2 || h.mock.Hits("paygo-model") != 1 {
		t.Fatalf("over-quota provider still first: plan=%d paygo=%d", h.mock.Hits("plan-model"), h.mock.Hits("paygo-model"))
	}
	// still a last resort when the others fail
	h.model("only", "oa/plan-model")
	if resp, _ := h.post("/v1/chat/completions", oaReq("only", false)); resp.StatusCode != 200 {
		t.Fatalf("over-quota provider not used as last resort: %d", resp.StatusCode)
	}
	l := h.logs()[0]
	if len(l.Attempts) != 1 || !l.Attempts[0].OverQuota {
		t.Fatalf("attempt not marked over quota: %+v", l.Attempts)
	}
	// raising the quota puts it back in front after a recount
	setQuotas(t, h, "oa", store.Quota{Period: "5h", Requests: 100})
	h.gw.Quotas.Refresh()
	h.post("/v1/chat/completions", oaReq("coder", false))
	if h.mock.Hits("plan-model") != 4 {
		t.Fatalf("plan not back in front: plan=%d", h.mock.Hits("plan-model"))
	}
}

func TestQuotaTokens(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/plan-model", "both/paygo-model")
	setQuotas(t, h, "oa", store.Quota{Period: "day", Tokens: 10})
	h.gw.Quotas.Refresh()
	h.post("/v1/chat/completions", oaReq("coder", false)) // mock usage: more than 10 tokens
	h.gw.Quotas.Refresh()
	if st := h.gw.Quotas.Status()["oa"]; len(st) != 1 || st[0].UsedTokens < 10 || !st[0].Over {
		t.Fatalf("status %+v", st)
	}
}

func TestQuotaValidationAndWindows(t *testing.T) {
	h := newHarness(t)
	p := *h.st.Snapshot().Providers["oa"]
	p.APIKey = ""
	for _, bad := range [][]store.Quota{
		{{Period: "hour", Requests: 1}},
		{{Period: "5h", Requests: -1}},
		{{Period: "5h", Requests: 1}, {Period: "5h", Tokens: 1}},
	} {
		p.Quotas = bad
		if err := h.st.UpdateProvider(&p); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// zero limits are dropped
	p.Quotas = []store.Quota{{Period: "week"}}
	mustNil(t, h.st.UpdateProvider(&p))
	if n := len(h.st.Snapshot().Providers["oa"].Quotas); n != 0 {
		t.Fatalf("empty quota kept: %d", n)
	}

	now := time.Date(2026, 10, 8, 15, 30, 0, 0, time.Local) // a Thursday
	for period, want := range map[string]time.Time{
		"5h":    now.Add(-5 * time.Hour),
		"day":   time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local),
		"week":  time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local),
		"month": time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		if got := store.QuotaStart(period, now); got != want.UnixMilli() {
			t.Errorf("%s: %v, want %v", period, time.UnixMilli(got), want)
		}
	}
}

func TestProviderUsagePartialHour(t *testing.T) {
	h := newHarness(t)
	base := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	for _, at := range []time.Time{base.Add(10 * time.Minute), base.Add(40 * time.Minute), base.Add(70 * time.Minute)} {
		h.st.AddLog(&store.RequestLog{CreatedAt: at.UnixMilli(), Provider: "oa", Success: true, InputTokens: 3, OutputTokens: 2})
	}
	h.st.FlushLogs()
	// from minute 30: the raw logs cover the partial hour, the rollup the rest
	n, tk, err := h.st.ProviderUsage("oa", base.Add(30*time.Minute).UnixMilli())
	mustNil(t, err)
	if n != 2 || tk != 10 {
		t.Fatalf("usage %d requests %d tokens, want 2 / 10", n, tk)
	}
}
