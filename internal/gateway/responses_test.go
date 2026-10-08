package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"ai-route/internal/convert"
	"ai-route/internal/store"
)

// OpenAI Responses API: clients on /v1/responses reach every upstream, and
// Responses-only upstream models are reachable from Chat / Messages clients.

func rsReq(model string, stream bool) map[string]any {
	return map[string]any{"model": model, "stream": stream, "store": false,
		"input": []map[string]any{
			{"role": "developer", "content": "be brief"},
			{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
		"max_output_tokens": 100,
	}
}

// rsEvents parses a Responses SSE body: event types and the terminal response.
func rsEvents(t *testing.T, body string) ([]string, map[string]any) {
	t.Helper()
	var types []string
	var final map[string]any
	seq := -1
	for _, ev := range sseEvents(t, body) {
		if ev.Data == "[DONE]" {
			t.Fatal("Responses streams have no [DONE]")
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			t.Fatalf("bad event %q", ev.Data)
		}
		typ, _ := d["type"].(string)
		if ev.Event != typ {
			t.Fatalf("event line %q != type %q", ev.Event, typ)
		}
		if n := int(d["sequence_number"].(float64)); n <= seq {
			t.Fatalf("sequence_number not increasing at %s", typ)
		} else {
			seq = n
		}
		types = append(types, typ)
		if r, ok := d["response"].(map[string]any); ok {
			final = r
		}
	}
	return types, final
}

func outputText(r map[string]any) string {
	var sb strings.Builder
	for _, it := range r["output"].([]any) {
		item := it.(map[string]any)
		if item["type"] == "message" {
			for _, p := range item["content"].([]any) {
				sb.WriteString(p.(map[string]any)["text"].(string))
			}
		}
	}
	return sb.String()
}

func TestResponsesClientToChatUpstream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	resp, body := h.post("/v1/responses", rsReq("coder", false))
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(body), &r)
	if r["object"] != "response" || r["status"] != "completed" || !strings.HasPrefix(r["id"].(string), "resp_") || outputText(r) != "hello from ok" {
		t.Fatalf("response: %s", body)
	}
	if u := r["usage"].(map[string]any); u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
		t.Fatalf("usage: %v", u)
	}
	// the upstream got a Chat request: developer -> system, max tokens kept
	up := h.mock.Body("ok")
	msgs := up["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "hi" || up["max_tokens"].(float64) != 100 {
		t.Fatalf("upstream chat body: %v", up)
	}
	if l := h.logs()[0]; l.Inbound != "responses" || l.UpstreamProtocol != "openai" || l.InputTokens != 10 {
		t.Fatalf("log: %+v", l)
	}

	// streaming, with a tool call
	h.model("tooly", "oa/tool")
	req := rsReq("tooly", true)
	req["tools"] = []map[string]any{{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object"}, "strict": nil}}
	resp, body = h.post("/v1/responses", req)
	types, final := rsEvents(t, body)
	if resp.StatusCode != 200 || types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("events: %v", types)
	}
	if !strings.Contains(strings.Join(types, ","), "response.function_call_arguments.delta") {
		t.Fatalf("no argument deltas: %v", types)
	}
	var call map[string]any
	for _, it := range final["output"].([]any) {
		if it.(map[string]any)["type"] == "function_call" {
			call = it.(map[string]any)
		}
	}
	if call == nil || call["call_id"] != "call_1" || call["arguments"] != `{"city":"bj"}` || final["usage"].(map[string]any)["input_tokens"].(float64) != 10 {
		t.Fatalf("final response: %v", final)
	}
}

func TestResponsesPassthrough(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.ResponsesAPI = true })
	h.model("coder", "oa/ok")
	req := rsReq("coder", true)
	req["include"] = []string{"reasoning.encrypted_content"}
	resp, body := h.post("/v1/responses", req)
	types, final := rsEvents(t, body)
	if resp.StatusCode != 200 || types[len(types)-1] != "response.completed" || outputText(final) != "hello from ok" {
		t.Fatalf("%v %s", types, body)
	}
	up := h.mock.Body("ok")
	if up["input"] == nil || up["include"] == nil || up["messages"] != nil {
		t.Fatalf("not passed through: %v", up)
	}
	if l := h.logs()[0]; l.UpstreamProtocol != "responses" || l.InputTokens != 12 || l.CachedTokens != 2 || l.OutputTokens != 6 {
		t.Fatalf("log: %+v", l)
	}
	// a failed stream start is retried / switched like any other error
	h.model("bad", "oa/rsfail", "an/ok")
	resp, body = h.post("/v1/responses", rsReq("bad", true))
	if resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatalf("response.failed should fail over: %s", body)
	}
}

func TestChatAndMessagesClientsToResponsesUpstream(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) {
		p.ModelProtocols = map[string]string{"gpt-*": "responses", "tool": "responses"}
	})
	// mock serves the requested model name; gpt-x is a Responses-only model
	h.model("gpt", "oa/gpt-x")
	resp, body := h.post("/v1/chat/completions", oaReq("gpt", false))
	var chat convert.OAResponse
	_ = json.Unmarshal([]byte(body), &chat)
	if resp.StatusCode != 200 || len(chat.Choices) == 0 || !strings.Contains(string(chat.Choices[0].Message.Content), "hello from gpt-x") ||
		chat.Choices[0].Message.ReasoningContent != "hmm" || chat.Usage.PromptTokens != 12 {
		t.Fatalf("chat over responses: %s", body)
	}
	up := h.mock.Body("gpt-x")
	if up["input"] == nil || up["store"] != false || up["messages"] != nil {
		t.Fatalf("upstream should get a Responses body: %v", up)
	}

	// Chat client, streaming tool call
	h.model("tooly", "oa/tool")
	req := oaReq("tooly", true)
	req["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
	resp, body = h.post("/v1/chat/completions", req)
	if resp.StatusCode != 200 || !strings.Contains(body, `"name":"get_weather"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.HasSuffix(strings.TrimSpace(body), "[DONE]") {
		t.Fatalf("chat stream over responses: %s", body)
	}
	if tools := h.mock.Body("tool")["tools"].([]any); tools[0].(map[string]any)["name"] != "get_weather" || tools[0].(map[string]any)["strict"] != false {
		t.Fatalf("tools not flattened: %v", tools)
	}

	// Anthropic client, streaming: Responses -> Chat -> Anthropic
	resp, body = h.post("/v1/messages", anReq("gpt", true))
	if resp.StatusCode != 200 || !strings.Contains(body, "message_stop") || !strings.Contains(body, `"text":"hello "`) || !strings.Contains(body, "thinking_delta") {
		t.Fatalf("messages stream over responses: %s", body)
	}
	if l := h.logs()[0]; l.Inbound != "anthropic" || l.UpstreamProtocol != "responses" || l.InputTokens != 12 || !l.Success {
		t.Fatalf("log: %+v", l)
	}
}

func TestResponsesClientToAnthropicUpstream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/ok")
	resp, body := h.post("/v1/responses", rsReq("coder", true))
	types, final := rsEvents(t, body)
	if resp.StatusCode != 200 || types[len(types)-1] != "response.completed" || !strings.Contains(outputText(final), "hello") {
		t.Fatalf("%v\n%s", types, body)
	}
	if out := final["output"].([]any); out[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("thinking should become a reasoning item: %v", out)
	}
	up := h.mock.Body("ok")
	if up["system"] == nil || up["max_tokens"].(float64) != 100 {
		t.Fatalf("anthropic upstream body: %v", up)
	}
}

func TestResponsesCodexStyleRequest(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	// Codex: instructions as a developer message, freeform apply_patch,
	// replayed reasoning / custom tool calls, built-in web_search
	req := map[string]any{
		"model": "coder", "stream": false, "store": false, "include": []string{"reasoning.encrypted_content"},
		"prompt_cache_key": "sess-1", "tool_choice": "auto", "parallel_tool_calls": false,
		"reasoning": map[string]any{"effort": "medium", "summary": "auto"},
		"tools": []map[string]any{
			{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}, "strict": false},
			{"type": "custom", "name": "apply_patch", "description": "edit files", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
			{"type": "web_search"},
		},
		"input": []map[string]any{
			{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": "you are codex"}}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "fix it"}}},
			{"type": "reasoning", "id": "rs_0", "summary": []map[string]any{{"type": "summary_text", "text": "plan"}}, "encrypted_content": "xyz"},
			{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": "*** Begin Patch"},
			{"type": "custom_tool_call_output", "call_id": "c1", "output": "done"},
			{"type": "web_search_call", "id": "ws_1", "status": "completed"},
		},
	}
	resp, body := h.post("/v1/responses", req)
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	up := h.mock.Body("ok")
	msgs := up["messages"].([]any)
	assistant := msgs[2].(map[string]any)
	tc := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if assistant["reasoning_content"] != "plan" || tc["name"] != "apply_patch" || tc["arguments"] != `{"input":"*** Begin Patch"}` ||
		msgs[3].(map[string]any)["role"] != "tool" || len(msgs) != 4 {
		t.Fatalf("history: %v", msgs)
	}
	tools := up["tools"].([]any)
	if len(tools) != 2 || up["reasoning_effort"] != "medium" || up["parallel_tool_calls"] != false {
		t.Fatalf("tools / params: %v", up)
	}
	if tools[1].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["required"] == nil {
		t.Fatalf("custom tool should take an input string: %v", tools[1])
	}
	// stateful requests cannot be converted
	req["previous_response_id"] = "resp_old"
	if resp, body := h.post("/v1/responses", req); resp.StatusCode != 400 || !strings.Contains(body, "previous_response_id") {
		t.Fatalf("previous_response_id: %d %s", resp.StatusCode, body)
	}
}
