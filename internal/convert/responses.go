package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OpenAI Responses API <-> Chat Completions. Field names follow the
// openai-python types (src/openai/types/responses) and the OpenAPI spec.
//
// Custom (freeform) tools such as Codex's apply_patch have no Chat
// equivalent: they become functions with a single string parameter "input",
// and calls of them are turned back into custom_tool_call items using the
// client's original request (see ResponsesToolInfo).

// ---------- request types ----------

type rsRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions"`
	Tools             []rsTool        `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Reasoning         *struct {
		Effort json.RawMessage `json:"effort"`
	} `json:"reasoning"`
	Text *struct {
		Format *rsFormat `json:"format"`
	} `json:"text"`
	PreviousResponseID string          `json:"previous_response_id"`
	Conversation       json.RawMessage `json:"conversation"`
	User               string          `json:"user"`
	SafetyIdentifier   string          `json:"safety_identifier"`
}

type rsFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type rsTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
	Format      json.RawMessage `json:"format"` // custom tools
	Tools       []rsTool        `json:"tools"`  // namespace
}

type rsItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type rsPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
}

// ResponsesToolInfo describes the tools of a Responses request that need
// special handling when a Chat answer is turned back into Responses items.
type ResponsesToolInfo struct {
	Custom    map[string]bool   // freeform tools (custom_tool_call)
	Namespace map[string]string // tool name -> namespace
}

// ParseResponsesTools reads the tools of a Responses request body.
func ParseResponsesTools(body []byte) ResponsesToolInfo {
	info := ResponsesToolInfo{Custom: map[string]bool{}, Namespace: map[string]string{}}
	var req struct {
		Tools []rsTool `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return info
	}
	var walk func(ts []rsTool, ns string)
	walk = func(ts []rsTool, ns string) {
		for _, t := range ts {
			switch t.Type {
			case "custom":
				info.Custom[t.Name] = true
			case "namespace":
				walk(t.Tools, t.Name)
				continue
			}
			if ns != "" {
				info.Namespace[t.Name] = ns
			}
		}
	}
	walk(req.Tools, "")
	return info
}

// customToolParams is the Chat stand-in for a freeform tool's input.
var customToolParams = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"The raw tool input, exactly as the tool's format describes."}},"required":["input"]}`)

// ---------- Responses request -> Chat request ----------

// ResponsesToChatRequest converts a Responses request into a Chat
// Completions request for model.
func ResponsesToChatRequest(body []byte, model string) ([]byte, error) {
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid responses request: %w", err)
	}
	if req.PreviousResponseID != "" || !isNullOrEmpty(req.Conversation) {
		return nil, errors.New("previous_response_id / conversation need an upstream that serves the Responses API; send the full input with store=false instead")
	}
	out := map[string]any{"model": model}
	var msgs []map[string]any
	if req.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}

	var reasoning strings.Builder // summaries waiting for the next assistant message
	// lastAssistant returns the assistant message tool calls can be added to
	lastAssistant := func() map[string]any {
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == "assistant" {
			return msgs[n-1]
		}
		m := map[string]any{"role": "assistant", "content": nil}
		msgs = append(msgs, m)
		return m
	}
	flushReasoning := func(m map[string]any) {
		if reasoning.Len() > 0 {
			m["reasoning_content"] = reasoning.String()
			reasoning.Reset()
		}
	}
	addToolCall := func(callID, name, args string) {
		m := lastAssistant()
		flushReasoning(m)
		calls, _ := m["tool_calls"].([]map[string]any)
		m["tool_calls"] = append(calls, map[string]any{
			"id": callID, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		})
	}

	if isJSONString(req.Input) {
		var s string
		_ = json.Unmarshal(req.Input, &s)
		msgs = append(msgs, map[string]any{"role": "user", "content": s})
	} else if !isNullOrEmpty(req.Input) {
		var items []rsItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}
		for _, it := range items {
			switch it.Type {
			case "", "message":
				role := it.Role
				if role == "developer" {
					role = "system" // not every Chat upstream knows "developer"
				}
				if role == "assistant" {
					m := map[string]any{"role": "assistant", "content": rsContentText(it.Content)}
					flushReasoning(m)
					msgs = append(msgs, m)
					continue
				}
				msgs = append(msgs, map[string]any{"role": role, "content": rsContentToChat(it.Content, role == "user")})
			case "function_call":
				addToolCall(it.CallID, it.Name, it.Arguments)
			case "custom_tool_call":
				args, _ := json.Marshal(map[string]string{"input": it.Input})
				addToolCall(it.CallID, it.Name, string(args))
			case "function_call_output", "custom_tool_call_output":
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": it.CallID, "content": rsOutputText(it.Output)})
			case "reasoning":
				for _, s := range it.Summary {
					if reasoning.Len() > 0 {
						reasoning.WriteString("\n")
					}
					reasoning.WriteString(s.Text)
				}
			}
			// other item types (web_search_call, compaction, item_reference
			// ...) are provider-side state with no Chat equivalent
		}
	}
	if msgs == nil {
		msgs = []map[string]any{}
	}
	out["messages"] = msgs

	var tools []map[string]any
	var addTools func(ts []rsTool)
	addTools = func(ts []rsTool) {
		for _, t := range ts {
			switch t.Type {
			case "function":
				params := t.Parameters
				if isNullOrEmpty(params) {
					params = json.RawMessage(`{"type":"object","properties":{}}`)
				}
				fn := map[string]any{"name": t.Name, "parameters": params}
				if t.Description != "" {
					fn["description"] = t.Description
				}
				if t.Strict != nil && *t.Strict {
					fn["strict"] = true
				}
				tools = append(tools, map[string]any{"type": "function", "function": fn})
			case "custom":
				desc := t.Description
				if !isNullOrEmpty(t.Format) {
					desc += "\n\nInput format: " + string(t.Format)
				}
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
					"name": t.Name, "description": strings.TrimSpace(desc), "parameters": customToolParams,
				}})
			case "namespace":
				addTools(t.Tools)
			}
			// built-in tools (web_search, file_search, ...) run on OpenAI's
			// side and cannot be offered to another upstream
		}
	}
	addTools(req.Tools)
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := rsToolChoiceToChat(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
		if req.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.Reasoning != nil && isJSONString(req.Reasoning.Effort) {
		var e string
		_ = json.Unmarshal(req.Reasoning.Effort, &e)
		if e != "" {
			out["reasoning_effort"] = e
		}
	}
	if req.Text != nil && req.Text.Format != nil {
		switch f := req.Text.Format; f.Type {
		case "json_object":
			out["response_format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			js := map[string]any{"name": f.Name, "schema": f.Schema}
			if f.Description != "" {
				js["description"] = f.Description
			}
			if f.Strict != nil {
				js["strict"] = *f.Strict
			}
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": js}
		}
	}
	if req.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if u := req.SafetyIdentifier; u != "" {
		out["user"] = u
	} else if req.User != "" {
		out["user"] = req.User
	}
	return json.Marshal(out)
}

// rsContentToChat converts message content (string | parts) to Chat content.
func rsContentToChat(raw json.RawMessage, allowImages bool) any {
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []rsPart
	_ = json.Unmarshal(raw, &parts)
	var out []map[string]any
	hasImage := false
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "refusal":
			out = append(out, map[string]any{"type": "text", "text": p.Refusal})
		case "input_image":
			if allowImages && p.ImageURL != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
				hasImage = true
			}
		}
	}
	if !hasImage {
		return rsContentText(raw)
	}
	return out
}

// rsContentText joins the text of message content.
func rsContentText(raw json.RawMessage) string {
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []rsPart
	_ = json.Unmarshal(raw, &parts)
	var sb strings.Builder
	for _, p := range parts {
		t := p.Text
		if p.Type == "refusal" {
			t = p.Refusal
		}
		if t == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(t)
	}
	return sb.String()
}

// rsOutputText flattens a tool output (string | content parts).
func rsOutputText(raw json.RawMessage) string {
	if isNullOrEmpty(raw) {
		return ""
	}
	return rsContentText(raw)
}

func rsToolChoiceToChat(raw json.RawMessage) any {
	if isNullOrEmpty(raw) {
		return nil
	}
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s // none | auto | required
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "function", "custom":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	case "allowed_tools":
		if tc.Mode == "required" {
			return "required"
		}
		return "auto"
	}
	return "auto" // built-in tool choices
}

// ---------- Chat request -> Responses request ----------

// ChatToResponsesRequest converts a Chat Completions request into a
// Responses request for model. The gateway is stateless: store is false and
// every turn carries the full history.
func ChatToResponsesRequest(body []byte, model string) ([]byte, error) {
	var req OAChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid chat completions request: %w", err)
	}
	var raw struct {
		ResponseFormat *struct {
			Type       string    `json:"type"`
			JSONSchema *rsFormat `json:"json_schema"`
		} `json:"response_format"`
	}
	_ = json.Unmarshal(body, &raw)

	out := map[string]any{"model": model, "store": false}
	var input []map[string]any
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if t := oaContentText(m.Content); t != "" {
				input = append(input, map[string]any{"role": m.Role, "content": t})
			}
		case "user":
			parts := oaContentParts(m.Content)
			var content []map[string]any
			hasImage := false
			for _, p := range parts {
				switch p.Type {
				case "text":
					content = append(content, map[string]any{"type": "input_text", "text": p.Text})
				case "image_url":
					if p.ImageURL != nil {
						detail := p.ImageURL.Detail
						if detail == "" {
							detail = "auto"
						}
						content = append(content, map[string]any{"type": "input_image", "image_url": p.ImageURL.URL, "detail": detail})
						hasImage = true
					}
				}
			}
			if hasImage {
				input = append(input, map[string]any{"role": "user", "content": content})
			} else {
				input = append(input, map[string]any{"role": "user", "content": oaContentText(m.Content)})
			}
		case "assistant":
			if t := oaContentText(m.Content); t != "" {
				input = append(input, map[string]any{"role": "assistant", "content": t})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": args})
			}
		case "tool", "function":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": oaContentText(m.Content)})
		}
	}
	if input == nil {
		input = []map[string]any{}
	}
	out["input"] = input

	var tools []map[string]any
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		params := t.Function.Parameters
		if isNullOrEmpty(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		// strict false keeps Chat semantics: Responses would otherwise try
		// to make the schema strict
		tool := map[string]any{"type": "function", "name": t.Function.Name, "parameters": params, "strict": false}
		if t.Function.Description != "" {
			tool["description"] = t.Function.Description
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := chatToolChoiceToResponses(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
		if req.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	switch {
	case req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0:
		out["max_output_tokens"] = *req.MaxCompletionTokens
	case req.MaxTokens != nil && *req.MaxTokens > 0:
		out["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		out["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	if rf := raw.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			out["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case "json_schema":
			if js := rf.JSONSchema; js != nil {
				f := map[string]any{"type": "json_schema", "name": js.Name, "schema": js.Schema}
				if js.Description != "" {
					f["description"] = js.Description
				}
				if js.Strict != nil {
					f["strict"] = *js.Strict
				}
				out["text"] = map[string]any{"format": f}
			}
		}
	}
	if req.Stream {
		out["stream"] = true
	}
	if req.User != "" {
		out["user"] = req.User
	}
	return json.Marshal(out)
}

func chatToolChoiceToResponses(raw json.RawMessage) any {
	if isNullOrEmpty(raw) {
		return nil
	}
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var tc struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tc) == nil && tc.Function.Name != "" {
		return map[string]any{"type": "function", "name": tc.Function.Name}
	}
	return nil
}

// ---------- Responses response -> Chat response ----------

type rsUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens int64    `json:"output_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	Cost         *float64 `json:"cost,omitempty"` // OpenRouter
}

func (u *rsUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{Input: u.InputTokens, Output: u.OutputTokens, Cost: u.Cost}
	if u.InputTokensDetails != nil {
		out.Cached = u.InputTokensDetails.CachedTokens
	}
	return out
}

type rsOutputItem struct {
	Type      string   `json:"type"`
	ID        string   `json:"id"`
	CallID    string   `json:"call_id"`
	Name      string   `json:"name"`
	Arguments string   `json:"arguments"`
	Input     string   `json:"input"`
	Content   []rsPart `json:"content"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type rsResponse struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	Output            []rsOutputItem `json:"output"`
	Usage             *rsUsage       `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func rsFinishReason(status, reason string, toolCalls bool) string {
	switch {
	case status == "incomplete" && reason == "max_output_tokens":
		return "length"
	case status == "incomplete" && reason == "content_filter":
		return "content_filter"
	case toolCalls:
		return "tool_calls"
	}
	return "stop"
}

// ResponsesToChatResponse converts a non-stream Responses body into a Chat
// Completions response.
func ResponsesToChatResponse(body []byte, publicModel string) ([]byte, Usage, error) {
	var r rsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, Usage{}, err
	}
	var text, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, it := range r.Output {
		switch it.Type {
		case "message":
			for _, p := range it.Content {
				if p.Type == "output_text" {
					text.WriteString(p.Text)
				} else if p.Type == "refusal" {
					text.WriteString(p.Refusal)
				}
			}
		case "reasoning":
			for _, s := range it.Summary {
				reasoning.WriteString(s.Text)
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{"id": it.CallID, "type": "function",
				"function": map[string]any{"name": it.Name, "arguments": it.Arguments}})
		case "custom_tool_call":
			args, _ := json.Marshal(map[string]string{"input": it.Input})
			toolCalls = append(toolCalls, map[string]any{"id": it.CallID, "type": "function",
				"function": map[string]any{"name": it.Name, "arguments": string(args)}})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	reason := ""
	if r.IncompleteDetails != nil {
		reason = r.IncompleteDetails.Reason
	}
	usage := r.Usage.toUsage()
	out, err := json.Marshal(map[string]any{
		"id": randID("chatcmpl-"), "object": "chat.completion", "created": time.Now().Unix(), "model": publicModel,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": rsFinishReason(r.Status, reason, len(toolCalls) > 0)}},
		"usage":   oaUsageFrom(usage),
	})
	return out, usage, err
}

// ---------- Chat response -> Responses response ----------

func rsUsageFrom(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":          u.Input,
		"input_tokens_details":  map[string]any{"cached_tokens": u.Cached},
		"output_tokens":         u.Output,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          u.Input + u.Output,
	}
}

// rsToolCallItem builds a function_call (or custom_tool_call) output item.
func rsToolCallItem(tools ResponsesToolInfo, callID, name, args, status string) map[string]any {
	if tools.Custom[name] {
		var in struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			in.Input = args // the model ignored the wrapper
		}
		return map[string]any{"type": "custom_tool_call", "id": randID("ctc_"), "call_id": callID, "name": name, "input": in.Input, "status": status}
	}
	item := map[string]any{"type": "function_call", "id": randID("fc_"), "call_id": callID, "name": name, "arguments": args, "status": status}
	if ns := tools.Namespace[name]; ns != "" {
		item["namespace"] = ns
	}
	return item
}

func rsResponseObject(id, model, status string, created int64, output []map[string]any, usage Usage, finish string) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	r := map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status, "model": model,
		"output": output, "usage": rsUsageFrom(usage), "error": nil, "incomplete_details": nil,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}, "store": false,
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if finish == "content_filter" {
			reason = "content_filter"
		}
		r["incomplete_details"] = map[string]any{"reason": reason}
	}
	return r
}

func rsStatusFor(finish string) string {
	if finish == "length" || finish == "content_filter" {
		return "incomplete"
	}
	return "completed"
}

// ChatToResponsesResponse converts a non-stream Chat Completions body into a
// Responses response. tools come from the client's Responses request.
func ChatToResponsesResponse(body []byte, publicModel string, tools ResponsesToolInfo) ([]byte, error) {
	var r OAResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var output []map[string]any
	finish := "stop"
	if len(r.Choices) > 0 {
		ch := r.Choices[0]
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
		if m := ch.Message; m != nil {
			reasoning := m.ReasoningContent
			if reasoning == "" {
				reasoning = m.Reasoning
			}
			if reasoning != "" {
				output = append(output, map[string]any{"type": "reasoning", "id": randID("rs_"),
					"summary": []map[string]any{{"type": "summary_text", "text": reasoning}}})
			}
			if t := oaContentText(m.Content); t != "" {
				output = append(output, map[string]any{"type": "message", "id": randID("msg_"), "status": "completed", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": t, "annotations": []any{}}}})
			}
			for _, tc := range m.ToolCalls {
				output = append(output, rsToolCallItem(tools, tc.ID, tc.Function.Name, tc.Function.Arguments, "completed"))
			}
		}
	}
	return json.Marshal(rsResponseObject(randID("resp_"), publicModel, rsStatusFor(finish), time.Now().Unix(), output, r.Usage.toUsage(), finish))
}

// ResponsesErrorEvents is what a Responses client gets when the stream
// breaks after it started.
func ResponsesErrorEvents(msg string) []SSEEvent {
	resp := rsResponseObject(randID("resp_"), "", "failed", time.Now().Unix(), nil, Usage{}, "")
	resp["error"] = map[string]any{"code": "server_error", "message": msg}
	return []SSEEvent{
		jsonEvent("error", map[string]any{"type": "error", "code": "server_error", "message": msg, "param": nil, "sequence_number": 0}),
		jsonEvent("response.failed", map[string]any{"type": "response.failed", "response": resp, "sequence_number": 1}),
	}
}
