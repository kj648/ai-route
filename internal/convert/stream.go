package convert

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// SSEEvent is one server-sent event. Event may be empty (OpenAI style).
type SSEEvent struct {
	Event string
	Data  string
}

// SSEReader parses a text/event-stream body.
type SSEReader struct {
	r *bufio.Reader
	// Raw collects non-SSE lines (e.g. a JSON error body returned instead of
	// a stream) so the caller can report them.
	Raw strings.Builder
}

func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event, or io.EOF when the stream ends.
func (s *SSEReader) Next() (SSEEvent, error) {
	var ev SSEEvent
	var data []string
	hasData := false
	for {
		line, err := s.r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if hasData {
					ev.Data = strings.Join(data, "\n")
					return ev, nil
				}
				ev = SSEEvent{}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				hasData = true
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			default:
				if s.Raw.Len() < 4096 {
					s.Raw.WriteString(line)
					s.Raw.WriteString("\n")
				}
			}
		}
		if err != nil {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return SSEEvent{}, err
		}
	}
}

// WriteSSE serializes an event.
func WriteSSE(w io.Writer, ev SSEEvent) error {
	var buf bytes.Buffer
	if ev.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(ev.Event)
		buf.WriteString("\n")
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	_, err := w.Write(buf.Bytes())
	return err
}

// StreamErrorMessage reports whether an upstream event is an error event.
func StreamErrorMessage(ev SSEEvent, proto string) (string, bool) {
	if ev.Data == "[DONE]" {
		return "", false
	}
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(ev.Data), &probe) != nil {
		return "", false
	}
	if proto == ProtoAnthropic && (ev.Event == "error" || probe.Type == "error") {
		return truncate(ev.Data, 500), true
	}
	if !isNullOrEmpty(probe.Error) {
		return truncate(string(probe.Error), 500), true
	}
	return "", false
}

// StreamConverter transforms upstream events into client events.
type StreamConverter interface {
	Process(ev SSEEvent) []SSEEvent
	Finish() []SSEEvent
	Usage() Usage
	// Complete reports whether the upstream sent a terminal event; a stream
	// that hits EOF without one was cut off.
	Complete() bool
	// Err returns an error reported by the upstream inside the stream.
	Err() string
}

// ConvertedSignature marks thinking blocks synthesized from OpenAI
// reasoning_content. They carry no real signature, so they are stripped
// before a request is forwarded to an Anthropic upstream.
const ConvertedSignature = "ai-route-converted"

func jsonEvent(name string, v any) SSEEvent {
	b, _ := json.Marshal(v)
	return SSEEvent{Event: name, Data: string(b)}
}

// ---------- pass-through: OpenAI ----------

type oaPass struct {
	clientUsage bool
	usage       Usage
	done        bool
	finished    bool
	err         string
}

// NewOpenAIPassthrough forwards an OpenAI stream as is, recording usage.
// When the client did not request usage, injected usage-only chunks are dropped.
func NewOpenAIPassthrough(clientUsage bool) StreamConverter { return &oaPass{clientUsage: clientUsage} }

func (p *oaPass) Process(ev SSEEvent) []SSEEvent {
	if ev.Data == "[DONE]" {
		p.done = true
		return []SSEEvent{{Data: "[DONE]"}}
	}
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *OAUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal([]byte(ev.Data), &chunk)
	if !isNullOrEmpty(chunk.Error) {
		p.err = truncate(string(chunk.Error), 500)
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			p.finished = true
		}
	}
	if chunk.Usage != nil {
		if u := chunk.Usage.toUsage(); u.Input > 0 || u.Output > 0 {
			p.usage = u
		}
		if !p.clientUsage && len(chunk.Choices) == 0 {
			return nil
		}
	}
	return []SSEEvent{{Data: ev.Data}}
}

func (p *oaPass) Finish() []SSEEvent {
	if p.done {
		return nil
	}
	p.done = true
	return []SSEEvent{{Data: "[DONE]"}}
}

func (p *oaPass) Usage() Usage   { return p.usage }
func (p *oaPass) Complete() bool { return p.done || p.finished }
func (p *oaPass) Err() string    { return p.err }

// ---------- pass-through: Anthropic ----------

type anPass struct {
	usage    ANUsage
	finished bool
	err      string
}

// NewAnthropicPassthrough forwards an Anthropic stream as is, recording usage.
func NewAnthropicPassthrough() StreamConverter { return &anPass{} }

func mergeANUsage(dst *ANUsage, src ANUsage) {
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
}

func (p *anPass) Process(ev SSEEvent) []SSEEvent {
	var d struct {
		Type    string `json:"type"`
		Message struct {
			Usage ANUsage `json:"usage"`
		} `json:"message"`
		Usage ANUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(ev.Data), &d) == nil {
		switch d.Type {
		case "message_start":
			mergeANUsage(&p.usage, d.Message.Usage)
		case "message_delta":
			mergeANUsage(&p.usage, d.Usage)
		case "message_stop":
			p.finished = true
		}
	}
	name := ev.Event
	if name == "" {
		name = d.Type
	}
	if name == "error" || d.Type == "error" {
		p.err = truncate(ev.Data, 500)
	}
	return []SSEEvent{{Event: name, Data: ev.Data}}
}

func (p *anPass) Finish() []SSEEvent { return nil }
func (p *anPass) Usage() Usage       { return p.usage.toUsage() }
func (p *anPass) Complete() bool     { return p.finished }
func (p *anPass) Err() string        { return p.err }

// ---------- Anthropic upstream -> OpenAI client ----------

type anToOA struct {
	model        string
	includeUsage bool
	id           string
	created      int64
	toolIndex    map[int]int
	nextTool     int
	usage        ANUsage
	finish       string
	done         bool
	finished     bool
	err          string
}

// NewAnthropicToOpenAIStream converts an Anthropic stream into OpenAI chunks.
func NewAnthropicToOpenAIStream(publicModel string, includeUsage bool) StreamConverter {
	return &anToOA{
		model: publicModel, includeUsage: includeUsage,
		id: randID("chatcmpl-"), created: time.Now().Unix(),
		toolIndex: map[int]int{},
	}
}

func (c *anToOA) chunk(delta map[string]any, finish any) SSEEvent {
	b, _ := json.Marshal(map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return SSEEvent{Data: string(b)}
}

func (c *anToOA) Process(ev SSEEvent) []SSEEvent {
	if c.done {
		return nil
	}
	var d struct {
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
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage ANUsage         `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(ev.Data), &d) != nil {
		return nil
	}
	if d.Type == "" {
		d.Type = ev.Event
	}
	switch d.Type {
	case "message_start":
		mergeANUsage(&c.usage, d.Message.Usage)
		return []SSEEvent{c.chunk(map[string]any{"role": "assistant", "content": ""}, nil)}
	case "content_block_start":
		switch d.ContentBlock.Type {
		case "tool_use":
			idx := c.nextTool
			c.nextTool++
			c.toolIndex[d.Index] = idx
			return []SSEEvent{c.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "id": d.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": d.ContentBlock.Name, "arguments": ""},
			}}}, nil)}
		case "text":
			if d.ContentBlock.Text != "" {
				return []SSEEvent{c.chunk(map[string]any{"content": d.ContentBlock.Text}, nil)}
			}
		}
	case "content_block_delta":
		switch d.Delta.Type {
		case "text_delta":
			return []SSEEvent{c.chunk(map[string]any{"content": d.Delta.Text}, nil)}
		case "thinking_delta":
			return []SSEEvent{c.chunk(map[string]any{"reasoning_content": d.Delta.Thinking}, nil)}
		case "input_json_delta":
			idx, ok := c.toolIndex[d.Index]
			if !ok {
				return nil
			}
			return []SSEEvent{c.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "function": map[string]any{"arguments": d.Delta.PartialJSON},
			}}}, nil)}
		}
	case "message_delta":
		if d.Delta.StopReason != "" {
			c.finish = d.Delta.StopReason
		}
		mergeANUsage(&c.usage, d.Usage)
	case "message_stop":
		c.finished = true
		return c.Finish()
	case "error":
		c.done = true
		errObj := d.Error
		if isNullOrEmpty(errObj) {
			errObj = json.RawMessage(`{"message":"upstream stream error"}`)
		}
		c.err = truncate(string(errObj), 500)
		b, _ := json.Marshal(map[string]any{"error": errObj})
		return []SSEEvent{{Data: string(b)}, {Data: "[DONE]"}}
	}
	return nil
}

func (c *anToOA) Finish() []SSEEvent {
	if c.done {
		return nil
	}
	c.done = true
	out := []SSEEvent{c.chunk(map[string]any{}, anStopToOpenAI(c.finish))}
	if c.includeUsage {
		b, _ := json.Marshal(map[string]any{
			"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{}, "usage": oaUsageFrom(c.usage.toUsage()),
		})
		out = append(out, SSEEvent{Data: string(b)})
	}
	return append(out, SSEEvent{Data: "[DONE]"})
}

func (c *anToOA) Usage() Usage   { return c.usage.toUsage() }
func (c *anToOA) Complete() bool { return c.finished || c.finish != "" }
func (c *anToOA) Err() string    { return c.err }

// ---------- OpenAI upstream -> Anthropic client ----------

type oaToAN struct {
	model     string
	msgID     string
	started   bool
	done      bool
	nextBlock int
	curType   string // "", "text", "thinking", "tool_use"
	curIdx    int
	tools     map[int]*toolBlock // by OpenAI tool index
	finish    string
	usage     Usage
	sawDone   bool
	err       string
}

type toolBlock struct {
	id    string
	block int
}

// NewOpenAIToAnthropicStream converts OpenAI chunks into an Anthropic stream.
func NewOpenAIToAnthropicStream(publicModel string) StreamConverter {
	return &oaToAN{model: publicModel, msgID: randID("msg_"), tools: map[int]*toolBlock{}}
}

func (c *oaToAN) start() []SSEEvent {
	if c.started {
		return nil
	}
	c.started = true
	return []SSEEvent{jsonEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": c.msgID, "type": "message", "role": "assistant", "model": c.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})}
}

func (c *oaToAN) closeBlock() []SSEEvent {
	if c.curType == "" {
		return nil
	}
	var out []SSEEvent
	if c.curType == "thinking" {
		out = append(out, c.delta(c.curIdx, map[string]any{"type": "signature_delta", "signature": ConvertedSignature}))
	}
	c.curType = ""
	return append(out, jsonEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.curIdx}))
}

func (c *oaToAN) openBlock(typ string, block map[string]any) []SSEEvent {
	out := c.closeBlock()
	c.curType = typ
	c.curIdx = c.nextBlock
	c.nextBlock++
	return append(out, jsonEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": c.curIdx, "content_block": block,
	}))
}

func (c *oaToAN) delta(idx int, delta map[string]any) SSEEvent {
	return jsonEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": delta})
}

func (c *oaToAN) Process(ev SSEEvent) []SSEEvent {
	if c.done {
		return nil
	}
	if ev.Data == "[DONE]" {
		c.sawDone = true
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
		c.done = true
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(chunk.Error, &e)
		if e.Message == "" {
			e.Message = string(chunk.Error)
		}
		c.err = truncate(e.Message, 500)
		return append(out, jsonEvent("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": e.Message},
		}))
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
			if c.curType != "thinking" {
				out = append(out, c.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})...)
			}
			out = append(out, c.delta(c.curIdx, map[string]any{"type": "thinking_delta", "thinking": *reasoning}))
		}
		if d.Content != nil && *d.Content != "" {
			if c.curType != "text" {
				out = append(out, c.openBlock("text", map[string]any{"type": "text", "text": ""})...)
			}
			out = append(out, c.delta(c.curIdx, map[string]any{"type": "text_delta", "text": *d.Content}))
		}
		for i, tc := range d.ToolCalls {
			oaIdx := i
			if tc.Index != nil {
				oaIdx = *tc.Index
			}
			tb, seen := c.tools[oaIdx]
			if !seen || (tc.ID != "" && tc.ID != tb.id) {
				id := tc.ID
				if id == "" {
					id = randID("toolu_")
				}
				out = append(out, c.openBlock("tool_use", map[string]any{
					"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{},
				})...)
				tb = &toolBlock{id: id, block: c.curIdx}
				c.tools[oaIdx] = tb
			}
			if tc.Function.Arguments != "" {
				out = append(out, c.delta(tb.block, map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}))
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			c.finish = *ch.FinishReason
		}
	}
	return out
}

func (c *oaToAN) Finish() []SSEEvent {
	if c.done {
		return nil
	}
	out := c.start()
	c.done = true
	out = append(out, c.closeBlock()...)
	finish := c.finish
	if len(c.tools) > 0 && (finish == "" || finish == "stop") {
		finish = "tool_calls"
	}
	usage := anUsageFrom(c.usage)
	out = append(out, jsonEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": oaFinishToAnthropic(finish), "stop_sequence": nil},
		"usage": usage,
	}))
	return append(out, jsonEvent("message_stop", map[string]any{"type": "message_stop"}))
}

func (c *oaToAN) Usage() Usage   { return c.usage }
func (c *oaToAN) Complete() bool { return c.sawDone || c.finish != "" }
func (c *oaToAN) Err() string    { return c.err }

// ClientWantsUsage reports whether an OpenAI request asked for stream usage.
func ClientWantsUsage(body []byte) bool {
	var r struct {
		StreamOptions *OAStreamOptions `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &r)
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}
