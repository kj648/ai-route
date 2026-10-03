package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RewriteModel replaces the "model" field of a JSON request body, leaving the
// rest of the body untouched. For OpenAI streaming requests it also enables
// stream_options.include_usage so token usage can be recorded; the returned
// bool reports whether the client had asked for usage itself.
func RewriteModel(body []byte, model string, proto string) ([]byte, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, fmt.Errorf("invalid JSON body: %w", err)
	}
	m["model"], _ = json.Marshal(model)
	clientUsage := false
	if proto == ProtoAnthropic {
		if msgs, changed := stripConvertedThinking(m["messages"]); changed {
			m["messages"] = msgs
		}
	}
	if proto == ProtoOpenAI {
		var stream bool
		_ = json.Unmarshal(m["stream"], &stream)
		if stream {
			var so map[string]any
			_ = json.Unmarshal(m["stream_options"], &so)
			if so == nil {
				so = map[string]any{}
			}
			if v, ok := so["include_usage"].(bool); ok && v {
				clientUsage = true
			}
			so["include_usage"] = true
			m["stream_options"], _ = json.Marshal(so)
		}
	}
	out, err := json.Marshal(m)
	return out, clientUsage, err
}

// stripConvertedThinking removes thinking blocks that this gateway synthesized
// from OpenAI reasoning (they have no valid signature and would be rejected
// by an Anthropic upstream). Other blocks are kept byte-for-byte.
func stripConvertedThinking(raw json.RawMessage) (json.RawMessage, bool) {
	if !strings.Contains(string(raw), ConvertedSignature) {
		return raw, false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return raw, false
	}
	for _, msg := range msgs {
		c := msg["content"]
		if isJSONString(c) || !strings.Contains(string(c), ConvertedSignature) {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(c, &blocks) != nil {
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			var probe struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			if json.Unmarshal(b, &probe) == nil && probe.Type == "thinking" && probe.Signature == ConvertedSignature {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			kept = append(kept, json.RawMessage(`{"type":"text","text":"(continued)"}`))
		}
		msg["content"], _ = json.Marshal(kept)
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return raw, false
	}
	return out, true
}

// ===================== OpenAI -> Anthropic =====================

// OpenAIToAnthropicRequest converts a chat completions request into a
// messages request for the given upstream model.
func OpenAIToAnthropicRequest(body []byte, model string, defaultMaxTokens int) ([]byte, error) {
	var req OAChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid chat completions request: %w", err)
	}
	out := map[string]any{"model": model}

	maxTokens := defaultMaxTokens
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		maxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	var system []map[string]any
	var msgs []map[string]any
	appendMsg := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]map[string]any), blocks...)
			return
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if t := oaContentText(m.Content); t != "" {
				system = append(system, map[string]any{"type": "text", "text": t})
			}
		case "user":
			var blocks []map[string]any
			for _, p := range oaContentParts(m.Content) {
				switch p.Type {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
					}
				case "image_url":
					if p.ImageURL != nil {
						blocks = append(blocks, imageURLToBlock(p.ImageURL.URL))
					}
				}
			}
			appendMsg("user", blocks)
		case "assistant":
			var blocks []map[string]any
			if t := oaContentText(m.Content); t != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": parseArgs(tc.Function.Arguments),
				})
			}
			appendMsg("assistant", blocks)
		case "tool", "function":
			appendMsg("user", []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     oaContentText(m.Content),
			}})
		}
	}
	if len(msgs) > 0 && msgs[0]["role"] != "user" {
		msgs = append([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "(continue)"}}}}, msgs...)
	}
	if msgs == nil {
		msgs = []map[string]any{}
	}
	out["messages"] = msgs
	if len(system) > 0 {
		out["system"] = system
	}
	if req.Temperature != nil {
		t := *req.Temperature
		if t > 1 {
			t = 1 // anthropic range is 0..1
		}
		out["temperature"] = t
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if stops := parseStop(req.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	if req.Stream {
		out["stream"] = true
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}
			schema := t.Function.Parameters
			if isNullOrEmpty(schema) {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool := map[string]any{"name": t.Function.Name, "input_schema": schema}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if tc := oaToolChoiceToAnthropic(req.ToolChoice, req.ParallelToolCalls); tc != nil {
				out["tool_choice"] = tc
			}
		}
	}
	if budget := effortBudget(req.ReasoningEffort); budget > 0 {
		if maxTokens <= budget {
			maxTokens = budget + 4096
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		delete(out, "temperature") // thinking requires default temperature
		delete(out, "top_p")
	}
	out["max_tokens"] = maxTokens
	if req.User != "" {
		out["metadata"] = map[string]any{"user_id": req.User}
	}
	return json.Marshal(out)
}

func effortBudget(effort string) int {
	switch effort {
	case "minimal", "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 16384
	}
	return 0
}

func imageURLToBlock(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		// data:image/png;base64,XXXX
		meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
		if ok {
			mediaType := strings.TrimSuffix(meta, ";base64")
			return map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": mediaType, "data": data,
			}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

func parseArgs(args string) any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return map[string]any{}
	}
	if _, ok := v.(map[string]any); !ok {
		return map[string]any{"value": v}
	}
	return v
}

func parseStop(raw json.RawMessage) []string {
	if isNullOrEmpty(raw) {
		return nil
	}
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var arr []string
	_ = json.Unmarshal(raw, &arr)
	return arr
}

func oaToolChoiceToAnthropic(raw json.RawMessage, parallel *bool) map[string]any {
	var tc map[string]any
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		switch s {
		case "none":
			tc = map[string]any{"type": "none"}
		case "required":
			tc = map[string]any{"type": "any"}
		case "auto":
			tc = map[string]any{"type": "auto"}
		}
	} else if !isNullOrEmpty(raw) {
		var obj struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
			tc = map[string]any{"type": "tool", "name": obj.Function.Name}
		}
	}
	if parallel != nil && !*parallel {
		if tc == nil {
			tc = map[string]any{"type": "auto"}
		}
		if tc["type"] != "none" {
			tc["disable_parallel_tool_use"] = true
		}
	}
	return tc
}

// ===================== Anthropic -> OpenAI =====================

// AnthropicToOpenAIRequest converts a messages request into a chat
// completions request for the given upstream model.
func AnthropicToOpenAIRequest(body []byte, model string) ([]byte, error) {
	var req ANRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid messages request: %w", err)
	}
	out := map[string]any{"model": model}
	var msgs []map[string]any

	if !isNullOrEmpty(req.System) {
		var parts []string
		for _, b := range anBlocks(req.System) {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(parts, "\n\n")})
		}
	}

	for _, m := range req.Messages {
		blocks := anBlocks(m.Content)
		switch m.Role {
		case "user":
			var parts []map[string]any
			hasImage := false
			var trailingImages []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				case "image":
					if p := anImageToPart(b.Source); p != nil {
						parts = append(parts, p)
						hasImage = true
					}
				case "tool_result":
					text, images := toolResultContent(b)
					if b.IsError && text != "" {
						text = "[error] " + text
					}
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
					trailingImages = append(trailingImages, images...)
				}
			}
			if len(trailingImages) > 0 {
				parts = append(trailingImages, parts...)
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
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "thinking":
					reasoning.WriteString(b.Thinking)
				case "tool_use":
					args := string(b.Input)
					if isNullOrEmpty(b.Input) {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id": b.ID, "type": "function",
						"function": map[string]any{"name": b.Name, "arguments": args},
					})
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
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	var tools []map[string]any
	for _, t := range req.Tools {
		if isNullOrEmpty(t.InputSchema) {
			continue // server tools (web_search etc.) have no OpenAI equivalent
		}
		fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := req.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "none":
				out["tool_choice"] = "none"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		out["user"] = req.Metadata.UserID
	}
	return json.Marshal(out)
}

func anImageToPart(src *ANSource) map[string]any {
	if src == nil {
		return nil
	}
	var url string
	switch src.Type {
	case "base64":
		url = "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		url = src.URL
	default:
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func toolResultContent(b ANBlock) (string, []map[string]any) {
	if isNullOrEmpty(b.Content) {
		return "", nil
	}
	if isJSONString(b.Content) {
		var s string
		_ = json.Unmarshal(b.Content, &s)
		return s, nil
	}
	var texts []string
	var images []map[string]any
	for _, c := range anBlocks(b.Content) {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image":
			if p := anImageToPart(c.Source); p != nil {
				images = append(images, p)
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

// RequestInfo is the minimal information the gateway needs from a request.
type RequestInfo struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func ParseRequestInfo(body []byte) (RequestInfo, error) {
	var ri RequestInfo
	err := json.Unmarshal(body, &ri)
	return ri, err
}
