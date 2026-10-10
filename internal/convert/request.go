package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RewriteModel replaces the "model" field of a JSON request body, leaving the
// rest of the body untouched, except sampling fields the model rejects
// (fixedSampling). For OpenAI streaming requests it also enables
// stream_options.include_usage so token usage can be recorded; the returned
// bool reports whether the client had asked for usage itself.
func RewriteModel(body []byte, model string, proto string) ([]byte, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, fmt.Errorf("invalid JSON body: %w", err)
	}
	m["model"], _ = json.Marshal(model)
	if fixedSampling(model) {
		for _, k := range samplingFields {
			delete(m, k)
		}
	}
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

// nativeOutputSchema returns the schema to send as output_config.format.
// Only strict json_schema is mapped: Anthropic requires the same schema
// restrictions (additionalProperties: false etc.) that OpenAI strict mode
// does, while a non-strict schema may be rejected.
func nativeOutputSchema(rf *OAResponseFormat, native bool) json.RawMessage {
	if !native || rf == nil || rf.Type != "json_schema" || rf.JSONSchema == nil ||
		!rf.JSONSchema.Strict || isNullOrEmpty(rf.JSONSchema.Schema) {
		return nil
	}
	return rf.JSONSchema.Schema
}

// responseFormatInstruction is the system prompt text that enforces
// response_format when the upstream cannot do it natively.
func responseFormatInstruction(rf *OAResponseFormat, native bool) string {
	if rf == nil || nativeOutputSchema(rf, native) != nil {
		return ""
	}
	const plain = "Respond with valid JSON only: no Markdown code fences and no text before or after the JSON."
	switch rf.Type {
	case "json_object":
		return plain
	case "json_schema":
		if rf.JSONSchema == nil || isNullOrEmpty(rf.JSONSchema.Schema) {
			return plain
		}
		var sb strings.Builder
		sb.WriteString(plain)
		sb.WriteString(" The JSON must conform to this JSON Schema")
		if rf.JSONSchema.Name != "" {
			sb.WriteString(" (" + rf.JSONSchema.Name + ")")
		}
		sb.WriteString(":\n")
		sb.Write(rf.JSONSchema.Schema)
		if rf.JSONSchema.Description != "" {
			sb.WriteString("\nSchema description: " + rf.JSONSchema.Description)
		}
		return sb.String()
	}
	return ""
}

func effortBudget(effort string) int {
	switch effort {
	case "minimal", "low":
		return 2048
	case "medium":
		return 8192
	case "high", "xhigh", "max":
		return 16384
	}
	return 0
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

// ConversationKey identifies a conversation for sticky routing: the first
// user message stays the same while the conversation grows, so requests of
// one conversation can keep hitting the same upstream (and its prompt
// cache). It is "" when the body has no user message.
// proto is the client protocol: only Responses requests keep their
// messages in "input" (for embeddings it is the text to embed).
func ConversationKey(body []byte, proto string) string {
	type message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var req struct {
		Messages []message       `json:"messages"`
		Input    json.RawMessage `json:"input"` // Responses API
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	msgs := req.Messages
	if proto == ProtoResponses {
		if isJSONString(req.Input) {
			return string(req.Input)
		}
		_ = json.Unmarshal(req.Input, &msgs)
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return string(m.Content)
		}
	}
	return ""
}
