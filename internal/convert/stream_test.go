package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// anBlock is one content block reassembled from an Anthropic stream.
type anBlock struct {
	typ, id, name string
	text, json    string // text / thinking, or partial_json joined
	signature     string
}

// checkAnthropicStream parses an Anthropic event stream and fails the test
// if it is malformed: deltas must address an open block, blocks must be
// opened and closed in order, and message_start / message_stop must frame
// everything. It returns the blocks and the stop_reason.
func checkAnthropicStream(t *testing.T, evs []SSEEvent) ([]anBlock, string, map[string]any) {
	t.Helper()
	var blocks []anBlock
	open := -1
	started, stopped := false, false
	stop := ""
	var usage map[string]any
	for i, ev := range evs {
		var d map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			t.Fatalf("event %d: bad JSON %q: %v", i, ev.Data, err)
		}
		typ, _ := d["type"].(string)
		if typ != ev.Event {
			t.Fatalf("event %d: event name %q != type %q", i, ev.Event, typ)
		}
		if stopped {
			t.Fatalf("event %d after message_stop: %s", i, ev.Data)
		}
		if !started && typ != "message_start" {
			t.Fatalf("event %d before message_start: %s", i, ev.Data)
		}
		idx := -1
		if f, ok := d["index"].(float64); ok {
			idx = int(f)
		}
		switch typ {
		case "message_start":
			started = true
		case "content_block_start":
			if open >= 0 || idx != len(blocks) {
				t.Fatalf("event %d: block %d started while block %d open (have %d blocks)", i, idx, open, len(blocks))
			}
			cb := d["content_block"].(map[string]any)
			b := anBlock{typ: cb["type"].(string)}
			b.id, _ = cb["id"].(string)
			b.name, _ = cb["name"].(string)
			blocks = append(blocks, b)
			open = idx
		case "content_block_delta":
			if idx != open {
				t.Fatalf("event %d: delta for block %d but block %d is open", i, idx, open)
			}
			delta := d["delta"].(map[string]any)
			b := &blocks[idx]
			switch delta["type"] {
			case "text_delta":
				b.text += delta["text"].(string)
			case "thinking_delta":
				b.text += delta["thinking"].(string)
			case "input_json_delta":
				b.json += delta["partial_json"].(string)
			case "signature_delta":
				b.signature = delta["signature"].(string)
			default:
				t.Fatalf("event %d: unknown delta %v", i, delta)
			}
		case "content_block_stop":
			if idx != open {
				t.Fatalf("event %d: stop for block %d but block %d is open", i, idx, open)
			}
			open = -1
		case "message_delta":
			if open >= 0 {
				t.Fatalf("event %d: message_delta with block %d open", i, open)
			}
			stop, _ = d["delta"].(map[string]any)["stop_reason"].(string)
			usage, _ = d["usage"].(map[string]any)
		case "message_stop":
			stopped = true
		case "error":
		default:
			t.Fatalf("event %d: unexpected type %q", i, typ)
		}
	}
	return blocks, stop, usage
}

func runStream(c StreamConverter, data ...string) []SSEEvent {
	var out []SSEEvent
	for _, d := range data {
		out = append(out, c.Process(SSEEvent{Data: d})...)
	}
	return out
}

func TestOpenAIToAnthropicStreamText(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	)
	blocks, stop, usage := checkAnthropicStream(t, evs)
	if len(blocks) != 1 || blocks[0].typ != "text" || blocks[0].text != "Hello" {
		t.Fatalf("blocks: %+v", blocks)
	}
	if stop != "end_turn" {
		t.Fatalf("stop_reason: %q", stop)
	}
	if usage["input_tokens"].(float64) != 6 || usage["cache_read_input_tokens"].(float64) != 4 || usage["output_tokens"].(float64) != 2 {
		t.Fatalf("usage: %v", usage)
	}
	if !c.Complete() || c.Err() != "" {
		t.Fatalf("complete=%v err=%q", c.Complete(), c.Err())
	}
	if u := c.Usage(); u.Input != 10 || u.Cached != 4 || u.Output != 2 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestOpenAIToAnthropicStreamInterleavedToolCalls(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"content":"Let me check."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		// a second call starts while the first is still streaming
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"grep","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	blocks, stop, _ := checkAnthropicStream(t, evs)
	if len(blocks) != 3 {
		t.Fatalf("expected text + 2 tool_use blocks: %+v", blocks)
	}
	a, b := blocks[1], blocks[2]
	if a.typ != "tool_use" || a.id != "call_a" || a.name != "read" || a.json != `{"path":"a.go"}` {
		t.Fatalf("first call: %+v", a)
	}
	if b.typ != "tool_use" || b.id != "call_b" || b.name != "grep" || b.json != `{"q":"x"}` {
		t.Fatalf("second call: %+v", b)
	}
	if stop != "tool_use" {
		t.Fatalf("stop_reason: %q", stop)
	}
}

func TestOpenAIToAnthropicStreamToolCallsWithoutFinish(t *testing.T) {
	// some vendors never send finish_reason: tool calls still end as tool_use
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
	)
	evs = append(evs, c.Finish()...)
	blocks, stop, _ := checkAnthropicStream(t, evs)
	if len(blocks) != 1 || blocks[0].json != "{}" || stop != "tool_use" {
		t.Fatalf("blocks=%+v stop=%q", blocks, stop)
	}
}

func TestOpenAIToAnthropicStreamTruncatedToolCall(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write","arguments":"{\"content\":\"very lo"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`[DONE]`,
	)
	_, stop, _ := checkAnthropicStream(t, evs)
	if stop != "max_tokens" {
		t.Fatalf("a call cut off by max_tokens must not be reported as tool_use: %q", stop)
	}
}

func TestOpenAIToAnthropicStreamThinkingThenText(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"more"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	blocks, _, _ := checkAnthropicStream(t, evs)
	if len(blocks) != 2 || blocks[0].typ != "thinking" || blocks[0].text != "think more" || blocks[0].signature != ConvertedSignature ||
		blocks[1].typ != "text" || blocks[1].text != "answer" {
		t.Fatalf("blocks: %+v", blocks)
	}
}

func TestOpenAIToAnthropicStreamError(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c,
		`{"choices":[{"delta":{"content":"par"}}]}`,
		`{"error":{"message":"upstream exploded","type":"server_error"}}`,
		`{"choices":[{"delta":{"content":"ignored"}}]}`,
	)
	last := evs[len(evs)-1]
	if last.Event != "error" || !strings.Contains(last.Data, "upstream exploded") {
		t.Fatalf("expected an error event last: %+v", evs)
	}
	if c.Err() != "upstream exploded" {
		t.Fatalf("Err: %q", c.Err())
	}
	if more := c.Process(SSEEvent{Data: `{"choices":[{"delta":{"content":"x"}}]}`}); more != nil {
		t.Fatalf("events after error: %+v", more)
	}
}

func TestOpenAIToAnthropicStreamCutOff(t *testing.T) {
	c := NewOpenAIToAnthropicStream("pub")
	evs := runStream(c, `{"choices":[{"delta":{"content":"par"}}]}`)
	if c.Complete() {
		t.Fatal("a stream without finish_reason or [DONE] is not complete")
	}
	// Finish closes the open block and ends the message cleanly
	evs = append(evs, c.Finish()...)
	blocks, stop, _ := checkAnthropicStream(t, evs)
	if len(blocks) != 1 || blocks[0].text != "par" || stop != "end_turn" {
		t.Fatalf("blocks=%+v stop=%q", blocks, stop)
	}
	if again := c.Finish(); again != nil {
		t.Fatalf("second Finish emitted events: %+v", again)
	}
}

// oaChunks parses OpenAI chunks, failing on anything that is not a chunk
// or [DONE]; it returns the chunks and whether [DONE] was last.
func oaChunks(t *testing.T, evs []SSEEvent) ([]map[string]any, bool) {
	t.Helper()
	var out []map[string]any
	for i, ev := range evs {
		if ev.Event != "" {
			t.Fatalf("event %d: OpenAI chunks have no event name: %q", i, ev.Event)
		}
		if ev.Data == "[DONE]" {
			if i != len(evs)-1 {
				t.Fatalf("[DONE] at %d of %d", i, len(evs))
			}
			return out, true
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		out = append(out, d)
	}
	return out, false
}

func TestAnthropicToOpenAIStreamToolUse(t *testing.T) {
	c := NewAnthropicToOpenAIStream("pub", true)
	var evs []SSEEvent
	for _, e := range [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":20,"cache_read_input_tokens":5,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		evs = append(evs, c.Process(SSEEvent{Event: e[0], Data: e[1]})...)
	}
	chunks, done := oaChunks(t, evs)
	if !done || !c.Complete() {
		t.Fatalf("done=%v complete=%v", done, c.Complete())
	}
	var text, args, name, id string
	var finish any
	var usage map[string]any
	for _, ch := range chunks {
		if u, ok := ch["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := ch["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if f := choice["finish_reason"]; f != nil {
			finish = f
		}
		delta := choice["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text += s
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, tc := range calls {
			m := tc.(map[string]any)
			if m["index"].(float64) != 0 {
				t.Fatalf("tool index: %v", m)
			}
			fn := m["function"].(map[string]any)
			if s, ok := m["id"].(string); ok {
				id = s
			}
			if s, ok := fn["name"].(string); ok {
				name = s
			}
			args += fn["arguments"].(string)
		}
	}
	if text != "hi" || id != "toolu_1" || name != "read" || args != `{"p":1}` || finish != "tool_calls" {
		t.Fatalf("text=%q id=%q name=%q args=%q finish=%v", text, id, name, args, finish)
	}
	// OpenAI prompt_tokens include the cached part; Anthropic input_tokens exclude it
	if usage["prompt_tokens"].(float64) != 25 || usage["completion_tokens"].(float64) != 9 ||
		usage["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64) != 5 {
		t.Fatalf("usage chunk: %v", usage)
	}
	if u := c.Usage(); u.Input != 25 || u.Cached != 5 || u.Output != 9 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestAnthropicToOpenAIStreamError(t *testing.T) {
	c := NewAnthropicToOpenAIStream("pub", false)
	evs := c.Process(SSEEvent{Event: "error", Data: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`})
	if len(evs) != 2 || evs[1].Data != "[DONE]" || !strings.Contains(evs[0].Data, "busy") {
		t.Fatalf("events: %+v", evs)
	}
	if !strings.Contains(c.Err(), "busy") {
		t.Fatalf("Err: %q", c.Err())
	}
}

func TestOpenAIToAnthropicResponseTruncatedToolCall(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"write","arguments":"{\"content\":\"lo"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	out, _, err := OpenAIToAnthropicResponse([]byte(body), "pub")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMap(t, out); m["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason: %v", m["stop_reason"])
	}
	body = strings.Replace(body, `"finish_reason":"length"`, `"finish_reason":"stop"`, 1)
	out, _, _ = OpenAIToAnthropicResponse([]byte(body), "pub")
	if m := decodeMap(t, out); m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", m["stop_reason"])
	}
}

// TestAnthropicUpstreamToResponsesClient drives the hub chain
// Anthropic -> Chat -> Responses end to end.
func TestAnthropicUpstreamToResponsesClient(t *testing.T) {
	c := NewStreamConverter(ProtoResponses, ProtoAnthropic, "pub", false, []byte(`{"tools":[{"type":"function","name":"read"}]}`))
	var evs []SSEEvent
	for _, e := range [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		evs = append(evs, c.Process(SSEEvent{Event: e[0], Data: e[1]})...)
	}
	evs = append(evs, c.Finish()...)
	var types []string
	var final map[string]any
	for _, e := range evs {
		types = append(types, e.Event)
		var d map[string]any
		_ = json.Unmarshal([]byte(e.Data), &d)
		if r, ok := d["response"].(map[string]any); ok && d["type"] == "response.completed" {
			final = r
		}
	}
	if types[0] != "response.created" || final == nil {
		t.Fatalf("events: %v", types)
	}
	out := final["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "message" || out[1].(map[string]any)["type"] != "function_call" ||
		out[1].(map[string]any)["call_id"] != "toolu_1" {
		t.Fatalf("output: %v", out)
	}
	if final["usage"].(map[string]any)["input_tokens"].(float64) != 7 || final["usage"].(map[string]any)["output_tokens"].(float64) != 3 {
		t.Fatalf("usage: %v", final["usage"])
	}
	if !c.Complete() || c.Err() != "" {
		t.Fatalf("complete=%v err=%q", c.Complete(), c.Err())
	}
	if u := c.Usage(); u.Input != 7 || u.Output != 3 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestSSEReaderCRLFAndEventReset(t *testing.T) {
	r := NewSSEReader(strings.NewReader("event: a\r\ndata: 1\r\n\r\ndata: 2\r\n\r\n: ping\r\n\r\ndata: 3"))
	var got []SSEEvent
	for {
		ev, err := r.Next()
		if err != nil {
			break
		}
		got = append(got, ev)
	}
	if len(got) != 3 || got[0].Event != "a" || got[0].Data != "1" || got[1].Event != "" || got[1].Data != "2" || got[2].Data != "3" {
		t.Fatalf("events: %+v", got)
	}
}
