package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenAIToAnthropicRequest(t *testing.T) {
	in := `{
	  "model":"coder","temperature":1.5,"stop":"END","stream":true,"parallel_tool_calls":false,
	  "messages":[
	    {"role":"system","content":"sys A"},
	    {"role":"developer","content":[{"type":"text","text":"sys B"}]},
	    {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"result 1"},
	    {"role":"user","content":"continue"}
	  ],
	  "tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
	  "tool_choice":"required"
	}`
	out, err := OpenAIToAnthropicRequest([]byte(in), "up", 4096)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["model"] != "up" || m["max_tokens"].(float64) != 4096 || m["temperature"].(float64) != 1 {
		t.Fatalf("basic fields: %s", out)
	}
	if sys := m["system"].([]any); len(sys) != 2 {
		t.Fatalf("system: %v", m["system"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected tool_result and following user text merged into one user turn: %s", out)
	}
	img := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if img["source"].(map[string]any)["media_type"] != "image/png" {
		t.Fatalf("image: %v", img)
	}
	tu := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["input"].(map[string]any)["a"].(float64) != 1 {
		t.Fatalf("tool_use: %v", tu)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["text"] != "continue" {
		t.Fatalf("merged user: %v", last)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "any" || tc["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice: %v", tc)
	}
	if m["stop_sequences"].([]any)[0] != "END" {
		t.Fatal("stop")
	}
}

func TestAnthropicToOpenAIRequest(t *testing.T) {
	in := `{
	  "model":"coder","max_tokens":1000,"stream":true,
	  "system":[{"type":"text","text":"be nice","cache_control":{"type":"ephemeral"}}],
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"x"},{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r1"}]},{"type":"text","text":"next"}]}
	  ],
	  "tools":[{"name":"f","description":"d","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	  "tool_choice":{"type":"tool","name":"f"}
	}`
	out, err := AnthropicToOpenAIRequest([]byte(in), "up")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs := m["messages"].([]any)
	roles := []string{}
	for _, x := range msgs {
		roles = append(roles, x.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles: %v\n%s", roles, out)
	}
	as := msgs[2].(map[string]any)
	if as["reasoning_content"] != "hmm" || as["content"] != "calling" {
		t.Fatalf("assistant: %v", as)
	}
	call := as["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["arguments"] != `{"a":1}` {
		t.Fatalf("args: %v", call)
	}
	if msgs[3].(map[string]any)["content"] != "r1" || msgs[4].(map[string]any)["content"] != "next" {
		t.Fatalf("tool/user: %s", out)
	}
	if len(m["tools"].([]any)) != 1 {
		t.Fatal("server tools must be dropped")
	}
	if m["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "f" {
		t.Fatal("tool_choice")
	}
	if m["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatal("include_usage")
	}
}

func TestRewriteModelKeepsUnknownFields(t *testing.T) {
	out, clientUsage, err := RewriteModel([]byte(`{"model":"a","stream":true,"custom_field":{"x":1}}`), "b", ProtoOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["model"] != "b" || m["custom_field"] == nil || clientUsage {
		t.Fatalf("%s", out)
	}
	if m["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatal("include_usage not injected")
	}
}

func TestSSEReader(t *testing.T) {
	body := ": keepalive\n\nevent: a\ndata: {\"x\":1}\n\ndata: line1\ndata: line2\n\ndata: [DONE]"
	r := NewSSEReader(strings.NewReader(body))
	var got []SSEEvent
	for {
		ev, err := r.Next()
		if err != nil {
			break
		}
		got = append(got, ev)
	}
	if len(got) != 3 || got[0].Event != "a" || got[1].Data != "line1\nline2" || got[2].Data != "[DONE]" {
		t.Fatalf("%+v", got)
	}
}

func TestValidateResponse(t *testing.T) {
	if ValidateResponse([]byte(`{"error":{"message":"x"}}`), ProtoOpenAI) == nil {
		t.Fatal("error body should be rejected")
	}
	if ValidateResponse([]byte(`{"choices":[]}`), ProtoOpenAI) != nil {
		t.Fatal("valid body rejected")
	}
	if ValidateResponse([]byte(`{"type":"error","error":{}}`), ProtoAnthropic) == nil {
		t.Fatal("anthropic error accepted")
	}
}
