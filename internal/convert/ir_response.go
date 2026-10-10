package convert

import (
	"encoding/json"
	"strings"
	"time"
)

// ---------- parsing ----------

// ParseResponse reads a non-stream completion in proto into the IR.
func ParseResponse(proto string, body []byte) (*Response, error) {
	switch proto {
	case ProtoAnthropic:
		return parseAnthropicResponse(body)
	case ProtoResponses:
		return parseResponsesResponse(body)
	default:
		return parseChatResponse(body)
	}
}

// chatStopReason maps finish_reason; a call cut off by max_tokens stays
// max_tokens so the client does not execute a half-formed call.
func chatStopReason(finish string, hasTool bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	if hasTool {
		return "tool_use"
	}
	return "end_turn"
}

func parseChatResponse(body []byte) (*Response, error) {
	var r OAResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := &Response{ID: r.ID, Model: r.Model, Usage: r.Usage.toUsage()}
	finish := ""
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
		if m := c.Message; m != nil {
			reasoning := m.ReasoningContent
			if reasoning == "" {
				reasoning = m.Reasoning
			}
			if reasoning != "" {
				out.Content = append(out.Content, Block{Type: "thinking", Thinking: reasoning, Signature: ConvertedSignature})
			}
			if t := oaContentText(m.Content); t != "" {
				out.Content = append(out.Content, textBlock(t, nil))
			}
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					id = randID("call_")
				}
				out.Content = append(out.Content, Block{Type: "tool_use", ID: id, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
			}
		}
	}
	out.StopReason = chatStopReason(finish, out.hasTool())
	return out, nil
}

func parseAnthropicResponse(body []byte) (*Response, error) {
	var r ANResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := &Response{ID: r.ID, Model: r.Model, StopReason: r.StopReason, Usage: r.Usage.toUsage()}
	if r.StopSequence != nil {
		out.StopSequence = *r.StopSequence
	}
	for _, b := range r.Content {
		out.Content = append(out.Content, anBlockToIR(b))
	}
	if out.StopReason == "" {
		out.StopReason = "end_turn"
	}
	return out, nil
}

// rsStopReason maps a Responses status to a stop reason.
func rsStopReason(status, reason string, hasTool bool) string {
	switch {
	case status == "incomplete" && reason == "max_output_tokens":
		return "max_tokens"
	case status == "incomplete" && reason == "content_filter":
		return "refusal"
	case hasTool:
		return "tool_use"
	}
	return "end_turn"
}

func parseResponsesResponse(body []byte) (*Response, error) {
	var r rsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := &Response{ID: r.ID, Usage: r.Usage.toUsage()}
	for _, it := range r.Output {
		switch it.Type {
		case "message":
			var sb strings.Builder
			for _, p := range it.Content {
				if p.Type == "output_text" {
					sb.WriteString(p.Text)
				} else if p.Type == "refusal" {
					sb.WriteString(p.Refusal)
				}
			}
			if sb.Len() > 0 {
				out.Content = append(out.Content, textBlock(sb.String(), nil))
			}
		case "reasoning":
			var sb strings.Builder
			for _, s := range it.Summary {
				sb.WriteString(s.Text)
			}
			if sb.Len() > 0 {
				out.Content = append(out.Content, Block{Type: "thinking", Thinking: sb.String(), Signature: ConvertedSignature})
			}
		case "function_call":
			out.Content = append(out.Content, Block{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: toolInput(it.Arguments)})
		case "custom_tool_call":
			args, _ := json.Marshal(map[string]string{"input": it.Input})
			out.Content = append(out.Content, Block{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: args})
		}
	}
	reason := ""
	if r.IncompleteDetails != nil {
		reason = r.IncompleteDetails.Reason
	}
	out.StopReason = rsStopReason(r.Status, reason, out.hasTool())
	return out, nil
}

// ---------- emitting ----------

// EmitResponse writes the IR as a completion in proto. publicModel is the
// name the client asked for; tools describes the client's Responses tools
// (custom tools and namespaces).
func EmitResponse(proto string, r *Response, publicModel string, tools ResponsesToolInfo) ([]byte, error) {
	switch proto {
	case ProtoAnthropic:
		return emitAnthropicResponse(r, publicModel)
	case ProtoResponses:
		return emitResponsesResponse(r, publicModel, tools)
	default:
		return emitChatResponse(r, publicModel)
	}
}

// chatMessageJSON writes the assistant message of a completion.
func chatMessageJSON(blocks []Block) map[string]any {
	var text, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			toolCalls = append(toolCalls, oaToolCall(b))
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if text.Len() == 0 && len(toolCalls) > 0 {
		msg["content"] = nil
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func emitChatResponse(r *Response, publicModel string) ([]byte, error) {
	id := r.ID
	if id == "" {
		id = randID("chatcmpl-")
	}
	return json.Marshal(map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": publicModel,
		"choices": []map[string]any{{"index": 0, "message": chatMessageJSON(r.Content), "finish_reason": anStopToOpenAI(r.StopReason)}},
		"usage":   oaUsageFrom(r.Usage),
	})
}

func emitAnthropicResponse(r *Response, publicModel string) ([]byte, error) {
	id := r.ID
	if !strings.HasPrefix(id, "msg_") {
		id = randID("msg_")
	}
	content := []map[string]any{}
	for _, b := range r.Content {
		switch b.Type {
		case "text", "thinking", "redacted_thinking", "tool_use":
			if j := anBlockJSON(b); j != nil {
				content = append(content, j)
			}
		}
	}
	var stopSeq any
	if r.StopSequence != "" {
		stopSeq = r.StopSequence
	}
	return json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": publicModel,
		"content": content, "stop_reason": r.StopReason, "stop_sequence": stopSeq,
		"usage": anUsageFrom(r.Usage),
	})
}

func emitResponsesResponse(r *Response, publicModel string, tools ResponsesToolInfo) ([]byte, error) {
	var output []map[string]any
	for _, b := range r.Content {
		switch b.Type {
		case "thinking":
			output = append(output, map[string]any{"type": "reasoning", "id": randID("rs_"),
				"summary": []map[string]any{{"type": "summary_text", "text": b.Thinking}}})
		case "text":
			output = append(output, map[string]any{"type": "message", "id": randID("msg_"), "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": b.Text, "annotations": []any{}}}})
		case "tool_use":
			args := string(b.Input)
			if isNullOrEmpty(b.Input) {
				args = "{}"
			}
			output = append(output, rsToolCallItem(tools, b.ID, b.Name, args, "completed"))
		}
	}
	finish := anStopToOpenAI(r.StopReason)
	return json.Marshal(rsResponseObject(randID("resp_"), publicModel, rsStatusFor(finish), time.Now().Unix(), output, r.Usage, finish))
}
