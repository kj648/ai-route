package convert

import (
	"encoding/json"
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
	Detail   string `json:"detail"`
	FileData string `json:"file_data"` // input_file: a data URL
	Filename string `json:"filename"`
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
