package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ai-route/internal/store"
)

func TestEmbeddingsRouting(t *testing.T) {
	h := newHarness(t)
	// an/* has no OpenAI endpoint -> skipped; fail500 is retried then replaced
	mustNil(t, h.st.CreateModel(&store.Model{Name: "vec", Tags: []string{"embedding"}, Targets: []string{"an/emb", "oa/fail500", "both/emb-up"}, Enabled: true}))
	resp, body := h.post("/v1/embeddings", map[string]any{"model": "vec", "input": "hello", "stream": true})
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "both/emb-up" {
		t.Fatalf("status %d target %s: %s", resp.StatusCode, resp.Header.Get("X-Route-Target"), body)
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	mustNil(t, json.Unmarshal([]byte(body), &out))
	if len(out.Data) != 1 || len(out.Data[0].Embedding) != 3 {
		t.Fatalf("body: %s", body)
	}
	if got := h.mock.Body("emb-up"); got["model"] != "emb-up" || got["input"] != "hello" {
		t.Fatalf("upstream body: %v", got)
	}
	if h.mock.Hits("emb") != 0 {
		t.Fatal("anthropic-only provider must be skipped for embeddings")
	}
	l := h.logs()[0]
	if l.Inbound != "embeddings" || l.UpstreamProtocol != "embeddings" || l.Stream || !l.Success ||
		l.InputTokens != 4 || !l.Fallback || attemptTargets(l) != "oa/fail500 oa/fail500#1 oa/fail500#2 both/emb-up" {
		t.Fatalf("log: %+v attempts=%s", l, attemptTargets(l))
	}
}

func TestEmbeddingsNoCapableTarget(t *testing.T) {
	h := newHarness(t)
	h.model("vec", "an/emb")
	resp, body := h.post("/v1/embeddings", map[string]any{"model": "vec", "input": "x"})
	if resp.StatusCode != 503 || !strings.Contains(body, `"error"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestModelTagsInListing(t *testing.T) {
	h := newHarness(t)
	mustNil(t, h.st.CreateModel(&store.Model{Name: "dess", Description: "主力编码", Tags: []string{"chat", "reasoning", "ctx-1m", "chat", " "}, Targets: []string{"oa/x"}, Enabled: true}))
	for _, anthropic := range []bool{false, true} {
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+h.key)
		if anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := http.DefaultClient.Do(req)
		mustNil(t, err)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Data []struct {
				ID          string   `json:"id"`
				Tags        []string `json:"tags"`
				Description string   `json:"description"`
			} `json:"data"`
		}
		mustNil(t, json.Unmarshal(b, &out))
		if len(out.Data) != 1 || strings.Join(out.Data[0].Tags, ",") != "chat,reasoning,ctx-1m" || out.Data[0].Description != "主力编码" {
			t.Fatalf("anthropic=%v listing: %s", anthropic, b)
		}
	}
}
