package gateway

import (
	"strings"
	"testing"

	"ai-route/internal/metrics"
	"ai-route/internal/store"
)

func TestMetricsRecordRequests(t *testing.T) {
	h := newHarness(t)
	// a provider of its own: the metrics registry is global, so label sets
	// shared with other tests would carry their counts
	mustNil(t, h.st.CreateProvider(&store.Provider{Prefix: "mx", OpenAIBaseURL: h.mock.srv.URL + "/v1", APIKey: "k", Enabled: true}))
	h.model("metrics-ok", "mx/ok")
	h.model("metrics-fb", "mx/fail500", "mx/ok")
	if resp, body := h.post("/v1/chat/completions", oaReq("metrics-ok", false)); resp.StatusCode != 200 {
		t.Fatal(body)
	}
	if resp, body := h.post("/v1/chat/completions", oaReq("metrics-fb", true)); resp.StatusCode != 200 {
		t.Fatal(body)
	}
	h.postWith("/v1/chat/completions", oaReq("metrics-ok", false), map[string]string{"Authorization": "Bearer nope"})
	var b strings.Builder
	metrics.Write(&b)
	out := b.String()
	for _, want := range []string{
		`ai_route_requests_total{model="metrics-ok",provider="mx",inbound="openai",status="200"} 1`,
		`ai_route_requests_total{model="metrics-fb",provider="mx",inbound="openai",status="200"} 1`,
		`ai_route_fallbacks_total{model="metrics-fb"} 1`,
		`ai_route_upstream_attempts_total{provider="mx",target="mx/fail500",result="error"} 3`,
		`ai_route_upstream_attempts_total{provider="mx",target="mx/ok",result="ok"} 2`,
		`ai_route_tokens_total{model="metrics-ok",provider="mx",kind="input"}`,
		`ai_route_request_duration_seconds_count{model="metrics-ok",provider="mx"} 1`,
		`ai_route_build_info{version=`,
		`ai_route_log_dropped_total 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if t.Failed() {
		t.Log(out)
	}
}
