package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// An Anthropic request survives a trip through the IR with everything the
// other protocols cannot express: cache breakpoints, thinking signatures,
// redacted thinking, documents, tool results with images and errors.
func TestIRRoundTripsAnthropicRequest(t *testing.T) {
	in := `{"model":"m","max_tokens":1024,"stream":true,"top_k":5,
	  "system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],
	  "messages":[
	    {"role":"user","content":[
	      {"type":"text","text":"read this"},
	      {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"},"title":"spec.pdf","cache_control":{"type":"ephemeral"}},
	      {"type":"image","source":{"type":"url","url":"https://x/a.png"}}]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"hmm","signature":"sig123"},
	      {"type":"redacted_thinking","data":"opaque"},
	      {"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"boom"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}
	  ],
	  "tools":[{"name":"f","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},{"type":"web_search_20250305","name":"web_search"}],
	  "tool_choice":{"type":"auto","disable_parallel_tool_use":true},
	  "thinking":{"type":"enabled","budget_tokens":500},
	  "metadata":{"user_id":"u1"}}`
	r, err := ParseRequest(ProtoAnthropic, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EmitRequest(ProtoAnthropic, r, EmitOptions{Model: "up", DefaultMaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(out, &got)
	_ = json.Unmarshal([]byte(in), &want)
	want["model"] = "up"
	// thinking: the client's budget is kept, sampling untouched
	for _, k := range []string{"system", "messages", "tools", "tool_choice", "thinking", "metadata", "max_tokens", "stream", "top_k"} {
		g, _ := json.Marshal(got[k])
		w, _ := json.Marshal(want[k])
		if string(g) != string(w) {
			t.Errorf("%s:\n got %s\nwant %s", k, g, w)
		}
	}
}

// Responses input with files, image tool outputs and reasoning reaches an
// Anthropic upstream with its structure, not as flattened text.
func TestIRResponsesToAnthropicKeepsStructure(t *testing.T) {
	in := `{"model":"m","instructions":"inst","reasoning":{"effort":"medium"},
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,QUJD"},{"type":"input_image","image_url":"data:image/png;base64,AA","detail":"high"}]},
	    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
	    {"type":"function_call","call_id":"c1","name":"shot","arguments":"{\"x\":1}"},
	    {"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"here"},{"type":"input_image","image_url":"data:image/png;base64,BB"}]}
	  ],
	  "tools":[{"type":"function","name":"shot","parameters":{"type":"object"}}]}`
	r, err := ParseRequest(ProtoResponses, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EmitRequest(ProtoAnthropic, r, EmitOptions{Model: "claude-sonnet-4-5", DefaultMaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages: %s", out)
	}
	u := msgs[0].(map[string]any)["content"].([]any)
	if len(u) != 3 || u[1].(map[string]any)["type"] != "document" || u[1].(map[string]any)["title"] != "a.pdf" || u[2].(map[string]any)["type"] != "image" {
		t.Fatalf("user blocks: %v", u)
	}
	// reasoning synthesized elsewhere has no signature: dropped for Anthropic
	a := msgs[1].(map[string]any)["content"].([]any)
	if len(a) != 1 || a[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant blocks: %v", a)
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	content := tr["content"].([]any)
	if tr["type"] != "tool_result" || len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("tool_result keeps its image: %v", tr)
	}
	if m["thinking"].(map[string]any)["budget_tokens"].(float64) != 8192 || m["system"].([]any)[0].(map[string]any)["text"] != "inst" {
		t.Fatalf("params: %v", m)
	}
}

// The same request to a Chat upstream flattens what Chat cannot carry.
func TestIRResponsesToChatFlattens(t *testing.T) {
	in := `{"model":"m","input":[
	    {"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"here"},{"type":"input_image","image_url":"data:image/png;base64,BB"}]},
	    {"type":"message","role":"user","content":"next"}]}`
	r, err := ParseRequest(ProtoResponses, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EmitRequest(ProtoOpenAI, r, EmitOptions{Model: "up"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "tool" || msgs[1].(map[string]any)["content"] != "here" {
		t.Fatalf("messages: %s", out)
	}
	parts := msgs[2].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "image_url" || parts[1].(map[string]any)["text"] != "next" {
		t.Fatalf("image hoisted into the user turn: %v", parts)
	}
}

// Anthropic request to a Responses upstream: thinking becomes a reasoning
// item and the effort level, documents become input files.
func TestIRAnthropicToResponses(t *testing.T) {
	in := `{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":16000},
	  "system":"sys",
	  "messages":[
	    {"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"},"title":"spec.pdf"},{"type":"text","text":"q"}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"a"}]}]}`
	r, err := ParseRequest(ProtoAnthropic, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EmitRequest(ProtoResponses, r, EmitOptions{Model: "up"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	input := m["input"].([]any)
	if len(input) != 4 || input[0].(map[string]any)["role"] != "system" {
		t.Fatalf("input: %s", out)
	}
	file := input[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if file["type"] != "input_file" || file["filename"] != "spec.pdf" || !strings.HasPrefix(file["file_data"].(string), "data:application/pdf;base64,") {
		t.Fatalf("file: %v", file)
	}
	if input[2].(map[string]any)["type"] != "reasoning" || input[3].(map[string]any)["content"] != "a" {
		t.Fatalf("assistant: %v", input[2:])
	}
	if m["reasoning"].(map[string]any)["effort"] != "high" || m["max_output_tokens"].(float64) != 100 || m["store"] != false {
		t.Fatalf("params: %v", m)
	}
}

// A Chat assistant turn with reasoning_content keeps it on the way to a
// Responses upstream (as a reasoning item) and loses it only for Anthropic,
// which would reject an unsigned thinking block.
func TestIRChatReasoningCarried(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a","reasoning_content":"why"},{"role":"user","content":"more"}]}`
	r, err := ParseRequest(ProtoOpenAI, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := EmitRequest(ProtoResponses, r, EmitOptions{Model: "up"})
	if !strings.Contains(string(rs), `"type":"reasoning"`) || !strings.Contains(string(rs), `"text":"why"`) {
		t.Fatalf("responses: %s", rs)
	}
	an, _ := EmitRequest(ProtoAnthropic, r, EmitOptions{Model: "up", DefaultMaxTokens: 10})
	if strings.Contains(string(an), "thinking") {
		t.Fatalf("anthropic: %s", an)
	}
	// a PDF data URL in a Chat message is a document, not an image
	in = `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,QUJD"}}]}]}`
	r, _ = ParseRequest(ProtoOpenAI, []byte(in))
	an, _ = EmitRequest(ProtoAnthropic, r, EmitOptions{Model: "up", DefaultMaxTokens: 10})
	if !strings.Contains(string(an), `"type":"document"`) {
		t.Fatalf("document: %s", an)
	}
}

// Responses upstream stream straight to an Anthropic client, including a
// custom tool call, through one decoder and one encoder.
func TestIRResponsesStreamToAnthropicClient(t *testing.T) {
	c := NewStreamConverter(ProtoAnthropic, ProtoResponses, "pub", false, nil)
	var evs []SSEEvent
	for _, e := range [][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"think"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","delta":"ok "}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","delta":"then"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":2,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"apply_patch","input":""}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":2,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"apply_patch","input":"*** Begin"}}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":6}}}`},
	} {
		evs = append(evs, c.Process(SSEEvent{Event: e[0], Data: e[1]})...)
	}
	blocks, stop, usage := checkAnthropicStream(t, evs)
	if len(blocks) != 3 || blocks[0].typ != "thinking" || blocks[0].text != "think" || blocks[0].signature != ConvertedSignature ||
		blocks[1].typ != "text" || blocks[1].text != "ok then" ||
		blocks[2].typ != "tool_use" || blocks[2].id != "c1" || blocks[2].name != "apply_patch" || blocks[2].json != `{"input":"*** Begin"}` {
		t.Fatalf("blocks: %+v", blocks)
	}
	if stop != "tool_use" || usage["input_tokens"].(float64) != 4 || usage["output_tokens"].(float64) != 6 || !c.Complete() {
		t.Fatalf("stop=%q usage=%v complete=%v", stop, usage, c.Complete())
	}
}

// Non-stream responses cross any pair; a Responses answer reaches an
// Anthropic client with its custom tool call intact.
func TestIRResponsesResponseToAnthropic(t *testing.T) {
	body := `{"id":"resp_1","status":"completed","output":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]},
	  {"type":"message","content":[{"type":"output_text","text":"done"}]},
	  {"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"diff"}],
	  "usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens":3}}`
	out, u, err := ConvertResponse(ProtoAnthropic, ProtoResponses, []byte(body), "pub", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	content := m["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["thinking"] != "why" || content[1].(map[string]any)["text"] != "done" ||
		content[2].(map[string]any)["input"].(map[string]any)["input"] != "diff" || m["stop_reason"] != "tool_use" {
		t.Fatalf("content: %s", out)
	}
	if u.Input != 5 || u.Cached != 2 || m["usage"].(map[string]any)["input_tokens"].(float64) != 3 {
		t.Fatalf("usage: %+v %v", u, m["usage"])
	}
}
