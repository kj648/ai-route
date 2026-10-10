package convert

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Streams go through the same intermediate form: a decoder turns the
// upstream's events into Events (Anthropic's event vocabulary, typed), an
// encoder writes them in the client's protocol. Each protocol has one of
// each, and a pipeline joins any two.

// Event is one streaming event in the intermediate form.
type Event struct {
	// Type: message_start | content_block_start | content_block_delta |
	// content_block_stop | message_delta | message_stop | error
	Type  string
	ID    string // message id (message_start)
	Index int    // block index
	Block *Block // content_block_start
	// DeltaType: text_delta | thinking_delta | input_json_delta | signature_delta
	DeltaType  string
	Delta      string
	StopReason string // message_delta
	Usage      Usage  // message_start / message_delta, when HasUsage
	HasUsage   bool
	Error      string // error
}

type decoder interface {
	Decode(ev SSEEvent) []Event
	// Finish ends a stream the upstream left open (after Complete, e.g. an
	// OpenAI stream with finish_reason but no [DONE]).
	Finish() []Event
	Usage() Usage
	Complete() bool
	Err() string
}

type encoder interface {
	Encode(ev Event) []SSEEvent
	Finish() []SSEEvent
}

type pipeline struct {
	dec decoder
	enc encoder
}

func (p *pipeline) encodeAll(evs []Event) []SSEEvent {
	var out []SSEEvent
	for _, ev := range evs {
		out = append(out, p.enc.Encode(ev)...)
	}
	return out
}

func (p *pipeline) Process(ev SSEEvent) []SSEEvent { return p.encodeAll(p.dec.Decode(ev)) }
func (p *pipeline) Finish() []SSEEvent {
	return append(p.encodeAll(p.dec.Finish()), p.enc.Finish()...)
}
func (p *pipeline) Usage() Usage   { return p.dec.Usage() }
func (p *pipeline) Complete() bool { return p.dec.Complete() }
func (p *pipeline) Err() string    { return p.dec.Err() }

func newDecoder(upstream string) decoder {
	switch upstream {
	case ProtoAnthropic:
		return &anDecoder{}
	case ProtoResponses:
		return &rsDecoder{}
	default:
		return &oaDecoder{}
	}
}

func newEncoder(inbound, publicModel string, includeUsage bool, tools ResponsesToolInfo) encoder {
	switch inbound {
	case ProtoAnthropic:
		return &anEncoder{model: publicModel}
	case ProtoResponses:
		return newRsEncoder(publicModel, tools)
	default:
		return &oaEncoder{model: publicModel, includeUsage: includeUsage, created: time.Now().Unix(), toolIndex: map[int]int{}}
	}
}

// ---------- block assembly shared by the chunked decoders ----------

// toolBlock is one tool call being assembled from deltas.
type toolBlock struct {
	id, name string
	block    int // IR block index; -1 while pending
	args     strings.Builder
}

// blocks turns a delta-oriented stream (OpenAI chunks, Responses events)
// into sequential IR blocks. Anthropic blocks are strictly sequential, so
// a tool call that starts while another is streaming is buffered and
// written whole once the live one closes; the same happens before a text
// or thinking block opens.
type blocks struct {
	started, ended bool
	msgID          string
	next           int
	curType        string // "" | text | thinking | tool_use
	curIdx         int
	tools          map[string]*toolBlock // by the protocol's own key
	pending        []string
	sawTool        bool
	finish         string // stop reason decided so far
}

func (s *blocks) start(id string, usage Usage, hasUsage bool) []Event {
	if s.started {
		return nil
	}
	s.started, s.msgID = true, id
	return []Event{{Type: "message_start", ID: id, Usage: usage, HasUsage: hasUsage}}
}

func (s *blocks) closeBlock() []Event {
	if s.curType == "" {
		return nil
	}
	var out []Event
	if s.curType == "thinking" {
		out = append(out, Event{Type: "content_block_delta", Index: s.curIdx, DeltaType: "signature_delta", Delta: ConvertedSignature})
	}
	s.curType = ""
	return append(out, Event{Type: "content_block_stop", Index: s.curIdx})
}

func (s *blocks) open(b Block) []Event {
	out := s.closeBlock()
	s.curType, s.curIdx = b.Type, s.next
	s.next++
	bb := b
	return append(out, Event{Type: "content_block_start", Index: s.curIdx, Block: &bb})
}

func (s *blocks) text(delta string) []Event {
	var out []Event
	if s.curType != "text" {
		out = append(out, s.flushPending()...)
		out = append(out, s.open(Block{Type: "text"})...)
	}
	return append(out, Event{Type: "content_block_delta", Index: s.curIdx, DeltaType: "text_delta", Delta: delta})
}

func (s *blocks) thinking(delta string) []Event {
	var out []Event
	if s.curType != "thinking" {
		out = append(out, s.flushPending()...)
		out = append(out, s.open(Block{Type: "thinking"})...)
	}
	return append(out, Event{Type: "content_block_delta", Index: s.curIdx, DeltaType: "thinking_delta", Delta: delta})
}

// toolStart begins a tool call under key (live, or pending while another
// call is streaming).
func (s *blocks) toolStart(key, id, name string) []Event {
	if s.tools == nil {
		s.tools = map[string]*toolBlock{}
	}
	s.sawTool = true
	tb := &toolBlock{id: id, name: name, block: -1}
	s.tools[key] = tb
	if s.curType == "tool_use" {
		s.pending = append(s.pending, key)
		return nil
	}
	out := s.open(Block{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(`{}`)})
	tb.block = s.curIdx
	return out
}

func (s *blocks) toolArgs(key, args string) []Event {
	tb := s.tools[key]
	if tb == nil || args == "" {
		return nil
	}
	switch {
	case s.curType == "tool_use" && tb.block == s.curIdx:
		return []Event{{Type: "content_block_delta", Index: tb.block, DeltaType: "input_json_delta", Delta: args}}
	case tb.block < 0:
		tb.args.WriteString(args)
	default:
		// arguments after the block was closed (text came in between): the
		// rest goes out as a second block with the same id, the best a
		// sequential protocol can do
		tb.block = -1
		tb.args.WriteString(args)
		s.pending = append(s.pending, key)
	}
	return nil
}

func (s *blocks) flushPending() []Event {
	if len(s.pending) == 0 {
		return nil
	}
	out := s.closeBlock()
	for _, key := range s.pending {
		tb := s.tools[key]
		out = append(out, s.open(Block{Type: "tool_use", ID: tb.id, Name: tb.name, Input: json.RawMessage(`{}`)})...)
		tb.block = s.curIdx
		if tb.args.Len() > 0 {
			out = append(out, Event{Type: "content_block_delta", Index: s.curIdx, DeltaType: "input_json_delta", Delta: tb.args.String()})
			tb.args.Reset()
		}
		out = append(out, s.closeBlock()...)
	}
	s.pending = nil
	return out
}

// end closes everything and ends the message. stop is a stop reason;
// "" picks tool_use or end_turn.
func (s *blocks) end(stop string, usage Usage) []Event {
	if s.ended {
		return nil
	}
	out := s.start("", Usage{}, false)
	s.ended = true
	out = append(out, s.flushPending()...)
	out = append(out, s.closeBlock()...)
	if stop == "" {
		stop = "end_turn"
		if s.sawTool {
			stop = "tool_use"
		}
	}
	return append(out,
		Event{Type: "message_delta", StopReason: stop, Usage: usage, HasUsage: true},
		Event{Type: "message_stop"})
}

// ---------- Anthropic decoder / encoder ----------

type anDecoder struct {
	usage    ANUsage
	finished bool
	err      string
}

func (d *anDecoder) Decode(ev SSEEvent) []Event {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string  `json:"id"`
			Usage ANUsage `json:"usage"`
		} `json:"message"`
		ContentBlock ANBlock `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage ANUsage         `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(ev.Data), &e) != nil {
		return nil
	}
	if e.Type == "" {
		e.Type = ev.Event
	}
	switch e.Type {
	case "message_start":
		mergeANUsage(&d.usage, e.Message.Usage)
		return []Event{{Type: "message_start", ID: e.Message.ID, Usage: d.usage.toUsage(), HasUsage: true}}
	case "content_block_start":
		b := anBlockToIR(e.ContentBlock)
		return []Event{{Type: "content_block_start", Index: e.Index, Block: &b}}
	case "content_block_delta":
		out := Event{Type: "content_block_delta", Index: e.Index, DeltaType: e.Delta.Type}
		switch e.Delta.Type {
		case "text_delta":
			out.Delta = e.Delta.Text
		case "thinking_delta":
			out.Delta = e.Delta.Thinking
		case "input_json_delta":
			out.Delta = e.Delta.PartialJSON
		case "signature_delta":
			out.Delta = e.Delta.Signature
		default:
			return nil
		}
		return []Event{out}
	case "content_block_stop":
		return []Event{{Type: "content_block_stop", Index: e.Index}}
	case "message_delta":
		mergeANUsage(&d.usage, e.Usage)
		return []Event{{Type: "message_delta", StopReason: e.Delta.StopReason, Usage: d.usage.toUsage(), HasUsage: true}}
	case "message_stop":
		d.finished = true
		return []Event{{Type: "message_stop"}}
	case "error":
		msg := "upstream stream error"
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &inner) == nil && inner.Message != "" {
			msg = inner.Message
		} else if !isNullOrEmpty(e.Error) {
			msg = string(e.Error)
		}
		d.err = truncate(msg, 500)
		return []Event{{Type: "error", Error: msg}}
	}
	return nil
}

func (d *anDecoder) Finish() []Event {
	if d.finished || d.err != "" {
		return nil
	}
	d.finished = true
	return []Event{{Type: "message_delta", StopReason: "end_turn", Usage: d.usage.toUsage(), HasUsage: true}, {Type: "message_stop"}}
}
func (d *anDecoder) Usage() Usage   { return d.usage.toUsage() }
func (d *anDecoder) Complete() bool { return d.finished }
func (d *anDecoder) Err() string    { return d.err }

type anEncoder struct {
	model string
	usage Usage
}

func (e *anEncoder) Encode(ev Event) []SSEEvent {
	switch ev.Type {
	case "message_start":
		id := ev.ID
		if !strings.HasPrefix(id, "msg_") {
			id = randID("msg_")
		}
		if ev.HasUsage {
			e.usage = ev.Usage
		}
		u := anUsageFrom(Usage{Input: e.usage.Input, Cached: e.usage.Cached})
		return []SSEEvent{jsonEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": u}})}
	case "content_block_start":
		if ev.Block == nil {
			return nil
		}
		b := *ev.Block
		if b.Type == "tool_use" {
			b.Input = json.RawMessage(`{}`) // the input arrives as deltas
		}
		if b.Type == "thinking" && b.Signature == ConvertedSignature {
			b.Signature = ""
		}
		block := anBlockJSON(b)
		if block == nil {
			return nil
		}
		return []SSEEvent{jsonEvent("content_block_start", map[string]any{"type": "content_block_start", "index": ev.Index, "content_block": block})}
	case "content_block_delta":
		delta := map[string]any{"type": ev.DeltaType}
		switch ev.DeltaType {
		case "text_delta":
			delta["text"] = ev.Delta
		case "thinking_delta":
			delta["thinking"] = ev.Delta
		case "input_json_delta":
			delta["partial_json"] = ev.Delta
		case "signature_delta":
			delta["signature"] = ev.Delta
		default:
			return nil
		}
		return []SSEEvent{jsonEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": ev.Index, "delta": delta})}
	case "content_block_stop":
		return []SSEEvent{jsonEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": ev.Index})}
	case "message_delta":
		if ev.HasUsage {
			e.usage = ev.Usage
		}
		return []SSEEvent{jsonEvent("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": ev.StopReason, "stop_sequence": nil}, "usage": anUsageFrom(e.usage)})}
	case "message_stop":
		return []SSEEvent{jsonEvent("message_stop", map[string]any{"type": "message_stop"})}
	case "error":
		return []SSEEvent{jsonEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Error}})}
	}
	return nil
}

func (e *anEncoder) Finish() []SSEEvent { return nil }

// ---------- OpenAI chunk decoder / encoder ----------

type oaDecoder struct {
	blocks
	usage   Usage
	sawDone bool
	done    bool
	err     string
}

func (d *oaDecoder) Decode(ev SSEEvent) []Event {
	if d.done {
		return nil
	}
	if ev.Data == "[DONE]" {
		d.sawDone = true
		return d.Finish()
	}
	var chunk struct {
		ID      string `json:"id"`
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
	out := d.start(chunk.ID, Usage{}, false)
	if !isNullOrEmpty(chunk.Error) {
		d.done = true
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(chunk.Error, &e)
		if e.Message == "" {
			e.Message = string(chunk.Error)
		}
		d.err = truncate(e.Message, 500)
		return append(out, Event{Type: "error", Error: e.Message})
	}
	if chunk.Usage != nil {
		if u := chunk.Usage.toUsage(); u.Input > 0 || u.Output > 0 {
			d.usage = u
		}
	}
	for _, ch := range chunk.Choices {
		delta := ch.Delta
		reasoning := delta.ReasoningContent
		if reasoning == nil || *reasoning == "" {
			reasoning = delta.Reasoning
		}
		if reasoning != nil && *reasoning != "" {
			out = append(out, d.thinking(*reasoning)...)
		}
		if delta.Content != nil && *delta.Content != "" {
			out = append(out, d.text(*delta.Content)...)
		}
		for i, tc := range delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			key := strconv.Itoa(idx)
			tb := d.tools[key]
			if tb == nil || (tc.ID != "" && tc.ID != tb.id) {
				id := tc.ID
				if id == "" {
					id = randID("call_")
				}
				out = append(out, d.toolStart(key, id, tc.Function.Name)...)
			}
			out = append(out, d.toolArgs(key, tc.Function.Arguments)...)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			d.finish = *ch.FinishReason
		}
	}
	return out
}

func (d *oaDecoder) Finish() []Event {
	if d.done {
		return nil
	}
	d.done = true
	return d.end(chatStopReason(d.finish, d.sawTool), d.usage)
}
func (d *oaDecoder) Usage() Usage   { return d.usage }
func (d *oaDecoder) Complete() bool { return d.sawDone || d.finish != "" }
func (d *oaDecoder) Err() string    { return d.err }

type oaEncoder struct {
	model        string
	includeUsage bool
	id           string
	created      int64
	toolIndex    map[int]int // IR block index -> tool call index
	nextTool     int
	finish       string
	usage        Usage
	done         bool
}

func (e *oaEncoder) chunk(delta map[string]any, finish any) SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return SSEEvent{Data: string(b)}
}

func (e *oaEncoder) Encode(ev Event) []SSEEvent {
	if e.done {
		return nil
	}
	switch ev.Type {
	case "message_start":
		e.id = ev.ID
		if e.id == "" {
			e.id = randID("chatcmpl-")
		}
		if ev.HasUsage {
			e.usage = ev.Usage
		}
		return []SSEEvent{e.chunk(map[string]any{"role": "assistant", "content": ""}, nil)}
	case "content_block_start":
		if ev.Block == nil {
			return nil
		}
		switch ev.Block.Type {
		case "tool_use":
			idx := e.nextTool
			e.nextTool++
			e.toolIndex[ev.Index] = idx
			return []SSEEvent{e.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "id": ev.Block.ID, "type": "function",
				"function": map[string]any{"name": ev.Block.Name, "arguments": ""},
			}}}, nil)}
		case "text":
			if ev.Block.Text != "" {
				return []SSEEvent{e.chunk(map[string]any{"content": ev.Block.Text}, nil)}
			}
		}
	case "content_block_delta":
		switch ev.DeltaType {
		case "text_delta":
			return []SSEEvent{e.chunk(map[string]any{"content": ev.Delta}, nil)}
		case "thinking_delta":
			return []SSEEvent{e.chunk(map[string]any{"reasoning_content": ev.Delta}, nil)}
		case "input_json_delta":
			idx, ok := e.toolIndex[ev.Index]
			if !ok {
				return nil
			}
			return []SSEEvent{e.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "function": map[string]any{"arguments": ev.Delta},
			}}}, nil)}
		}
	case "message_delta":
		e.finish = ev.StopReason
		if ev.HasUsage {
			e.usage = ev.Usage
		}
	case "message_stop":
		return e.Finish()
	case "error":
		e.done = true
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error, "type": "server_error"}})
		return []SSEEvent{{Data: string(b)}, {Data: "[DONE]"}}
	}
	return nil
}

func (e *oaEncoder) Finish() []SSEEvent {
	if e.done {
		return nil
	}
	e.done = true
	var out []SSEEvent
	if e.id == "" {
		out = append(out, e.Encode(Event{Type: "message_start"})...)
		e.done = true
	}
	out = append(out, e.chunk(map[string]any{}, anStopToOpenAI(e.finish)))
	if e.includeUsage {
		b, _ := json.Marshal(map[string]any{
			"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
			"choices": []any{}, "usage": oaUsageFrom(e.usage),
		})
		out = append(out, SSEEvent{Data: string(b)})
	}
	return append(out, SSEEvent{Data: "[DONE]"})
}

// ---------- Responses decoder / encoder ----------

type rsDecoder struct {
	blocks
	usage    Usage
	finished bool
	done     bool
	err      string
	custom   map[string]bool // item id -> custom tool call
}

func (d *rsDecoder) Decode(ev SSEEvent) []Event {
	if d.done {
		return nil
	}
	var e struct {
		Type     string        `json:"type"`
		ItemID   string        `json:"item_id"`
		Delta    string        `json:"delta"`
		Item     *rsOutputItem `json:"item"`
		Response *rsResponse   `json:"response"`
	}
	if json.Unmarshal([]byte(ev.Data), &e) != nil {
		return nil
	}
	if e.Type == "" {
		e.Type = ev.Event
	}
	id := ""
	if e.Response != nil {
		id = e.Response.ID
	}
	out := d.start(id, Usage{}, false)
	switch e.Type {
	case "response.output_text.delta", "response.refusal.delta":
		if e.Delta != "" {
			out = append(out, d.text(e.Delta)...)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if e.Delta != "" {
			out = append(out, d.thinking(e.Delta)...)
		}
	case "response.output_item.added":
		if it := e.Item; it != nil && (it.Type == "function_call" || it.Type == "custom_tool_call") {
			if d.custom == nil {
				d.custom = map[string]bool{}
			}
			d.custom[it.ID] = it.Type == "custom_tool_call"
			out = append(out, d.toolStart(it.ID, it.CallID, it.Name)...)
		}
	case "response.function_call_arguments.delta":
		if e.Delta != "" {
			out = append(out, d.toolArgs(e.ItemID, e.Delta)...)
		}
	case "response.output_item.done":
		if it := e.Item; it != nil {
			if it.Type == "custom_tool_call" {
				// a freeform tool's input arrives whole: wrap it as {"input": ...}
				args, _ := json.Marshal(map[string]string{"input": it.Input})
				out = append(out, d.toolArgs(it.ID, string(args))...)
			}
			if it.Type == "message" || it.Type == "reasoning" {
				out = append(out, d.closeBlock()...)
			}
		}
	case "response.completed", "response.incomplete":
		d.finished = true
		d.done = true
		reason, status := "", "completed"
		if r := e.Response; r != nil {
			d.usage = r.Usage.toUsage()
			status = r.Status
			if r.IncompleteDetails != nil {
				reason = r.IncompleteDetails.Reason
			}
		}
		return append(out, d.end(rsStopReason(status, reason, d.sawTool), d.usage)...)
	case "response.failed", "error":
		msg, _ := isResponsesErrorEvent(ev)
		d.err = msg
		d.done = true
		return append(out, Event{Type: "error", Error: msg})
	}
	return out
}

func (d *rsDecoder) Finish() []Event {
	if d.done {
		return nil
	}
	d.done = true
	return d.end("", d.usage)
}
func (d *rsDecoder) Usage() Usage   { return d.usage }
func (d *rsDecoder) Complete() bool { return d.finished }
func (d *rsDecoder) Err() string    { return d.err }

// rsItem is an output item being written by the Responses encoder.
type rsOpenItem struct {
	index  int
	id     string
	kind   string // reasoning | message | function_call | custom_tool_call
	text   []byte // reasoning summary / message text / arguments
	callID string
	name   string
	closed bool
}

type rsEncoder struct {
	model   string
	tools   ResponsesToolInfo
	id      string
	created int64
	seq     int
	started bool
	items   []*rsOpenItem
	byBlock map[int]*rsOpenItem // IR block index -> item
	output  []map[string]any    // finished items, by output index
	finish  string
	usage   Usage
	done    bool
}

func newRsEncoder(publicModel string, tools ResponsesToolInfo) *rsEncoder {
	return &rsEncoder{model: publicModel, tools: tools, id: randID("resp_"), created: time.Now().Unix(), byBlock: map[int]*rsOpenItem{}}
}

func (c *rsEncoder) ev(typ string, fields map[string]any) SSEEvent {
	fields["type"] = typ
	fields["sequence_number"] = c.seq
	c.seq++
	return jsonEvent(typ, fields)
}

func (c *rsEncoder) response(status string) map[string]any {
	out := make([]map[string]any, 0, len(c.output))
	for _, it := range c.output {
		if it != nil {
			out = append(out, it)
		}
	}
	return rsResponseObject(c.id, c.model, status, c.created, out, c.usage, c.finish)
}

func (c *rsEncoder) start() []SSEEvent {
	if c.started {
		return nil
	}
	c.started = true
	return []SSEEvent{
		c.ev("response.created", map[string]any{"response": c.response("in_progress")}),
		c.ev("response.in_progress", map[string]any{"response": c.response("in_progress")}),
	}
}

func (c *rsEncoder) open(block int, kind, id string) *rsOpenItem {
	it := &rsOpenItem{index: len(c.items), id: id, kind: kind}
	c.items = append(c.items, it)
	c.output = append(c.output, nil)
	c.byBlock[block] = it
	return it
}

// closeItem finishes an item and records its final form.
func (c *rsEncoder) closeItem(it *rsOpenItem) []SSEEvent {
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

func (c *rsEncoder) Encode(ev Event) []SSEEvent {
	if c.done {
		return nil
	}
	out := c.start()
	switch ev.Type {
	case "content_block_start":
		if ev.Block == nil {
			return out
		}
		switch ev.Block.Type {
		case "thinking":
			it := c.open(ev.Index, "reasoning", randID("rs_"))
			out = append(out,
				c.ev("response.output_item.added", map[string]any{"output_index": it.index, "item": map[string]any{"type": "reasoning", "id": it.id, "summary": []any{}}}),
				c.ev("response.reasoning_summary_part.added", map[string]any{"item_id": it.id, "output_index": it.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
		case "text":
			it := c.open(ev.Index, "message", randID("msg_"))
			out = append(out,
				c.ev("response.output_item.added", map[string]any{"output_index": it.index, "item": map[string]any{"type": "message", "id": it.id, "status": "in_progress", "role": "assistant", "content": []any{}}}),
				c.ev("response.content_part.added", map[string]any{"item_id": it.id, "output_index": it.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
			if ev.Block.Text != "" {
				out = append(out, c.Encode(Event{Type: "content_block_delta", Index: ev.Index, DeltaType: "text_delta", Delta: ev.Block.Text})...)
			}
		case "tool_use":
			kind, prefix := "function_call", "fc_"
			if c.tools.Custom[ev.Block.Name] {
				kind, prefix = "custom_tool_call", "ctc_"
			}
			it := c.open(ev.Index, kind, randID(prefix))
			it.callID, it.name = ev.Block.ID, ev.Block.Name
			added := map[string]any{"type": kind, "id": it.id, "call_id": it.callID, "name": it.name, "status": "in_progress"}
			if kind == "custom_tool_call" {
				added["input"] = ""
			} else {
				added["arguments"] = ""
			}
			out = append(out, c.ev("response.output_item.added", map[string]any{"output_index": it.index, "item": added}))
		}
	case "content_block_delta":
		it := c.byBlock[ev.Index]
		if it == nil || it.closed {
			return out
		}
		switch ev.DeltaType {
		case "text_delta":
			it.text = append(it.text, ev.Delta...)
			out = append(out, c.ev("response.output_text.delta", map[string]any{"item_id": it.id, "output_index": it.index, "content_index": 0, "delta": ev.Delta, "logprobs": []any{}}))
		case "thinking_delta":
			it.text = append(it.text, ev.Delta...)
			out = append(out, c.ev("response.reasoning_summary_text.delta", map[string]any{"item_id": it.id, "output_index": it.index, "summary_index": 0, "delta": ev.Delta}))
		case "input_json_delta":
			it.text = append(it.text, ev.Delta...)
			if it.kind == "function_call" {
				out = append(out, c.ev("response.function_call_arguments.delta", map[string]any{"item_id": it.id, "output_index": it.index, "delta": ev.Delta}))
			}
		}
	case "content_block_stop":
		out = append(out, c.closeItem(c.byBlock[ev.Index])...)
	case "message_delta":
		c.finish = anStopToOpenAI(ev.StopReason)
		if ev.HasUsage {
			c.usage = ev.Usage
		}
	case "message_start":
		if ev.HasUsage {
			c.usage = ev.Usage
		}
	case "message_stop":
		return append(out, c.Finish()...)
	case "error":
		c.done = true
		resp := c.response("failed")
		resp["error"] = map[string]any{"code": "server_error", "message": ev.Error}
		return append(out,
			c.ev("error", map[string]any{"code": "server_error", "message": ev.Error, "param": nil}),
			c.ev("response.failed", map[string]any{"response": resp}))
	}
	return out
}

func (c *rsEncoder) Finish() []SSEEvent {
	if c.done {
		return nil
	}
	out := c.start()
	c.done = true
	// close whatever is still open, in output order
	open := make([]*rsOpenItem, 0, len(c.items))
	for _, it := range c.items {
		if !it.closed {
			open = append(open, it)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].index < open[j].index })
	for _, it := range open {
		out = append(out, c.closeItem(it)...)
	}
	status := rsStatusFor(c.finish)
	typ := "response.completed"
	if status == "incomplete" {
		typ = "response.incomplete"
	}
	return append(out, c.ev(typ, map[string]any{"response": c.response(status)}))
}
