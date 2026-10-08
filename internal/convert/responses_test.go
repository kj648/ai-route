package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatToResponsesRequestFields(t *testing.T) {
	in := `{"model":"m","stream":true,"max_completion_tokens":50,"temperature":0.2,"reasoning_effort":"high","user":"u1",
	  "messages":[
	    {"role":"system","content":"sys"},
	    {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]},
	    {"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"42"}
	  ],
	  "tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
	  "tool_choice":{"type":"function","function":{"name":"f"}},
	  "response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"},"strict":true}}}`
	out, err := ChatToResponsesRequest([]byte(in), "up")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	input := m["input"].([]any)
	img := input[1].(map[string]any)["content"].([]any)[1].(map[string]any)
	if img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,AA" || img["detail"] != "auto" {
		t.Fatalf("image: %v", img)
	}
	call := input[3].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["arguments"] != "{}" ||
		input[4].(map[string]any)["type"] != "function_call_output" || input[4].(map[string]any)["output"] != "42" {
		t.Fatalf("tool turns: %v", input)
	}
	f := m["text"].(map[string]any)["format"].(map[string]any)
	if m["max_output_tokens"].(float64) != 50 || m["reasoning"].(map[string]any)["effort"] != "high" || m["store"] != false ||
		f["type"] != "json_schema" || f["name"] != "x" || f["strict"] != true || m["stream_options"] != nil {
		t.Fatalf("params: %v", m)
	}
	if tc := m["tool_choice"].(map[string]any); tc["type"] != "function" || tc["name"] != "f" {
		t.Fatalf("tool_choice: %v", tc)
	}
}

func TestResponsesToChatRequestFields(t *testing.T) {
	in := `{"model":"m","instructions":"inst","input":"hello","stream":true,
	  "tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read","parameters":null,"strict":true}]}],
	  "tool_choice":{"type":"allowed_tools","mode":"required","tools":[]},
	  "text":{"format":{"type":"json_object"}},"reasoning":{"effort":7}}`
	out, err := ResponsesToChatRequest([]byte(in), "up")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs := m["messages"].([]any)
	fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if msgs[0].(map[string]any)["content"] != "inst" || msgs[1].(map[string]any)["content"] != "hello" ||
		fn["name"] != "read" || fn["strict"] != true || m["tool_choice"] != "required" ||
		m["response_format"].(map[string]any)["type"] != "json_object" || m["reasoning_effort"] != nil ||
		m["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("converted: %s", out)
	}
}

func TestChatToResponsesStreamCustomTools(t *testing.T) {
	tools := ParseResponsesTools([]byte(`{"tools":[{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}]}`))
	c := NewChatToResponsesStream("pub", tools)
	var evs []SSEEvent
	for _, d := range []string{
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"apply_patch","arguments":"{\"input\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"*** Begin\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
		`[DONE]`,
	} {
		evs = append(evs, c.Process(SSEEvent{Data: d})...)
	}
	evs = append(evs, c.Finish()...)
	var types []string
	var final map[string]any
	for _, e := range evs {
		types = append(types, e.Event)
		var d map[string]any
		_ = json.Unmarshal([]byte(e.Data), &d)
		if r, ok := d["response"].(map[string]any); ok {
			final = r
		}
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "response.custom_tool_call_input.done") || strings.Count(joined, "response.function_call_arguments.delta") != 1 ||
		types[len(types)-1] != "response.incomplete" {
		t.Fatalf("events: %v", types)
	}
	out := final["output"].([]any)
	custom, fn := out[0].(map[string]any), out[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["input"] != "*** Begin" || custom["call_id"] != "c1" ||
		fn["type"] != "function_call" || fn["namespace"] != "mcp" ||
		final["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" || final["usage"].(map[string]any)["input_tokens"].(float64) != 3 {
		t.Fatalf("final: %v", final)
	}
}

func TestResponsesToChatCustomToolAndIncomplete(t *testing.T) {
	body := `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
	  "output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"apply_patch","input":"diff"}],
	  "usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":1},"output_tokens":2,"total_tokens":7}}`
	out, u, err := ResponsesToChatResponse([]byte(body), "pub")
	if err != nil {
		t.Fatal(err)
	}
	if u.Input != 5 || u.Cached != 1 || u.Output != 2 {
		t.Fatalf("usage: %+v", u)
	}
	if !strings.Contains(string(out), `"arguments":"{\"input\":\"diff\"}"`) || !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("chat: %s", out)
	}
	// and back to Responses for a client that declared the custom tool
	rs, err := ChatToResponsesResponse(out, "pub", ParseResponsesTools([]byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`)))
	if err != nil || !strings.Contains(string(rs), `"type":"custom_tool_call"`) || !strings.Contains(string(rs), `"input":"diff"`) || !strings.Contains(string(rs), `"status":"incomplete"`) {
		t.Fatalf("round trip: %s %v", rs, err)
	}
}
