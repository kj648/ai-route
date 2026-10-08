package convert

import (
	"encoding/json"
	"sort"
	"time"
)

// Responses streams: every event is `event: <type>` + JSON with "type" and
// "sequence_number"; there is no [DONE], the stream ends with
// response.completed / response.incomplete / response.failed.

// ---------- pass-through: Responses ----------

type rsPass struct {
	usage    Usage
	finished bool
	err      string
}

// NewResponsesPassthrough forwards a Responses stream, recording usage.
func NewResponsesPassthrough() StreamConverter { return &rsPass{} }

func (p *rsPass) Process(ev SSEEvent) []SSEEvent {
	var d struct {
		Type     string      `json:"type"`
		Response *rsResponse `json:"response"`
	}
	_ = json.Unmarshal([]byte(ev.Data), &d)
	switch d.Type {
	case "response.completed", "response.incomplete":
		p.finished = true
		if d.Response != nil {
			p.usage = d.Response.Usage.toUsage()
		}
	case "response.failed", "error":
		msg, _ := isResponsesErrorEvent(ev)
		p.err = msg
	}
	return []SSEEvent{ev}
}

func (p *rsPass) Finish() []SSEEvent { return nil }
func (p *rsPass) Usage() Usage       { return p.usage }
func (p *rsPass) Complete() bool     { return p.finished }
func (p *rsPass) Err() string        { return p.err }

// ---------- Responses upstream -> Chat chunks ----------

type rsToChat struct {
	model        string
	includeUsage bool
	id           string
	created      int64
	started      bool
	toolIndex    map[string]int // item id -> chat tool index
	custom       map[string]bool
	nextTool     int
	usage        Usage
	finish       string
	done         bool
	finished     bool
	err          string
}

// NewResponsesToChatStream converts a Responses stream into Chat chunks.
func NewResponsesToChatStream(publicModel string, includeUsage bool) StreamConverter {
	return &rsToChat{model: publicModel, includeUsage: includeUsage, id: randID("chatcmpl-"),
		created: time.Now().Unix(), toolIndex: map[string]int{}, custom: map[string]bool{}}
}

func (c *rsToChat) chunk(delta map[string]any, finish any) SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return SSEEvent{Data: string(b)}
}

func (c *rsToChat) Process(ev SSEEvent) []SSEEvent {
	if c.done {
		return nil
	}
	var d struct {
		Type     string        `json:"type"`
		ItemID   string        `json:"item_id"`
		Delta    string        `json:"delta"`
		Item     *rsOutputItem `json:"item"`
		Response *rsResponse   `json:"response"`
		Message  string        `json:"message"`
	}
	if json.Unmarshal([]byte(ev.Data), &d) != nil {
		return nil
	}
	if d.Type == "" {
		d.Type = ev.Event
	}
	var out []SSEEvent
	if !c.started {
		c.started = true
		out = append(out, c.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	}
	switch d.Type {
	case "response.output_text.delta", "response.refusal.delta":
		if d.Delta != "" {
			out = append(out, c.chunk(map[string]any{"content": d.Delta}, nil))
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if d.Delta != "" {
			out = append(out, c.chunk(map[string]any{"reasoning_content": d.Delta}, nil))
		}
	case "response.output_item.added":
		if it := d.Item; it != nil && (it.Type == "function_call" || it.Type == "custom_tool_call") {
			idx := c.nextTool
			c.nextTool++
			c.toolIndex[it.ID] = idx
			c.custom[it.ID] = it.Type == "custom_tool_call"
			out = append(out, c.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "id": it.CallID, "type": "function",
				"function": map[string]any{"name": it.Name, "arguments": ""},
			}}}, nil))
		}
	case "response.function_call_arguments.delta":
		if idx, ok := c.toolIndex[d.ItemID]; ok && d.Delta != "" {
			out = append(out, c.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "function": map[string]any{"arguments": d.Delta},
			}}}, nil))
		}
	case "response.output_item.done":
		// a freeform tool's input arrives whole: wrap it as {"input": ...}
		if it := d.Item; it != nil && it.Type == "custom_tool_call" {
			if idx, ok := c.toolIndex[it.ID]; ok {
				args, _ := json.Marshal(map[string]string{"input": it.Input})
				out = append(out, c.chunk(map[string]any{"tool_calls": []map[string]any{{
					"index": idx, "function": map[string]any{"arguments": string(args)},
				}}}, nil))
			}
		}
	case "response.completed", "response.incomplete":
		c.finished = true
		reason := ""
		if r := d.Response; r != nil {
			c.usage = r.Usage.toUsage()
			if r.IncompleteDetails != nil {
				reason = r.IncompleteDetails.Reason
			}
			c.finish = rsFinishReason(r.Status, reason, c.nextTool > 0)
		}
		return append(out, c.Finish()...)
	case "response.failed", "error":
		msg, _ := isResponsesErrorEvent(ev)
		c.err = msg
		c.done = true
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}})
		return append(out, SSEEvent{Data: string(b)}, SSEEvent{Data: "[DONE]"})
	}
	return out
}

func (c *rsToChat) Finish() []SSEEvent {
	if c.done {
		return nil
	}
	c.done = true
	finish := c.finish
	if finish == "" {
		finish = rsFinishReason("completed", "", c.nextTool > 0)
	}
	out := []SSEEvent{c.chunk(map[string]any{}, finish)}
	if c.includeUsage {
		b, _ := json.Marshal(map[string]any{
			"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{}, "usage": oaUsageFrom(c.usage),
		})
		out = append(out, SSEEvent{Data: string(b)})
	}
	return append(out, SSEEvent{Data: "[DONE]"})
}

func (c *rsToChat) Usage() Usage   { return c.usage }
func (c *rsToChat) Complete() bool { return c.finished }
func (c *rsToChat) Err() string    { return c.err }

// ---------- Chat chunks -> Responses events ----------

type rsOpenItem struct {
	index  int
	id     string
	kind   string // reasoning | message | function_call | custom_tool_call
	text   []byte // reasoning summary / message text / arguments
	callID string
	name   string
	closed bool
}

type chatToRs struct {
	model   string
	tools   ResponsesToolInfo
	id      string
	created int64
	seq     int
	started bool
	items   []*rsOpenItem
	cur     *rsOpenItem         // open reasoning or message item
	calls   map[int]*rsOpenItem // chat tool index -> item
	output  []map[string]any    // finished items, by output index
	finish  string
	usage   Usage
	done    bool
}

// NewChatToResponsesStream converts Chat chunks into a Responses stream.
// tools come from the client's Responses request.
func NewChatToResponsesStream(publicModel string, tools ResponsesToolInfo) StreamConverter {
	return &chatToRs{model: publicModel, tools: tools, id: randID("resp_"), created: time.Now().Unix(), calls: map[int]*rsOpenItem{}}
}

func (c *chatToRs) ev(typ string, fields map[string]any) SSEEvent {
	fields["type"] = typ
	fields["sequence_number"] = c.seq
	c.seq++
	return jsonEvent(typ, fields)
}

func (c *chatToRs) response(status string) map[string]any {
	out := make([]map[string]any, 0, len(c.output))
	for _, it := range c.output {
		if it != nil {
			out = append(out, it)
		}
	}
	return rsResponseObject(c.id, c.model, status, c.created, out, c.usage, c.finish)
}

func (c *chatToRs) start() []SSEEvent {
	if c.started {
		return nil
	}
	c.started = true
	return []SSEEvent{
		c.ev("response.created", map[string]any{"response": c.response("in_progress")}),
		c.ev("response.in_progress", map[string]any{"response": c.response("in_progress")}),
	}
}

func (c *chatToRs) open(kind string, item map[string]any) *rsOpenItem {
	it := &rsOpenItem{index: len(c.items), id: item["id"].(string), kind: kind}
	c.items = append(c.items, it)
	c.output = append(c.output, nil)
	return it
}

// closeItem finishes an item and records its final form.
func (c *chatToRs) closeItem(it *rsOpenItem) []SSEEvent {
	if it == nil || it.closed {
		return nil
	}
	it.closed = true
	text := string(it.text)
	var out []SSEEvent
	var item map[string]any
	switch it.kind {
	case "reasoning":
		part := map[string]any{"type": "summary_text", "text": text}
		out = append(out,
			c.ev("response.reasoning_summary_text.done", map[string]any{"item_id": it.id, "output_index": it.index, "summary_index": 0, "text": text}),
			c.ev("response.reasoning_summary_part.done", map[string]any{"item_id": it.id, "output_index": it.index, "summary_index": 0, "part": part}))
		item = map[string]any{"type": "reasoning", "id": it.id, "summary": []map[string]any{part}}
	case "message":
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		out = append(out,
			c.ev("response.output_text.done", map[string]any{"item_id": it.id, "output_index": it.index, "content_index": 0, "text": text, "logprobs": []any{}}),
			c.ev("response.content_part.done", map[string]any{"item_id": it.id, "output_index": it.index, "content_index": 0, "part": part}))
		item = map[string]any{"type": "message", "id": it.id, "status": "completed", "role": "assistant", "content": []map[string]any{part}}
	default:
		args := text
		if args == "" {
			args = "{}"
		}
		item = rsToolCallItem(c.tools, it.callID, it.name, args, "completed")
		item["id"] = it.id
		if it.kind == "custom_tool_call" {
			input := item["input"].(string)
			out = append(out,
				c.ev("response.custom_tool_call_input.delta", map[string]any{"item_id": it.id, "output_index": it.index, "delta": input}),
				c.ev("response.custom_tool_call_input.done", map[string]any{"item_id": it.id, "output_index": it.index, "input": input}))
		} else {
			out = append(out, c.ev("response.function_call_arguments.done", map[string]any{"item_id": it.id, "output_index": it.index, "arguments": args}))
		}
	}
	c.output[it.index] = item
	return append(out, c.ev("response.output_item.done", map[string]any{"output_index": it.index, "item": item}))
}

func (c *chatToRs) closeCur() []SSEEvent {
	out := c.closeItem(c.cur)
	c.cur = nil
	return out
}

func (c *chatToRs) Process(ev SSEEvent) []SSEEvent {
	if c.done {
		return nil
	}
	if ev.Data == "[DONE]" {
		return c.Finish()
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          *string      `json:"content"`
				ReasoningContent *string      `json:"reasoning_content"`
				Reasoning        *string      `json:"reasoning"`
				ToolCalls        []OAToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *OAUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(ev.Data), &chunk) != nil {
		return nil
	}
	out := c.start()
	if !isNullOrEmpty(chunk.Error) {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(chunk.Error, &e)
		if e.Message == "" {
			e.Message = string(chunk.Error)
		}
		c.done = true
		resp := c.response("failed")
		resp["error"] = map[string]any{"code": "server_error", "message": e.Message}
		return append(out,
			c.ev("error", map[string]any{"code": "server_error", "message": e.Message, "param": nil}),
			c.ev("response.failed", map[string]any{"response": resp}))
	}
	if chunk.Usage != nil {
		if u := chunk.Usage.toUsage(); u.Input > 0 || u.Output > 0 {
			c.usage = u
		}
	}
	for _, ch := range chunk.Choices {
		d := ch.Delta
		reasoning := d.ReasoningContent
		if reasoning == nil || *reasoning == "" {
			reasoning = d.Reasoning
		}
		if reasoning != nil && *reasoning != "" {
			if c.cur == nil || c.cur.kind != "reasoning" {
				out = append(out, c.closeCur()...)
				c.cur = c.open("reasoning", map[string]any{"id": randID("rs_")})
				out = append(out,
					c.ev("response.output_item.added", map[string]any{"output_index": c.cur.index, "item": map[string]any{"type": "reasoning", "id": c.cur.id, "summary": []any{}}}),
					c.ev("response.reasoning_summary_part.added", map[string]any{"item_id": c.cur.id, "output_index": c.cur.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
			}
			c.cur.text = append(c.cur.text, *reasoning...)
			out = append(out, c.ev("response.reasoning_summary_text.delta", map[string]any{"item_id": c.cur.id, "output_index": c.cur.index, "summary_index": 0, "delta": *reasoning}))
		}
		if d.Content != nil && *d.Content != "" {
			if c.cur == nil || c.cur.kind != "message" {
				out = append(out, c.closeCur()...)
				c.cur = c.open("message", map[string]any{"id": randID("msg_")})
				out = append(out,
					c.ev("response.output_item.added", map[string]any{"output_index": c.cur.index, "item": map[string]any{"type": "message", "id": c.cur.id, "status": "in_progress", "role": "assistant", "content": []any{}}}),
					c.ev("response.content_part.added", map[string]any{"item_id": c.cur.id, "output_index": c.cur.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
			}
			c.cur.text = append(c.cur.text, *d.Content...)
			out = append(out, c.ev("response.output_text.delta", map[string]any{"item_id": c.cur.id, "output_index": c.cur.index, "content_index": 0, "delta": *d.Content, "logprobs": []any{}}))
		}
		for i, tc := range d.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			it, seen := c.calls[idx]
			if !seen || (tc.ID != "" && tc.ID != it.callID) {
				out = append(out, c.closeCur()...)
				callID := tc.ID
				if callID == "" {
					callID = randID("call_")
				}
				kind, prefix := "function_call", "fc_"
				if c.tools.Custom[tc.Function.Name] {
					kind, prefix = "custom_tool_call", "ctc_"
				}
				it = c.open(kind, map[string]any{"id": randID(prefix)})
				it.callID, it.name = callID, tc.Function.Name
				c.calls[idx] = it
				added := map[string]any{"type": kind, "id": it.id, "call_id": callID, "name": it.name, "status": "in_progress"}
				if kind == "custom_tool_call" {
					added["input"] = ""
				} else {
					added["arguments"] = ""
				}
				out = append(out, c.ev("response.output_item.added", map[string]any{"output_index": it.index, "item": added}))
			}
			if a := tc.Function.Arguments; a != "" {
				it.text = append(it.text, a...)
				if it.kind == "function_call" {
					out = append(out, c.ev("response.function_call_arguments.delta", map[string]any{"item_id": it.id, "output_index": it.index, "delta": a}))
				}
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			c.finish = *ch.FinishReason
		}
	}
	return out
}

func (c *chatToRs) Finish() []SSEEvent {
	if c.done {
		return nil
	}
	out := c.start()
	c.done = true
	out = append(out, c.closeCur()...)
	idx := make([]int, 0, len(c.calls))
	for k := range c.calls {
		idx = append(idx, k)
	}
	sort.Ints(idx)
	for _, k := range idx {
		out = append(out, c.closeItem(c.calls[k])...)
	}
	status := rsStatusFor(c.finish)
	typ := "response.completed"
	if status == "incomplete" {
		typ = "response.incomplete"
	}
	return append(out, c.ev(typ, map[string]any{"response": c.response(status)}))
}

func (c *chatToRs) Usage() Usage   { return c.usage }
func (c *chatToRs) Complete() bool { return c.done }
func (c *chatToRs) Err() string    { return "" }
