package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Request-log behaviour: every request through the gateway leaves exactly one
// log entry with the routing decision, usage and per-attempt details.

func TestRequestLogSuccessFields(t *testing.T) {
	h := newHarness(t)
	mustNil(t, h.st.CreateModel(&store.Model{Name: "dess", Aliases: []string{"claude-*"}, Targets: []string{"an/ok"}, Enabled: true}))
	before := time.Now().UnixMilli()
	resp, body := h.postWith("/v1/chat/completions", oaReq("claude-sonnet-x", false), map[string]string{"X-Forwarded-For": "10.1.2.3, 172.16.0.1"})
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	logs := h.logs()
	if len(logs) != 1 {
		t.Fatalf("want 1 log, got %d", len(logs))
	}
	l := logs[0]
	checks := []struct {
		name string
		ok   bool
	}{
		{"created_at", l.CreatedAt >= before && l.CreatedAt <= time.Now().UnixMilli()},
		{"key", l.KeyID > 0 && l.KeyName == "tester"},
		{"models", l.RequestedModel == "claude-sonnet-x" && l.PublicModel == "dess"},
		{"protocols", l.Inbound == "openai" && l.UpstreamProtocol == "anthropic" && !l.Stream},
		{"target", l.Provider == "an" && l.UpstreamModel == "ok"},
		{"result", l.Success && l.HTTPStatus == 200 && l.Error == "" && !l.Fallback},
		{"usage", l.InputTokens == 10 && l.OutputTokens == 5 && l.CachedTokens == 3}, // 7 input + 3 cache read
		{"attempts", len(l.Attempts) == 1 && l.Attempts[0].Target == "an/ok" && l.Attempts[0].HTTPStatus == 200 && l.Attempts[0].Error == ""},
		{"client_ip", l.ClientIP == "10.1.2.3"},
		{"latency", l.LatencyMs >= 0 && l.TTFBMs >= 0 && l.TTFBMs <= l.LatencyMs},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("field %s wrong: %+v", c.name, l)
		}
	}
}

func TestRequestLogStreamUsageAllDirections(t *testing.T) {
	cases := []struct {
		target, path string
		body         map[string]any
		in, out      int64
	}{
		{"oa/ok", "/v1/chat/completions", oaReq("m", true), 10, 5}, // openai passthrough
		{"an/ok", "/v1/messages", anReq("m", true), 7, 9},          // anthropic passthrough
		{"an/ok", "/v1/chat/completions", oaReq("m", true), 7, 9},  // anthropic -> openai
		{"oa/ok", "/v1/messages", anReq("m", true), 10, 5},         // openai -> anthropic
	}
	for _, c := range cases {
		h := newHarness(t)
		h.model("m", c.target)
		h.post(c.path, c.body)
		l := h.logs()[0]
		if !l.Success || !l.Stream || l.InputTokens != c.in || l.OutputTokens != c.out {
			t.Errorf("%s via %s: success=%v stream=%v in=%d out=%d", c.target, c.path, l.Success, l.Stream, l.InputTokens, l.OutputTokens)
		}
	}
}

func TestRequestLogFailures(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/fail500", "an/fail400")
	h.model("secret", "oa/ok")
	limited := &store.APIKey{Name: "limited", Enabled: true, AllowedModels: []string{"dess"}}
	mustNil(t, h.st.CreateKey(limited))

	h.post("/v1/chat/completions", oaReq("nope", false))
	h.post("/v1/chat/completions", oaReq("dess", false))
	h.postWith("/v1/chat/completions", oaReq("secret", false), map[string]string{"Authorization": "Bearer " + limited.Key})
	// rejected authentication is not logged (there is no key to attribute it to)
	h.postWith("/v1/chat/completions", oaReq("dess", false), map[string]string{"Authorization": "Bearer wrong"})

	logs := h.logs()
	if len(logs) != 3 {
		t.Fatalf("want 3 logs, got %d", len(logs))
	}
	notFound, allFailed, forbidden := logs[2], logs[1], logs[0]
	if notFound.Success || notFound.HTTPStatus != 404 || notFound.PublicModel != "" || notFound.Error != "model not found" {
		t.Errorf("not found: %+v", notFound)
	}
	if allFailed.Success || allFailed.HTTPStatus != 400 || len(allFailed.Attempts) != 4 ||
		!strings.Contains(allFailed.Error, "bad request") || allFailed.Provider != "" {
		t.Errorf("all failed: %+v", allFailed)
	}
	if forbidden.Success || forbidden.HTTPStatus != 403 || forbidden.KeyName != "limited" || forbidden.PublicModel != "secret" {
		t.Errorf("forbidden: %+v", forbidden)
	}
}

func TestRequestLogStreamFailureIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/truncate")
	h.post("/v1/chat/completions", oaReq("dess", true))
	l := h.logs()[0]
	if l.Success || l.HTTPStatus != 200 || l.Provider != "oa" || !strings.Contains(l.Error, "ended before completion") {
		t.Fatalf("truncated stream log: %+v", l)
	}
}

func TestRequestLogQueryAndStats(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/fail500", "an/ok") // always falls back
	h.model("fast", "oa/ok")
	h.model("broken", "oa/fail400")
	other := &store.APIKey{Name: "other", Enabled: true}
	mustNil(t, h.st.CreateKey(other))

	for i := 0; i < 3; i++ {
		h.post("/v1/chat/completions", oaReq("fast", false))
	}
	h.post("/v1/chat/completions", oaReq("dess", false))
	h.postWith("/v1/chat/completions", oaReq("broken", false), map[string]string{"Authorization": "Bearer " + other.Key})
	h.st.FlushLogs()

	q := func(lq store.LogQuery) ([]*store.RequestLog, int64) {
		t.Helper()
		items, total, err := h.st.QueryLogs(lq)
		mustNil(t, err)
		return items, total
	}
	if _, total := q(store.LogQuery{}); total != 5 {
		t.Fatalf("total %d", total)
	}
	if items, total := q(store.LogQuery{Model: "fast"}); total != 3 || items[0].PublicModel != "fast" {
		t.Errorf("model filter: %d", total)
	}
	if items, total := q(store.LogQuery{Provider: "an"}); total != 1 || items[0].PublicModel != "dess" {
		t.Errorf("provider filter: %d", total)
	}
	if items, total := q(store.LogQuery{Status: "failed"}); total != 1 || items[0].PublicModel != "broken" {
		t.Errorf("status filter: %d", total)
	}
	if _, total := q(store.LogQuery{Status: "success"}); total != 4 {
		t.Errorf("success filter: %d", total)
	}
	if items, total := q(store.LogQuery{Fallback: true}); total != 1 || items[0].PublicModel != "dess" {
		t.Errorf("fallback filter: %d", total)
	}
	if items, total := q(store.LogQuery{KeyID: other.ID}); total != 1 || items[0].KeyName != "other" {
		t.Errorf("key filter: %d", total)
	}
	// pagination: newest first, stable pages
	page1, _ := q(store.LogQuery{Limit: 2})
	page2, _ := q(store.LogQuery{Limit: 2, Offset: 2})
	page3, _ := q(store.LogQuery{Limit: 2, Offset: 4})
	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 || page1[0].ID <= page1[1].ID || page1[1].ID <= page2[0].ID {
		t.Errorf("pagination: %d %d %d", len(page1), len(page2), len(page3))
	}

	st, err := h.st.GetStats(time.Now().Add(-time.Hour).UnixMilli(), int64(5*time.Minute/time.Millisecond))
	mustNil(t, err)
	if st.Total.Requests != 5 || st.Total.Success != 4 || st.Total.Failed != 1 || st.Total.Fallback != 1 {
		t.Errorf("stats total: %+v", st.Total)
	}
	byKey := map[string]int64{}
	for _, r := range st.ByModel {
		byKey["model:"+r.Key] = r.Requests
	}
	for _, r := range st.ByTarget {
		byKey["target:"+r.Key] = r.Requests
	}
	for _, r := range st.ByKey {
		byKey["key:"+r.Key] = r.Requests
	}
	for k, want := range map[string]int64{"model:fast": 3, "model:dess": 1, "model:broken": 1, "target:oa/ok": 3, "target:an/ok": 1, "target:(none)": 1, "key:tester": 4, "key:other": 1} {
		if byKey[k] != want {
			t.Errorf("stats %s = %d, want %d", k, byKey[k], want)
		}
	}
	var timelineSum int64
	for _, r := range st.Timeline {
		timelineSum += r.Requests
	}
	if timelineSum != 5 {
		t.Errorf("timeline sum %d", timelineSum)
	}
}

func TestRequestLogAdminTest(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/ok")
	res := h.gw.TestModel(context.Background(), "dess", "anthropic", false, "")
	if !res.OK || res.HTTPStatus != http.StatusOK {
		t.Fatalf("%+v", res)
	}
	l := h.logs()[0]
	if l.KeyName != "(admin test)" || l.KeyID != 0 || l.Inbound != "anthropic" || !l.Success {
		t.Fatalf("admin test log: %+v", l)
	}
	// direct provider tests bypass routing and are not logged
	p, _ := h.st.ListProviders()
	h.gw.TestTarget(context.Background(), p[0], "ok", "", false)
	if n := len(h.logs()); n != 1 {
		t.Fatalf("provider test was logged (%d logs)", n)
	}
}
