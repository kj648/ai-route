package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ai-route/internal/convert"
	"ai-route/internal/store"
)

// ---------- mock upstream ----------

// mockUpstream serves both protocols. The upstream model name selects the
// behaviour: "fail500", "fail429", "fail400", "streamerr", "tool", anything else = ok.
type mockUpstream struct {
	mu       sync.Mutex
	hits     map[string]int
	lastBody map[string][]byte
	lastHdr  map[string]http.Header
	srv      *httptest.Server
}

func newMock(t *testing.T) *mockUpstream {
	m := &mockUpstream{hits: map[string]int{}, lastBody: map[string][]byte{}, lastHdr: map[string]http.Header{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockUpstream) Hits(model string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[model]
}

func (m *mockUpstream) Body(model string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var v map[string]any
	_ = json.Unmarshal(m.lastBody[model], &v)
	return v
}

func (m *mockUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	m.mu.Lock()
	m.hits[req.Model]++
	m.lastBody[req.Model] = body
	m.lastHdr[req.Model] = r.Header.Clone()
	m.mu.Unlock()

	anthropic := strings.HasSuffix(r.URL.Path, "/v1/messages")
	m.mu.Lock()
	hits := m.hits[req.Model]
	m.mu.Unlock()
	switch req.Model {
	case "flaky": // fails only on the first call
		if hits == 1 {
			http.Error(w, `{"error":{"message":"bad gateway"}}`, 502)
			return
		}
	case "rate429": // short rate limit, then ok
		if hits == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":{"message":"rate limited"}}`, 429)
			return
		}
	case "fail500":
		http.Error(w, `{"error":{"message":"boom"}}`, 500)
		return
	case "fail429":
		w.Header().Set("Retry-After", "120")
		http.Error(w, `{"error":{"message":"quota exceeded"}}`, 429)
		return
	case "fail400":
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
		return
	case "fail401":
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
		return
	case "fail402":
		http.Error(w, `{"error":{"message":"insufficient balance"}}`, 402)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": req.Model,
			"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
			"usage": map[string]any{"prompt_tokens": 4, "total_tokens": 4},
		})
		return
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		if anthropic {
			content := []map[string]any{
				{"type": "thinking", "thinking": "let me think", "signature": "sig"},
				{"type": "text", "text": "hello from " + req.Model},
			}
			stop := "end_turn"
			if req.Model == "tool" {
				content = append(content, map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "bj"}})
				stop = "tool_use"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant", "model": req.Model,
				"content": content, "stop_reason": stop,
				"usage": map[string]any{"input_tokens": 7, "output_tokens": 5, "cache_read_input_tokens": 3},
			})
			return
		}
		msg := map[string]any{"role": "assistant", "content": "hello from " + req.Model, "reasoning_content": "let me think"}
		finish := "stop"
		if req.Model == "tool" {
			msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"bj"}`}}}
			finish = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   oaMockUsage(req.Model),
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	send := func(event, data string) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
	}
	if req.Model == "streamerr" {
		if anthropic {
			send("error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
		} else {
			send("", `{"error":{"message":"overloaded"}}`)
		}
		return
	}
	if req.Model == "truncate" || req.Model == "midstreamerr" {
		if anthropic {
			send("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"x","usage":{"input_tokens":7,"output_tokens":1}}}`)
			send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)
			if req.Model == "midstreamerr" {
				send("error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
			}
		} else {
			send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
			if req.Model == "midstreamerr" {
				send("", `{"error":{"message":"overloaded"}}`)
			}
		}
		return
	}
	if anthropic {
		send("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"x","usage":{"input_tokens":7,"output_tokens":1}}}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hel"}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`)
		send("content_block_stop", `{"type":"content_block_stop","index":1}`)
		stop := "end_turn"
		if req.Model == "tool" {
			send("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`)
			send("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
			send("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"bj\"}"}}`)
			send("content_block_stop", `{"type":"content_block_stop","index":2}`)
			stop = "tool_use"
		}
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":9}}`)
		send("message_stop", `{"type":"message_stop"}`)
		return
	}
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}`)
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`)
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`)
	finish := "stop"
	if req.Model == "tool" {
		send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`)
		send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`)
		send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"bj\"}"}}]},"finish_reason":null}]}`)
		finish = "tool_calls"
	}
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`)
	usage, _ := json.Marshal(oaMockUsage(req.Model))
	send("", `{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":`+string(usage)+`}`)
	send("", `[DONE]`)
}

// oaMockUsage is the OpenAI usage the mock reports; model "costly" also
// reports the charge the way OpenRouter does.
func oaMockUsage(model string) map[string]any {
	u := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	if model == "costly" {
		u["cost"] = 0.0123
	}
	return u
}

// ---------- harness ----------

type harness struct {
	t    *testing.T
	st   *store.Store
	gw   *Gateway
	srv  *httptest.Server
	mock *mockUpstream
	key  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mock := newMock(t)
	mustNil(t, st.CreateProvider(&store.Provider{Prefix: "both", OpenAIBaseURL: mock.srv.URL + "/v1", AnthropicBaseURL: mock.srv.URL, APIKey: "k-both", Enabled: true}))
	mustNil(t, st.CreateProvider(&store.Provider{Prefix: "oa", OpenAIBaseURL: mock.srv.URL + "/v1", APIKey: "k-oa", Enabled: true}))
	mustNil(t, st.CreateProvider(&store.Provider{Prefix: "an", AnthropicBaseURL: mock.srv.URL, APIKey: "k-an", Enabled: true, Headers: map[string]string{"X-Extra": "1"}}))
	mustNil(t, st.CreateProvider(&store.Provider{Prefix: "off", OpenAIBaseURL: mock.srv.URL + "/v1", APIKey: "k", Enabled: false}))
	k := &store.APIKey{Name: "tester", Enabled: true}
	mustNil(t, st.CreateKey(k))
	settings := store.DefaultSettings()
	settings.RetryBackoffMs = 5 // keep tests fast
	mustNil(t, st.UpdateSettings(settings))
	gw := New(st)
	mux := http.NewServeMux()
	gw.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, st: st, gw: gw, srv: srv, mock: mock, key: k.Key}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) model(name string, targets ...string) {
	h.t.Helper()
	mustNil(h.t, h.st.CreateModel(&store.Model{Name: name, Targets: targets, Enabled: true}))
}

func (h *harness) post(path string, body any) (*http.Response, string) {
	h.t.Helper()
	return h.postWith(path, body, nil)
}

// postWith sends a request with extra headers (they override the defaults).
func (h *harness) postWith(path string, body any, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/9.9")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func oaReq(model string, stream bool) map[string]any {
	return map[string]any{"model": model, "stream": stream, "messages": []map[string]any{{"role": "user", "content": "hi"}}}
}

func anReq(model string, stream bool) map[string]any {
	return map[string]any{"model": model, "stream": stream, "max_tokens": 100, "messages": []map[string]any{{"role": "user", "content": "hi"}}}
}

func sseEvents(t *testing.T, body string) []convert.SSEEvent {
	t.Helper()
	r := convert.NewSSEReader(strings.NewReader(body))
	var out []convert.SSEEvent
	for {
		ev, err := r.Next()
		if err != nil {
			return out
		}
		out = append(out, ev)
	}
}

// waitLogs waits for the async log writer.
func (h *harness) logs() []*store.RequestLog {
	h.t.Helper()
	h.st.FlushLogs()
	l, _, err := h.st.QueryLogs(store.LogQuery{Limit: 100})
	mustNil(h.t, err)
	return l
}

// ---------- tests ----------

func TestOpenAIPassthroughRewritesModel(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/up-model")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hello from up-model") {
		t.Fatalf("unexpected body: %s", body)
	}
	if got := h.mock.Body("up-model")["model"]; got != "up-model" {
		t.Fatalf("upstream got model %v", got)
	}
	h.mock.mu.Lock()
	hdr := h.mock.lastHdr["up-model"]
	h.mock.mu.Unlock()
	if hdr.Get("Authorization") != "Bearer k-oa" {
		t.Fatalf("upstream auth = %q", hdr.Get("Authorization"))
	}
	if hdr.Get("User-Agent") != "claude-cli/9.9" {
		t.Fatalf("user agent not forwarded: %q", hdr.Get("User-Agent"))
	}
	logs := h.logs()
	if len(logs) != 1 || !logs[0].Success || logs[0].InputTokens != 10 || logs[0].Provider != "oa" {
		t.Fatalf("bad log: %+v", logs[0])
	}
}

func TestFallbackOn500(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500", "an/ok-model")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Route-Target") != "an/ok-model" {
		t.Fatalf("target = %s", resp.Header.Get("X-Route-Target"))
	}
	// converted from anthropic
	var r convert.OAResponse
	mustNil(t, json.Unmarshal([]byte(body), &r))
	if r.Model != "coder" || r.Choices[0].Message.ReasoningContent != "let me think" {
		t.Fatalf("bad converted response: %s", body)
	}
	logs := h.logs()
	// 1 try + 2 retries on the failing target, then the fallback
	if !logs[0].Fallback || len(logs[0].Attempts) != 4 || logs[0].Attempts[0].HTTPStatus != 500 || logs[0].Attempts[2].Retry != 2 {
		t.Fatalf("bad log: %+v", logs[0])
	}
	if logs[0].InputTokens != 10 || logs[0].CachedTokens != 3 {
		t.Fatalf("usage not normalized: %+v", logs[0])
	}
}

func TestDisabledProviderSkipped(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "off/whatever", "missing/x", "oa/ok")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 || h.mock.Hits("whatever") != 0 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestQuotaCooldownMovesProviderToEnd(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail429", "an/ok-model")
	h.post("/v1/chat/completions", oaReq("coder", false))
	if h.mock.Hits("fail429") != 1 {
		t.Fatal("first request should try fail429")
	}
	// second request: oa is cooling down, an/ok-model is tried first
	resp, _ := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 || h.mock.Hits("fail429") != 1 {
		t.Fatalf("cooling provider was retried first (hits=%d)", h.mock.Hits("fail429"))
	}
	var open bool
	for _, s := range h.gw.Breaker.Status() {
		if s.Kind == "provider" && s.Name == "oa" && s.Open && s.RemainingSecond >= 100 {
			open = true // Retry-After: 120 honoured
		}
	}
	if !open {
		t.Fatalf("provider oa not cooling: %+v", h.gw.Breaker.Status())
	}
}

func TestSoftFailuresThreshold(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500", "oa/ok")
	// default threshold: 2 failed requests (each = 1 try + 2 retries)
	for i := 0; i < 2; i++ {
		h.post("/v1/chat/completions", oaReq("coder", false))
	}
	if h.mock.Hits("fail500") != 6 {
		t.Fatalf("hits=%d", h.mock.Hits("fail500"))
	}
	h.post("/v1/chat/completions", oaReq("coder", false))
	if h.mock.Hits("fail500") != 6 {
		t.Fatal("target should be cooling after 2 consecutive failed requests")
	}
}

func TestAllFailReturnsLastStatus(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500", "an/fail400")
	resp, body := h.post("/v1/messages", anReq("coder", false))
	if resp.StatusCode != 400 || !strings.Contains(body, "all upstream targets failed") || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestStreamFirstEventErrorFallsBack(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/streamerr", "an/streamerr", "both/ok")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", true))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "both/ok" {
		t.Fatalf("status %d target %s body %s", resp.StatusCode, resp.Header.Get("X-Route-Target"), body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream not terminated: %s", body)
	}
}

func TestOpenAIStreamPassthroughHidesInjectedUsage(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	_, body := h.post("/v1/chat/completions", oaReq("coder", true))
	if strings.Contains(body, `"usage"`) {
		t.Fatalf("usage chunk should be hidden when client did not ask: %s", body)
	}
	if h.mock.Body("ok")["stream_options"] == nil {
		t.Fatal("include_usage not injected upstream")
	}
	logs := h.logs()
	if logs[0].InputTokens != 10 || logs[0].OutputTokens != 5 || !logs[0].Stream {
		t.Fatalf("usage not recorded: %+v", logs[0])
	}
}

func TestOpenAIClientAnthropicUpstreamStream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/tool")
	req := oaReq("coder", true)
	req["stream_options"] = map[string]any{"include_usage": true}
	_, body := h.post("/v1/chat/completions", req)
	evs := sseEvents(t, body)
	var text, reasoning, args, toolName, finish string
	var usageSeen bool
	for _, ev := range evs {
		if ev.Data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string               `json:"content"`
					ReasoningContent string               `json:"reasoning_content"`
					ToolCalls        []convert.OAToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *convert.OAUsage `json:"usage"`
		}
		mustNil(t, json.Unmarshal([]byte(ev.Data), &c))
		if c.Usage != nil {
			usageSeen = c.Usage.PromptTokens == 7 && c.Usage.CompletionTokens == 9
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			reasoning += ch.Delta.ReasoningContent
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					toolName = tc.Function.Name
				}
				args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text != "hello" || reasoning != "hmm" || toolName != "get_weather" || args != `{"city":"bj"}` || finish != "tool_calls" || !usageSeen {
		t.Fatalf("text=%q reasoning=%q tool=%q args=%q finish=%q usage=%v\n%s", text, reasoning, toolName, args, finish, usageSeen, body)
	}
	if evs[len(evs)-1].Data != "[DONE]" {
		t.Fatal("missing [DONE]")
	}
}

func TestAnthropicClientOpenAIUpstreamStream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/tool")
	_, body := h.post("/v1/messages", anReq("coder", true))
	evs := sseEvents(t, body)
	var names []string
	var text, thinking, args, stop string
	open := map[float64]bool{}
	for _, ev := range evs {
		names = append(names, ev.Event)
		var d map[string]any
		mustNil(t, json.Unmarshal([]byte(ev.Data), &d))
		if d["type"] != ev.Event {
			t.Fatalf("event name %q != data type %v", ev.Event, d["type"])
		}
		switch ev.Event {
		case "content_block_start":
			open[d["index"].(float64)] = true
		case "content_block_stop":
			delete(open, d["index"].(float64))
		case "content_block_delta":
			if !open[d["index"].(float64)] {
				t.Fatalf("delta for closed block: %s", ev.Data)
			}
			delta := d["delta"].(map[string]any)
			switch delta["type"] {
			case "text_delta":
				text += delta["text"].(string)
			case "thinking_delta":
				thinking += delta["thinking"].(string)
			case "input_json_delta":
				args += delta["partial_json"].(string)
			}
		case "message_delta":
			stop = d["delta"].(map[string]any)["stop_reason"].(string)
			if d["usage"].(map[string]any)["output_tokens"].(float64) != 5 {
				t.Fatalf("usage missing in message_delta: %s", ev.Data)
			}
		}
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" || len(open) != 0 {
		t.Fatalf("bad event order: %v", names)
	}
	if text != "hello" || thinking != "hmm" || args != `{"city":"bj"}` || stop != "tool_use" {
		t.Fatalf("text=%q thinking=%q args=%q stop=%q", text, thinking, args, stop)
	}
	// request conversion check
	up := h.mock.Body("tool")
	if up["max_tokens"].(float64) != 100 || up["stream_options"] == nil {
		t.Fatalf("bad converted request: %v", up)
	}
}

func TestAnthropicClientOpenAIUpstreamNonStream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/tool")
	resp, body := h.post("/v1/messages", anReq("coder", false))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var r convert.ANResponse
	mustNil(t, json.Unmarshal([]byte(body), &r))
	if r.StopReason != "tool_use" || len(r.Content) != 3 || r.Content[2].Name != "get_weather" || string(r.Content[2].Input) != `{"city":"bj"}` {
		t.Fatalf("bad response: %s", body)
	}
}

func TestAnthropicPassthroughStreamAndHeaders(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/ok")
	b, _ := json.Marshal(anReq("coder", true))
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/messages?beta=true", bytes.NewReader(b))
	req.Header.Set("x-api-key", h.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")
	resp, err := http.DefaultClient.Do(req)
	mustNil(t, err)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(out), "event: message_stop") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	h.mock.mu.Lock()
	hdr := h.mock.lastHdr["ok"]
	h.mock.mu.Unlock()
	if hdr.Get("x-api-key") != "k-an" || hdr.Get("anthropic-beta") == "" || hdr.Get("X-Extra") != "1" {
		t.Fatalf("headers: %v", hdr)
	}
	logs := h.logs()
	if logs[0].InputTokens != 7 || logs[0].OutputTokens != 9 {
		t.Fatalf("usage: %+v", logs[0])
	}
}

func TestForcedProtocol(t *testing.T) {
	h := newHarness(t)
	mustNil(t, h.st.CreateProvider(&store.Provider{Prefix: "oc", OpenAIBaseURL: h.mock.srv.URL + "/v1", AnthropicBaseURL: h.mock.srv.URL, APIKey: "x", Enabled: true,
		ModelProtocols: map[string]string{"mini*": "anthropic"}}))
	h.model("coder", "oc/minimax")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	if h.mock.Body("minimax")["max_tokens"] == nil { // only present after OpenAI->Anthropic conversion
		t.Fatal("expected anthropic upstream")
	}
}

func TestAuthAndModelErrors(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	h.model("secret", "oa/ok")
	mustNil(t, h.st.CreateModel(&store.Model{Name: "fast", Aliases: []string{"claude-*haiku*"}, Targets: []string{"oa/fast-up"}, Enabled: true}))

	// alias glob
	resp, _ := h.post("/v1/messages", anReq("claude-3-5-haiku-20241022", false))
	if resp.StatusCode != 200 || h.mock.Hits("fast-up") != 1 {
		t.Fatalf("alias not resolved: %d", resp.StatusCode)
	}
	// unknown model
	resp, _ = h.post("/v1/chat/completions", oaReq("nope", false))
	if resp.StatusCode != 404 {
		t.Fatalf("want 404 got %d", resp.StatusCode)
	}
	// restricted key
	k := &store.APIKey{Name: "limited", Enabled: true, AllowedModels: []string{"coder"}}
	mustNil(t, h.st.CreateKey(k))
	h.key = k.Key
	resp, _ = h.post("/v1/chat/completions", oaReq("secret", false))
	if resp.StatusCode != 403 {
		t.Fatalf("want 403 got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	r2, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if !strings.Contains(string(b), `"coder"`) || strings.Contains(string(b), "secret") {
		t.Fatalf("models list: %s", b)
	}
	// bad key
	h.key = "sk-wrong"
	resp, _ = h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 got %d", resp.StatusCode)
	}
}

func TestPrefixRenameUpdatesTargets(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok", "an/ok")
	ps, _ := h.st.ListProviders()
	for _, p := range ps {
		if p.Prefix == "oa" {
			p.Prefix = "openai2"
			p.APIKey = ""
			mustNil(t, h.st.UpdateProvider(p))
			got, _ := h.st.GetProvider(p.ID)
			if got.APIKey != "k-oa" {
				t.Fatal("empty api key should keep existing key")
			}
		}
	}
	ms, _ := h.st.ListModels()
	if ms[0].Targets[0] != "openai2/ok" || ms[0].Targets[1] != "an/ok" {
		t.Fatalf("targets: %v", ms[0].Targets)
	}
}

func TestAdminTestModel(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500", "an/ok")
	res := h.gw.TestModel(t.Context(), "coder", "openai", true, "")
	if !res.OK || res.Target != "an/ok" || res.Reply != "hello" || len(res.Attempts) != 4 {
		t.Fatalf("%+v", res)
	}
	p, _ := h.st.ListProviders()
	tr := h.gw.TestTarget(t.Context(), p[0], "ok", "anthropic", false)
	if !tr.OK || tr.Reply != "hello from ok" {
		t.Fatalf("%+v", tr)
	}
}

func TestTruncatedAndErroredStreamsAreFailures(t *testing.T) {
	cases := []struct {
		target, path string
		body         map[string]any
		want         string
	}{
		{"oa/truncate", "/v1/chat/completions", oaReq("m", true), "ended before completion"},
		{"an/truncate", "/v1/messages", anReq("m", true), "ended before completion"},
		{"oa/truncate", "/v1/messages", anReq("m", true), "ended before completion"},
		{"an/truncate", "/v1/chat/completions", oaReq("m", true), "ended before completion"},
		{"oa/midstreamerr", "/v1/chat/completions", oaReq("m", true), "upstream stream error"},
		{"an/midstreamerr", "/v1/messages", anReq("m", true), "upstream stream error"},
		{"oa/midstreamerr", "/v1/messages", anReq("m", true), "upstream stream error"},
		{"an/midstreamerr", "/v1/chat/completions", oaReq("m", true), "upstream stream error"},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.model("m", c.target)
		_, body := h.post(c.path, c.body)
		if !strings.Contains(body, "partial") || !strings.Contains(body, "error") {
			t.Errorf("%s via %s: client not told about the error: %s", c.target, c.path, body)
		}
		if c.path == "/v1/messages" && strings.Contains(body, "message_stop") {
			t.Errorf("%s via %s: fake clean finish sent", c.target, c.path)
		}
		logs := h.logs()
		if logs[0].Success || !strings.Contains(logs[0].Error, c.want) {
			t.Errorf("%s via %s: log %+v", c.target, c.path, logs[0])
		}
	}
}

func TestConvertedThinkingStrippedForAnthropicUpstream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/ok")
	req := anReq("coder", false)
	req["messages"] = []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": []map[string]any{
			{"type": "thinking", "thinking": "from openai", "signature": convert.ConvertedSignature},
			{"type": "thinking", "thinking": "real", "signature": "abc"},
			{"type": "text", "text": "hello"},
		}},
		{"role": "user", "content": "again"},
	}
	resp, body := h.post("/v1/messages", req)
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	up, _ := json.Marshal(h.mock.Body("ok")["messages"])
	if strings.Contains(string(up), "from openai") || !strings.Contains(string(up), `"signature":"abc"`) {
		t.Fatalf("upstream messages: %s", up)
	}
}

func TestTransientErrorsRetrySameTarget(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/flaky", "an/ok")
	h.model("limited", "oa/rate429", "an/ok")
	for _, model := range []string{"coder", "limited"} {
		resp, body := h.post("/v1/chat/completions", oaReq(model, false))
		if resp.StatusCode != 200 || strings.HasPrefix(resp.Header.Get("X-Route-Target"), "an/") {
			t.Fatalf("%s: should recover on the same target, got %s: %s", model, resp.Header.Get("X-Route-Target"), body)
		}
	}
	if h.mock.Hits("ok") != 0 {
		t.Fatal("fallback must not be used for transient errors")
	}
	for _, s := range h.gw.Breaker.Status() {
		if s.Open {
			t.Fatalf("recovered target must not cool down: %+v", s)
		}
	}
	logs := h.logs()
	for _, l := range logs {
		if l.Fallback || len(l.Attempts) != 2 || l.Attempts[1].Retry != 1 {
			t.Fatalf("log: %+v", l)
		}
	}
}

func TestNonTransientErrorsSwitchImmediately(t *testing.T) {
	h := newHarness(t)
	h.model("bad", "oa/fail400", "an/ok")
	h.model("quota", "oa/fail429", "an/ok") // Retry-After: 120 => plan exhausted
	h.post("/v1/chat/completions", oaReq("bad", false))
	h.post("/v1/chat/completions", oaReq("quota", false))
	if h.mock.Hits("fail400") != 1 || h.mock.Hits("fail429") != 1 || h.mock.Hits("ok") != 2 {
		t.Fatalf("400 hits=%d 429 hits=%d ok=%d", h.mock.Hits("fail400"), h.mock.Hits("fail429"), h.mock.Hits("ok"))
	}
}

func TestStreamFirstEventErrorIsRetried(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/streamerr", "an/ok")
	h.post("/v1/chat/completions", oaReq("coder", true))
	if h.mock.Hits("streamerr") != 3 {
		t.Fatalf("hits=%d", h.mock.Hits("streamerr"))
	}
}

func TestUserAgentPolicy(t *testing.T) {
	h := newHarness(t)
	mustNil(t, h.st.CreateProvider(&store.Provider{Prefix: "fixed", OpenAIBaseURL: h.mock.srv.URL + "/v1", APIKey: "x", Enabled: true,
		UAMode: "override", UserAgent: "my-agent/1.0"}))
	mustNil(t, h.st.CreateProvider(&store.Provider{Prefix: "pass", OpenAIBaseURL: h.mock.srv.URL + "/v1", APIKey: "x", Enabled: true,
		UserAgent: "fallback-agent/2.0"}))
	h.model("a", "fixed/ua-fixed")
	h.model("b", "pass/ua-pass")
	h.post("/v1/chat/completions", oaReq("a", false)) // client UA: claude-cli/9.9
	h.post("/v1/chat/completions", oaReq("b", false))
	ua := func(model string) string {
		h.mock.mu.Lock()
		defer h.mock.mu.Unlock()
		return h.mock.lastHdr[model].Get("User-Agent")
	}
	if ua("ua-fixed") != "my-agent/1.0" || ua("ua-pass") != "claude-cli/9.9" {
		t.Fatalf("fixed=%q pass=%q", ua("ua-fixed"), ua("ua-pass"))
	}
	p := &store.Provider{UserAgent: "fallback-agent/2.0", UAMode: "passthrough"}
	if upstreamUA(p, "") != "fallback-agent/2.0" || upstreamUA(&store.Provider{}, "") != defaultUA {
		t.Fatal("fallback UA not applied")
	}
	if err := h.st.CreateProvider(&store.Provider{Prefix: "bad", OpenAIBaseURL: "http://x/v1", UAMode: "override"}); err == nil {
		t.Fatal("override without user_agent accepted")
	}
}

func TestOpenAIClientAnthropicUpstreamKeepsCacheAndFormat(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/ok")
	req := oaReq("coder", false)
	req["messages"] = []map[string]any{
		{"role": "system", "content": []map[string]any{{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral"}}}},
		{"role": "user", "content": "hi", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	req["response_format"] = map[string]any{"type": "json_object"}
	resp, body := h.post("/v1/chat/completions", req)
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	up := h.mock.Body("ok")
	sys := up["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["cache_control"] == nil ||
		!strings.Contains(sys[1].(map[string]any)["text"].(string), "valid JSON") {
		t.Fatalf("upstream system: %v", sys)
	}
	user := up["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if user["cache_control"] == nil {
		t.Fatalf("upstream user: %v", user)
	}
	// the mock is not api.anthropic.com, so no native structured output
	if _, ok := up["output_config"]; ok {
		t.Fatalf("output_config sent to a compatible upstream: %v", up)
	}
}

func TestIsOfficialAnthropic(t *testing.T) {
	for base, want := range map[string]bool{
		"https://api.anthropic.com":              true,
		"https://API.anthropic.com/v1":           true,
		"https://open.bigmodel.cn/api/anthropic": false,
		"https://api.anthropic.com.evil.test":    false,
		"":                                       false,
	} {
		if got := isOfficialAnthropic(base); got != want {
			t.Errorf("%q: got %v", base, got)
		}
	}
}
