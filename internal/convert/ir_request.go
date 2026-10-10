package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------- parsing ----------

// ParseRequest reads a request body in proto into the IR.
func ParseRequest(proto string, body []byte) (*Request, error) {
	switch proto {
	case ProtoAnthropic:
		return parseAnthropicRequest(body)
	case ProtoResponses:
		return parseResponsesRequest(body)
	default:
		return parseChatRequest(body)
	}
}

func parseChatRequest(body []byte) (*Request, error) {
	var req OAChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid chat completions request: %w", err)
	}
	r := &Request{Model: req.Model, Temperature: req.Temperature, TopP: req.TopP, Stop: parseStop(req.Stop),
		Stream: req.Stream, Effort: req.ReasoningEffort, ResponseFormat: req.ResponseFormat, User: req.User,
		ParallelTools: req.ParallelToolCalls}
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		r.MaxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		r.MaxTokens = *req.MaxTokens
	}
	r.StreamUsage = req.Stream && req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			var blocks []Block
			for _, p := range oaContentParts(m.Content) {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, textBlock(p.Text, p.CacheControl))
				}
			}
			r.System = append(r.System, withTrailingCache(blocks, m.CacheControl)...)
		case "user":
			var blocks []Block
			for _, p := range oaContentParts(m.Content) {
				switch p.Type {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, textBlock(p.Text, p.CacheControl))
					}
				case "image_url":
					if p.ImageURL != nil {
						blocks = append(blocks, imageBlock(p.ImageURL.URL, p.ImageURL.Detail, p.CacheControl))
					}
				}
			}
			r.Messages = appendTurn(r.Messages, "user", withTrailingCache(blocks, m.CacheControl))
		case "assistant":
			var blocks []Block
			reasoning := m.ReasoningContent
			if reasoning == "" {
				reasoning = m.Reasoning
			}
			if reasoning != "" {
				blocks = append(blocks, Block{Type: "thinking", Thinking: reasoning, Signature: ConvertedSignature})
			}
			for _, p := range oaContentParts(m.Content) {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, textBlock(p.Text, p.CacheControl))
				}
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
			}
			r.Messages = appendTurn(r.Messages, "assistant", withTrailingCache(blocks, m.CacheControl))
		case "tool", "function":
			// a breakpoint on any part of the message lands on the tool_result
			cc := m.CacheControl
			var content []Block
			for _, p := range oaContentParts(m.Content) {
				if !isNullOrEmpty(p.CacheControl) {
					cc = p.CacheControl
				}
				if p.Type == "text" && p.Text != "" {
					content = append(content, textBlock(p.Text, nil))
				}
			}
			r.Messages = appendTurn(r.Messages, "user", []Block{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content, CacheControl: cc}})
		}
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if isNullOrEmpty(schema) {
			schema = defaultSchema
		}
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema, CacheControl: t.CacheControl})
	}
	r.ToolChoice = parseChatToolChoice(req.ToolChoice)
	return r, nil
}

func parseChatToolChoice(raw json.RawMessage) *ToolChoice {
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		switch s {
		case "none", "auto":
			return &ToolChoice{Mode: s}
		case "required":
			return &ToolChoice{Mode: "any"}
		}
		return nil
	}
	if isNullOrEmpty(raw) {
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Mode string `json:"mode"` // Responses allowed_tools
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch {
	case obj.Function.Name != "":
		return &ToolChoice{Mode: "tool", Name: obj.Function.Name}
	case (obj.Type == "function" || obj.Type == "custom") && obj.Name != "":
		return &ToolChoice{Mode: "tool", Name: obj.Name}
	case obj.Type == "allowed_tools" && obj.Mode == "required":
		return &ToolChoice{Mode: "any"}
	case obj.Type != "":
		return &ToolChoice{Mode: "auto"} // built-in tool choices
	}
	return nil
}

func parseAnthropicRequest(body []byte) (*Request, error) {
	var req ANRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid messages request: %w", err)
	}
	var extra struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	_ = json.Unmarshal(body, &extra)
	r := &Request{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP, TopK: req.TopK,
		Stop: req.StopSequences, Stream: req.Stream, Thinking: req.Thinking}
	if extra.OutputConfig != nil {
		r.OutputEffort = extra.OutputConfig.Effort
	}
	if req.Metadata != nil {
		r.User = req.Metadata.UserID
	}
	for _, b := range anBlocks(req.System) {
		if b.Type == "text" && b.Text != "" {
			r.System = append(r.System, textBlock(b.Text, b.CacheControl))
		}
	}
	for _, m := range req.Messages {
		var blocks []Block
		for _, b := range anBlocks(m.Content) {
			blocks = append(blocks, anBlockToIR(b))
		}
		r.Messages = appendTurn(r.Messages, m.Role, blocks)
	}
	for _, t := range req.Tools {
		tool := Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, CacheControl: t.CacheControl}
		if isNullOrEmpty(t.InputSchema) {
			tool.ServerType = t.Type
			if tool.ServerType == "" {
				tool.ServerType = "server"
			}
		}
		r.Tools = append(r.Tools, tool)
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "any", "none":
			r.ToolChoice = &ToolChoice{Mode: tc.Type}
		case "tool":
			r.ToolChoice = &ToolChoice{Mode: "tool", Name: tc.Name}
		}
		if tc.DisableParallelToolUse {
			r.ParallelTools = ptr(false)
		}
	}
	return r, nil
}

// anBlockToIR converts one Anthropic content block.
func anBlockToIR(b ANBlock) Block {
	out := Block{Type: b.Type, Text: b.Text, Source: b.Source, Title: b.Title, Thinking: b.Thinking, Signature: b.Signature,
		Data: b.Data, ID: b.ID, Name: b.Name, ToolUseID: b.ToolUseID, IsError: b.IsError, CacheControl: b.CacheControl}
	if b.Type == "tool_use" {
		out.Input = b.Input
		if isNullOrEmpty(out.Input) {
			out.Input = json.RawMessage(`{}`)
		}
	}
	if b.Type == "tool_result" {
		if isJSONString(b.Content) {
			var s string
			_ = json.Unmarshal(b.Content, &s)
			if s != "" {
				out.Content = []Block{textBlock(s, nil)}
			}
		} else {
			for _, c := range anBlocks(b.Content) {
				out.Content = append(out.Content, anBlockToIR(c))
			}
		}
	}
	return out
}

func parseResponsesRequest(body []byte) (*Request, error) {
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid responses request: %w", err)
	}
	if req.PreviousResponseID != "" || !isNullOrEmpty(req.Conversation) {
		return nil, errors.New("previous_response_id / conversation need an upstream that serves the Responses API; send the full input with store=false instead")
	}
	r := &Request{Model: req.Model, Temperature: req.Temperature, TopP: req.TopP, Stream: req.Stream, ParallelTools: req.ParallelToolCalls}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		r.MaxTokens = *req.MaxOutputTokens
	}
	if req.Instructions != "" {
		r.System = append(r.System, textBlock(req.Instructions, nil))
	}
	if req.Reasoning != nil && isJSONString(req.Reasoning.Effort) {
		_ = json.Unmarshal(req.Reasoning.Effort, &r.Effort)
	}
	if req.Text != nil && req.Text.Format != nil {
		switch f := req.Text.Format; f.Type {
		case "json_object":
			r.ResponseFormat = &OAResponseFormat{Type: "json_object"}
		case "json_schema":
			rf := &OAResponseFormat{Type: "json_schema"}
			rf.JSONSchema = &oaJSONSchema{Name: f.Name, Description: f.Description, Schema: f.Schema}
			if f.Strict != nil {
				rf.JSONSchema.Strict = *f.Strict
			}
			r.ResponseFormat = rf
		}
	}
	if req.SafetyIdentifier != "" {
		r.User = req.SafetyIdentifier
	} else {
		r.User = req.User
	}

	var pendingThinking []Block // reasoning items precede the assistant message they belong to
	assistant := func(blocks []Block) {
		r.Messages = appendTurn(r.Messages, "assistant", append(pendingThinking, blocks...))
		pendingThinking = nil
	}
	if isJSONString(req.Input) {
		var s string
		_ = json.Unmarshal(req.Input, &s)
		r.Messages = appendTurn(r.Messages, "user", []Block{textBlock(s, nil)})
	} else if !isNullOrEmpty(req.Input) {
		var items []rsItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}
		for _, it := range items {
			switch it.Type {
			case "", "message":
				switch it.Role {
				case "system", "developer":
					if t := rsContentText(it.Content); t != "" {
						r.System = append(r.System, textBlock(t, nil))
					}
				case "assistant":
					if t := rsContentText(it.Content); t != "" {
						assistant([]Block{textBlock(t, nil)})
					} else {
						assistant(nil)
					}
				default:
					r.Messages = appendTurn(r.Messages, "user", rsContentBlocks(it.Content))
				}
			case "function_call":
				assistant([]Block{{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: toolInput(it.Arguments)}})
			case "custom_tool_call":
				args, _ := json.Marshal(map[string]string{"input": it.Input})
				assistant([]Block{{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: args}})
			case "function_call_output", "custom_tool_call_output":
				var content []Block
				if isJSONString(it.Output) {
					var s string
					_ = json.Unmarshal(it.Output, &s)
					if s != "" {
						content = []Block{textBlock(s, nil)}
					}
				} else if !isNullOrEmpty(it.Output) {
					content = rsContentBlocks(it.Output)
				}
				r.Messages = appendTurn(r.Messages, "user", []Block{{Type: "tool_result", ToolUseID: it.CallID, Content: content}})
			case "reasoning":
				var sb strings.Builder
				for _, s := range it.Summary {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(s.Text)
				}
				if sb.Len() > 0 {
					pendingThinking = append(pendingThinking, Block{Type: "thinking", Thinking: sb.String(), Signature: ConvertedSignature})
				}
			}
			// other item types (web_search_call, compaction, item_reference
			// ...) are provider-side state with no equivalent elsewhere
		}
	}
	var addTools func(ts []rsTool, ns string)
	addTools = func(ts []rsTool, ns string) {
		for _, t := range ts {
			switch t.Type {
			case "function":
				params := t.Parameters
				if isNullOrEmpty(params) {
					params = defaultSchema
				}
				r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, InputSchema: params, Strict: t.Strict, Namespace: ns})
			case "custom":
				r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Custom: true, Format: t.Format, Namespace: ns})
			case "namespace":
				addTools(t.Tools, t.Name)
			}
			// built-in tools (web_search, file_search, ...) run on OpenAI's
			// side and cannot be offered to another upstream
		}
	}
	addTools(req.Tools, "")
	r.ToolChoice = parseChatToolChoice(req.ToolChoice)
	return r, nil
}

// rsContentBlocks converts Responses message content (string | parts).
func rsContentBlocks(raw json.RawMessage) []Block {
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		if s == "" {
			return nil
		}
		return []Block{textBlock(s, nil)}
	}
	var parts []rsPart
	_ = json.Unmarshal(raw, &parts)
	var out []Block
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			if p.Text != "" {
				out = append(out, textBlock(p.Text, nil))
			}
		case "refusal":
			if p.Refusal != "" {
				out = append(out, textBlock(p.Refusal, nil))
			}
		case "input_image":
			if p.ImageURL != "" {
				out = append(out, imageBlock(p.ImageURL, p.Detail, nil))
			}
		case "input_file":
			if p.FileData != "" {
				b := imageBlock(p.FileData, "", nil)
				b.Type, b.Title = "document", p.Filename
				out = append(out, b)
			}
		}
	}
	return out
}

// ---------- emitting ----------

// EmitOptions tunes how a request is written for the upstream.
type EmitOptions struct {
	Model string
	// DefaultMaxTokens is used when the client gave none (Anthropic
	// requires max_tokens).
	DefaultMaxTokens int
	// NativeStructured: the Anthropic upstream accepts output_config.format.
	NativeStructured bool
}

// EmitRequest writes the IR as a request body in proto.
func EmitRequest(proto string, r *Request, opts EmitOptions) ([]byte, error) {
	switch proto {
	case ProtoAnthropic:
		return emitAnthropicRequest(r, opts)
	case ProtoResponses:
		return emitResponsesRequest(r, opts)
	default:
		return emitChatRequest(r, opts)
	}
}

// ---- Anthropic ----

func emitAnthropicRequest(r *Request, opts EmitOptions) ([]byte, error) {
	out := map[string]any{"model": opts.Model}
	tr := traitsFor(opts.Model)
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = opts.DefaultMaxTokens
	}

	var system []map[string]any
	for _, b := range r.System {
		system = append(system, anBlockJSON(b))
	}
	if t := responseFormatInstruction(r.ResponseFormat, opts.NativeStructured); t != "" {
		system = append(system, map[string]any{"type": "text", "text": t})
	}
	msgs := []map[string]any{}
	for _, m := range r.Messages {
		var blocks []map[string]any
		for _, b := range m.Content {
			if b.Type == "thinking" && b.Signature == ConvertedSignature {
				continue // synthesized from another protocol: no valid signature
			}
			if j := anBlockJSON(b); j != nil {
				blocks = append(blocks, j)
			}
		}
		if len(blocks) == 0 {
			if m.Role == "assistant" {
				blocks = []map[string]any{{"type": "text", "text": "(continued)"}}
			} else {
				continue
			}
		}
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == m.Role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]map[string]any), blocks...)
			continue
		}
		msgs = append(msgs, map[string]any{"role": m.Role, "content": blocks})
	}
	if len(msgs) > 0 && msgs[0]["role"] != "user" {
		msgs = append([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "(continue)"}}}}, msgs...)
	}
	out["messages"] = msgs
	if len(system) > 0 {
		out["system"] = system
	}
	if r.Temperature != nil {
		t := *r.Temperature
		if t > 1 {
			t = 1 // anthropic range is 0..1
		}
		out["temperature"] = t
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if r.TopK != nil {
		out["top_k"] = *r.TopK
	}
	if len(r.Stop) > 0 {
		out["stop_sequences"] = r.Stop
	}
	if r.Stream {
		out["stream"] = true
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		if t.Custom {
			// a freeform tool becomes a function taking the raw input
			desc := t.Description
			if !isNullOrEmpty(t.Format) {
				desc += "\n\nInput format: " + string(t.Format)
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": strings.TrimSpace(desc), "input_schema": customToolParams})
			continue
		}
		tool := map[string]any{"name": t.Name}
		if t.ServerType != "" {
			tool["type"] = t.ServerType
		} else {
			tool["input_schema"] = t.InputSchema
		}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		if !isNullOrEmpty(t.CacheControl) {
			tool["cache_control"] = t.CacheControl
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := anToolChoiceJSON(r.ToolChoice, r.ParallelTools); tc != nil {
			if tr.noForcedTool && (tc["type"] == "any" || tc["type"] == "tool") {
				tc["type"] = "auto" // forced tool use is rejected by these models
				delete(tc, "name")
			}
			out["tool_choice"] = tc
		}
	}
	outputConfig := map[string]any{}
	effort := r.Effort
	if effort == "" {
		effort = effortFromThinking(r.Thinking, r.OutputEffort)
	}
	if lvl := effortLevel(effort); tr.adaptive && lvl != "" {
		if tr.major == 4 && tr.minor < 7 && lvl == "xhigh" {
			lvl = "high" // xhigh arrived with Opus 4.7
		}
		out["thinking"] = map[string]any{"type": "adaptive"}
		outputConfig["effort"] = lvl
		delete(out, "temperature")
		delete(out, "top_p")
	} else if budget := effortBudget(effort); budget > 0 && !tr.noBudget {
		if r.Thinking != nil && r.Thinking.Type == "enabled" && r.Thinking.BudgetTokens > 0 {
			budget = r.Thinking.BudgetTokens // the client's own budget wins
		}
		if maxTokens <= budget {
			maxTokens = budget + 4096
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		delete(out, "temperature") // thinking requires default temperature
		delete(out, "top_p")
	}
	if tr.noSampling {
		delete(out, "temperature")
		delete(out, "top_p")
	}
	out["max_tokens"] = maxTokens
	if r.User != "" {
		out["metadata"] = map[string]any{"user_id": r.User}
	}
	if schema := nativeOutputSchema(r.ResponseFormat, opts.NativeStructured); schema != nil {
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": schema}
	}
	if len(outputConfig) > 0 {
		out["output_config"] = outputConfig
	}
	return json.Marshal(out)
}

// anBlockJSON writes one block in Anthropic form; nil for blocks the
// protocol has no place for.
func anBlockJSON(b Block) map[string]any {
	var m map[string]any
	switch b.Type {
	case "text":
		m = map[string]any{"type": "text", "text": b.Text}
	case "image", "document":
		if b.Source == nil {
			return nil
		}
		m = map[string]any{"type": b.Type, "source": b.Source}
		if b.Title != "" {
			m["title"] = b.Title
		}
	case "thinking":
		m = map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature}
	case "redacted_thinking":
		m = map[string]any{"type": "redacted_thinking", "data": b.Data}
	case "tool_use":
		input := b.Input
		if isNullOrEmpty(input) {
			input = json.RawMessage(`{}`)
		}
		m = map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": input}
	case "tool_result":
		m = map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID}
		var content []map[string]any
		for _, c := range b.Content {
			if j := anBlockJSON(c); j != nil {
				content = append(content, j)
			}
		}
		switch {
		case len(content) == 1 && content[0]["type"] == "text":
			m["content"] = content[0]["text"]
		case len(content) > 0:
			m["content"] = content
		default:
			m["content"] = ""
		}
		if b.IsError {
			m["is_error"] = true
		}
	default:
		return nil
	}
	if !isNullOrEmpty(b.CacheControl) {
		m["cache_control"] = b.CacheControl
	}
	return m
}

func anToolChoiceJSON(tc *ToolChoice, parallel *bool) map[string]any {
	var out map[string]any
	if tc != nil {
		switch tc.Mode {
		case "none", "any", "auto":
			out = map[string]any{"type": tc.Mode}
		case "tool":
			out = map[string]any{"type": "tool", "name": tc.Name}
		}
	}
	if parallel != nil && !*parallel {
		if out == nil {
			out = map[string]any{"type": "auto"}
		}
		if out["type"] != "none" {
			out["disable_parallel_tool_use"] = true
		}
	}
	return out
}

// ---- Chat Completions ----

func emitChatRequest(r *Request, opts EmitOptions) ([]byte, error) {
	out := map[string]any{"model": opts.Model}
	var msgs []map[string]any
	if t := textOf(r.System, "\n\n"); t != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": t})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			var parts []map[string]any
			var images []map[string]any // from tool results: hoisted to the front
			hasImage := false
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				case "image":
					if p := oaImagePart(b); p != nil {
						parts = append(parts, p)
						hasImage = true
					}
				case "tool_result":
					text := textOf(b.Content, "\n")
					if b.IsError && text != "" {
						text = "[error] " + text
					}
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
					for _, c := range b.Content {
						if c.Type == "image" {
							if p := oaImagePart(c); p != nil {
								images = append(images, p)
							}
						}
					}
				}
			}
			if len(images) > 0 {
				parts = append(images, parts...)
				hasImage = true
			}
			if len(parts) == 0 {
				continue
			}
			if hasImage {
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			} else {
				texts := make([]string, 0, len(parts))
				for _, p := range parts {
					texts = append(texts, p["text"].(string))
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(texts, "\n\n")})
			}
		case "assistant":
			var text, reasoning strings.Builder
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "thinking":
					reasoning.WriteString(b.Thinking)
				case "tool_use":
					toolCalls = append(toolCalls, oaToolCall(b))
				}
			}
			msg := map[string]any{"role": "assistant"}
			if text.Len() > 0 {
				msg["content"] = text.String()
			} else if len(toolCalls) > 0 {
				msg["content"] = nil
			} else {
				msg["content"] = ""
			}
			if reasoning.Len() > 0 {
				msg["reasoning_content"] = reasoning.String()
			}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			msgs = append(msgs, msg)
		}
	}
	if msgs == nil {
		msgs = []map[string]any{}
	}
	out["messages"] = msgs
	if r.MaxTokens > 0 {
		out["max_tokens"] = r.MaxTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if len(r.Stop) > 0 {
		out["stop"] = r.Stop
	}
	if r.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		if t.ServerType != "" {
			continue // server tools (web_search etc.) have no OpenAI equivalent
		}
		fn := map[string]any{"name": t.Name}
		if t.Custom {
			desc := t.Description
			if !isNullOrEmpty(t.Format) {
				desc += "\n\nInput format: " + string(t.Format)
			}
			fn["description"], fn["parameters"] = strings.TrimSpace(desc), customToolParams
		} else {
			fn["parameters"] = t.InputSchema
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Strict != nil && *t.Strict {
				fn["strict"] = true
			}
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := r.ToolChoice; tc != nil {
			switch tc.Mode {
			case "auto", "none":
				out["tool_choice"] = tc.Mode
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
			}
		}
		if r.ParallelTools != nil {
			out["parallel_tool_calls"] = *r.ParallelTools
		}
	}
	// Anthropic thinking is not turned into reasoning_effort: compatible
	// upstreams differ on whether they accept the field
	if r.Effort != "" {
		out["reasoning_effort"] = r.Effort
	}
	if rf := r.ResponseFormat; rf != nil {
		out["response_format"] = rf
	}
	if r.User != "" {
		out["user"] = r.User
	}
	return json.Marshal(out)
}

func oaImagePart(b Block) map[string]any {
	url := sourceURL(b.Source)
	if url == "" {
		return nil
	}
	img := map[string]any{"url": url}
	if b.Detail != "" {
		img["detail"] = b.Detail
	}
	return map[string]any{"type": "image_url", "image_url": img}
}

func oaToolCall(b Block) map[string]any {
	args := string(b.Input)
	if isNullOrEmpty(b.Input) {
		args = "{}"
	}
	return map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": args}}
}

// ---- Responses ----

func emitResponsesRequest(r *Request, opts EmitOptions) ([]byte, error) {
	// the gateway is stateless: store is false and every turn carries the
	// full history
	out := map[string]any{"model": opts.Model, "store": false}
	input := []map[string]any{}
	for _, b := range r.System {
		if b.Text != "" {
			input = append(input, map[string]any{"role": "system", "content": b.Text})
		}
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			var content []map[string]any
			rich := false
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					content = append(content, map[string]any{"type": "input_text", "text": b.Text})
				case "image":
					if p := rsImagePart(b); p != nil {
						content = append(content, p)
						rich = true
					}
				case "document":
					if p := rsFilePart(b); p != nil {
						content = append(content, p)
						rich = true
					}
				case "tool_result":
					input = append(input, rsToolOutput(b))
				}
			}
			if len(content) == 0 {
				continue
			}
			if rich {
				input = append(input, map[string]any{"role": "user", "content": content})
			} else {
				input = append(input, map[string]any{"role": "user", "content": textOf(m.Content, "\n\n")})
			}
		case "assistant":
			var text, reasoning strings.Builder
			var calls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "thinking":
					reasoning.WriteString(b.Thinking)
				case "tool_use":
					calls = append(calls, rsCallItem(r.Tools, b))
				}
			}
			if reasoning.Len() > 0 {
				input = append(input, map[string]any{"type": "reasoning", "id": randID("rs_"),
					"summary": []map[string]any{{"type": "summary_text", "text": reasoning.String()}}})
			}
			if text.Len() > 0 {
				input = append(input, map[string]any{"role": "assistant", "content": text.String()})
			}
			input = append(input, calls...)
		}
	}
	out["input"] = input

	var tools []map[string]any
	for _, t := range r.Tools {
		if t.ServerType != "" {
			continue
		}
		var tool map[string]any
		if t.Custom {
			tool = map[string]any{"type": "custom", "name": t.Name}
			if !isNullOrEmpty(t.Format) {
				tool["format"] = t.Format
			}
		} else {
			// strict false keeps the schema as given: Responses would
			// otherwise try to make it strict
			strict := false
			if t.Strict != nil {
				strict = *t.Strict
			}
			tool = map[string]any{"type": "function", "name": t.Name, "parameters": t.InputSchema, "strict": strict}
		}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := r.ToolChoice; tc != nil {
			switch tc.Mode {
			case "auto", "none":
				out["tool_choice"] = tc.Mode
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "name": tc.Name}
			}
		}
		if r.ParallelTools != nil {
			out["parallel_tool_calls"] = *r.ParallelTools
		}
	}
	if r.MaxTokens > 0 {
		out["max_output_tokens"] = r.MaxTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	effort := r.Effort
	if effort == "" {
		effort = effortFromThinking(r.Thinking, r.OutputEffort)
	}
	if effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}
	if rf := r.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			out["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case "json_schema":
			if js := rf.JSONSchema; js != nil {
				f := map[string]any{"type": "json_schema", "name": js.Name, "schema": js.Schema, "strict": js.Strict}
				if js.Description != "" {
					f["description"] = js.Description
				}
				out["text"] = map[string]any{"format": f}
			}
		}
	}
	if r.Stream {
		out["stream"] = true
	}
	if r.User != "" {
		out["user"] = r.User
	}
	return json.Marshal(out)
}

func rsImagePart(b Block) map[string]any {
	url := sourceURL(b.Source)
	if url == "" {
		return nil
	}
	detail := b.Detail
	if detail == "" {
		detail = "auto"
	}
	return map[string]any{"type": "input_image", "image_url": url, "detail": detail}
}

func rsFilePart(b Block) map[string]any {
	if b.Source == nil || b.Source.Type != "base64" {
		return nil
	}
	name := b.Title
	if name == "" {
		name = "document"
	}
	return map[string]any{"type": "input_file", "filename": name, "file_data": sourceURL(b.Source)}
}

// rsToolOutput writes a tool_result as a function_call_output item; text
// only unless the result carries images.
func rsToolOutput(b Block) map[string]any {
	item := map[string]any{"type": "function_call_output", "call_id": b.ToolUseID}
	var parts []map[string]any
	rich := false
	for _, c := range b.Content {
		switch c.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "input_text", "text": c.Text})
		case "image":
			if p := rsImagePart(c); p != nil {
				parts = append(parts, p)
				rich = true
			}
		}
	}
	if rich {
		item["output"] = parts
	} else {
		text := textOf(b.Content, "\n")
		if b.IsError && text != "" {
			text = "[error] " + text
		}
		item["output"] = text
	}
	return item
}

// rsCallItem writes a tool_use as a function_call, or a custom_tool_call
// when the tool was declared custom.
func rsCallItem(tools []Tool, b Block) map[string]any {
	for _, t := range tools {
		if t.Custom && t.Name == b.Name {
			var in struct {
				Input string `json:"input"`
			}
			if json.Unmarshal(b.Input, &in) != nil {
				in.Input = string(b.Input)
			}
			return map[string]any{"type": "custom_tool_call", "call_id": b.ID, "name": b.Name, "input": in.Input}
		}
	}
	args := string(b.Input)
	if isNullOrEmpty(b.Input) {
		args = "{}"
	}
	return map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args}
}
