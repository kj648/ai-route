package gateway

import (
	"strings"
	"testing"
	"time"

	"ai-route/internal/store"
)

func TestCaptureRecordsBodiesOfMatchingRequests(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail400", "an/ok-model")
	h.model("other", "oa/ok-model")
	k := h.st.Snapshot().Keys[h.key]
	rule := &store.CaptureRule{KeyID: k.ID, Model: "coder", Total: 2}
	mustNil(t, h.st.CreateCaptureRule(rule, time.Hour))

	h.post("/v1/chat/completions", oaReq("other", false)) // other model: not captured
	resp, body := h.post("/v1/chat/completions", oaReq("coder", true))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	reqID := resp.Header.Get("X-Route-Request-Id")
	h.post("/v1/chat/completions", oaReq("coder", false))
	h.post("/v1/chat/completions", oaReq("coder", false)) // rule used up
	h.gw.WaitCaptures()

	list, err := h.st.ListCaptures()
	mustNil(t, err)
	if len(list) != 2 {
		t.Fatalf("%d captures, want 2", len(list))
	}
	var first *store.Capture
	for _, c := range list {
		if c.CaptureData != nil {
			t.Fatal("listing carries bodies")
		}
		if c.RequestID == reqID {
			first = c
		}
	}
	if first == nil {
		t.Fatalf("stream request %s not captured: %+v", reqID, list)
	}
	c, err := h.st.GetCapture(first.ID)
	mustNil(t, err)
	if c.Status != 200 || c.Model != "coder" || c.KeyName != "tester" || !strings.Contains(c.ClientRequest, `"hi"`) {
		t.Fatalf("capture %+v", c)
	}
	if len(c.Attempts) != 2 {
		t.Fatalf("attempts %+v", c.Attempts)
	}
	a0, a1 := c.Attempts[0], c.Attempts[1]
	if a0.Target != "oa/fail400" || a0.Status != 400 || !strings.Contains(a0.Response, "bad request") || !strings.Contains(a0.Request, `"fail400"`) {
		t.Fatalf("failed attempt %+v", a0)
	}
	if a1.Protocol != "anthropic" || a1.Status != 200 || !strings.Contains(a1.Request, `"max_tokens"`) || !strings.Contains(a1.Response, "message_start") || strings.Contains(a1.URL, "k-an") {
		t.Fatalf("converted attempt %+v", a1)
	}
	if !strings.Contains(c.ClientResponse, "chat.completion.chunk") || !strings.Contains(c.ClientResponse, "[DONE]") {
		t.Fatalf("client response %q", c.ClientResponse)
	}
	rules, err := h.st.ListCaptureRules()
	mustNil(t, err)
	if len(rules) != 1 || rules[0].Remaining != 0 {
		t.Fatalf("rules %+v", rules)
	}
}

func TestCaptureOffCostsNothing(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok-model")
	mustNil(t, h.st.CreateCaptureRule(&store.CaptureRule{Model: "elsewhere", Total: 5}, time.Hour))
	h.post("/v1/chat/completions", oaReq("coder", false))
	h.gw.WaitCaptures()
	list, err := h.st.ListCaptures()
	mustNil(t, err)
	if len(list) != 0 {
		t.Fatalf("captured without a matching rule: %+v", list)
	}
	rules, _ := h.st.ListCaptureRules()
	if rules[0].Remaining != 5 {
		t.Fatalf("non-matching request took a slot: %+v", rules[0])
	}
}
