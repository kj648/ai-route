// Package convert translates requests, responses and SSE streams between the
// OpenAI Chat Completions protocol and the Anthropic Messages protocol.
package convert

import (
	"encoding/json"
	"strings"
)

const (
	ProtoOpenAI    = "openai"
	ProtoAnthropic = "anthropic"
	// ProtoEmbeddings is the OpenAI /embeddings endpoint (no conversion).
	ProtoEmbeddings = "embeddings"
)

// Usage is normalized token usage. Input includes cached tokens.
type Usage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cached int64 `json:"cached"`
}

// ---------- OpenAI request ----------

type OAChatRequest struct {
	Model               string            `json:"model"`
	Messages            []OAMessage       `json:"messages"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	Stop                json.RawMessage   `json:"stop,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *OAStreamOptions  `json:"stream_options,omitempty"`
	Tools               []OATool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	ResponseFormat      *OAResponseFormat `json:"response_format,omitempty"`
	User                string            `json:"user,omitempty"`
}

type OAResponseFormat struct {
	Type       string `json:"type"` // text | json_object | json_schema
	JSONSchema *struct {
		Name        string          `json:"name,omitempty"`
		Description string          `json:"description,omitempty"`
		Schema      json.RawMessage `json:"schema,omitempty"`
		Strict      bool            `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}

type OAStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OAMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []OAToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	// CacheControl is the message-level Anthropic cache breakpoint some
	// OpenAI-format clients send; it applies to the message's last block.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type OAContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type OAToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type OATool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// ---------- OpenAI response ----------

type OAResponse struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []OAChoice `json:"choices"`
	Usage   *OAUsage   `json:"usage,omitempty"`
}

type OAChoice struct {
	Index        int        `json:"index"`
	Message      *OAMessage `json:"message,omitempty"`
	Delta        *OAMessage `json:"delta,omitempty"`
	FinishReason *string    `json:"finish_reason"`
}

type OAUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	// some providers (e.g. kimi/deepseek style) report cache hits here
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens,omitempty"`
}

func (u *OAUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.Cached = u.PromptTokensDetails.CachedTokens
	}
	if out.Cached == 0 {
		out.Cached = u.PromptCacheHitTokens
	}
	return out
}

// ---------- Anthropic request ----------

type ANRequest struct {
	Model         string          `json:"model"`
	Messages      []ANMessage     `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []ANTool        `json:"tools,omitempty"`
	ToolChoice    *ANToolChoice   `json:"tool_choice,omitempty"`
	Thinking      *ANThinking     `json:"thinking,omitempty"`
	Metadata      *struct {
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
}

type ANMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ANBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *ANSource       `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type ANSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type ANTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type ANToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type ANThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// ---------- Anthropic response ----------

type ANResponse struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []ANBlock `json:"content"`
	StopReason   string    `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        ANUsage   `json:"usage"`
}

type ANUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

func (u ANUsage) toUsage() Usage {
	return Usage{
		Input:  u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		Output: u.OutputTokens,
		Cached: u.CacheReadInputTokens,
	}
}

// ---------- helpers ----------

// isJSONString reports whether raw is a JSON string literal.
func isJSONString(raw json.RawMessage) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '"':
			return true
		default:
			return false
		}
	}
	return false
}

func isNullOrEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// oaContentParts parses OpenAI message content (string | parts | null).
func oaContentParts(raw json.RawMessage) []OAContentPart {
	if isNullOrEmpty(raw) {
		return nil
	}
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return []OAContentPart{{Type: "text", Text: s}}
	}
	var parts []OAContentPart
	_ = json.Unmarshal(raw, &parts)
	return parts
}

func oaContentText(raw json.RawMessage) string {
	var sb strings.Builder
	for _, p := range oaContentParts(raw) {
		if p.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// anBlocks parses Anthropic content (string | blocks).
func anBlocks(raw json.RawMessage) []ANBlock {
	if isNullOrEmpty(raw) {
		return nil
	}
	if isJSONString(raw) {
		var s string
		_ = json.Unmarshal(raw, &s)
		return []ANBlock{{Type: "text", Text: s}}
	}
	var blocks []ANBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

func ptr[T any](v T) *T { return &v }
