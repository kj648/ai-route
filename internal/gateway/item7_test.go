package gateway

import (
	"fmt"
	"strings"
	"testing"

	"ai-route/internal/store"
)

func TestRerankPassthrough(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.Prices = map[string]store.Price{"bge-*": {Input: 1000}} })
	h.model("rr", "an/bge-reranker", "oa/bge-reranker") // an has no OpenAI endpoint: skipped
	req := map[string]any{"model": "rr", "query": "q", "documents": []string{"a", "b"}, "top_n": 1}
	resp, body := h.post("/v1/rerank", req)
	if resp.StatusCode != 200 || !strings.Contains(body, `"relevance_score":0.9`) || resp.Header.Get("X-Route-Target") != "oa/bge-reranker" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	up := h.mock.Body("bge-reranker")
	if up["query"] != "q" || up["top_n"].(float64) != 1 {
		t.Fatalf("upstream body: %v", up)
	}
	l := h.logs()[0]
	if l.Inbound != "rerank" || l.UpstreamProtocol != "rerank" || l.InputTokens != 12 || l.Stream || l.CostSource != "price" {
		t.Fatalf("log: %+v", l)
	}
	if resp, _ := h.post("/rerank", req); resp.StatusCode != 200 {
		t.Fatal("/rerank alias")
	}
}

func TestWeightedGroupSplitsAndSticks(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok*3 | both/ok", "an/ok")
	conv := func(i int) map[string]any {
		req := oaReq("coder", false)
		req["messages"] = []map[string]any{{"role": "system", "content": "same system"}, {"role": "user", "content": fmt.Sprintf("conversation %d", i)}}
		return req
	}
	counts := map[string]int{}
	first := map[int]string{}
	for i := 0; i < 200; i++ {
		resp, body := h.post("/v1/chat/completions", conv(i))
		if resp.StatusCode != 200 {
			t.Fatal(body)
		}
		target := resp.Header.Get("X-Route-Target")
		counts[target]++
		first[i] = target
	}
	// weight 3:1 -> ~150 / 50; never the next priority while the group is healthy
	if counts["an/ok"] != 0 || counts["oa/ok"] < 120 || counts["oa/ok"] > 180 || counts["both/ok"] < 20 {
		t.Fatalf("distribution: %v", counts)
	}
	// a conversation keeps its upstream as it grows
	for i := 0; i < 20; i++ {
		req := conv(i)
		req["messages"] = append(req["messages"].([]map[string]any),
			map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "follow-up"})
		if resp, _ := h.post("/v1/chat/completions", req); resp.Header.Get("X-Route-Target") != first[i] {
			t.Fatalf("conversation %d moved from %s to %s", i, first[i], resp.Header.Get("X-Route-Target"))
		}
	}
}

func TestWeightedGroupFallsBackWithinGroup(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500 | both/ok", "an/ok")
	for i := 0; i < 10; i++ {
		req := oaReq("coder", false)
		req["messages"] = []map[string]any{{"role": "user", "content": fmt.Sprintf("c%d", i)}}
		resp, _ := h.post("/v1/chat/completions", req)
		if resp.Header.Get("X-Route-Target") != "both/ok" {
			t.Fatalf("request %d served by %q, want the healthy group member", i, resp.Header.Get("X-Route-Target"))
		}
	}
}
