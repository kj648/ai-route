package convert

import (
	"encoding/json"
	"strings"
)

// The intermediate representation. Every protocol is parsed into these
// types and emitted from them, so a request or response passes through one
// parser and one emitter whatever the pair of protocols. The shape follows
// Anthropic's block model, which expresses everything the others do, with a
// few extension fields for what only the OpenAI protocols carry.

// Request is a protocol-neutral chat request.
type Request struct {
	Model    string
	System   []Block // text blocks (with cache breakpoints)
	Messages []Message
	Tools    []Tool
	// ToolChoice is nil when the client said nothing.
	ToolChoice *ToolChoice
	// ParallelTools is OpenAI's parallel_tool_calls (nil = unspecified);
	// Anthropic's disable_parallel_tool_use is its inverse.
	ParallelTools *bool
	MaxTokens     int // 0 = not given
	Temperature   *float64
	TopP          *float64
	TopK          *int
	Stop          []string
	Stream        bool
	// StreamUsage reports that an OpenAI client asked for usage in its
	// stream itself (stream_options.include_usage).
	StreamUsage bool
	// Effort is the OpenAI / Responses reasoning effort level; Thinking and
	// OutputEffort are Anthropic's thinking parameter and
	// output_config.effort. A request carries what its protocol said.
	Effort       string
	Thinking     *ANThinking
	OutputEffort string
	// ResponseFormat is OpenAI structured output (also Responses text.format).
	ResponseFormat *OAResponseFormat
	// User is the end-user id (user, metadata.user_id, safety_identifier).
	User string
}

// Message is one turn.
type Message struct {
	Role    string // user | assistant
	Content []Block
}

// Block is one content block. Type selects the fields that matter:
//
//	text               Text
//	image              Source, Detail
//	document           Source, Title
//	thinking           Thinking, Signature
//	redacted_thinking  Data
//	tool_use           ID, Name, Input
//	tool_result        ToolUseID, Content, IsError
//
// CacheControl is an Anthropic cache breakpoint on the block.
type Block struct {
	Type         string
	Text         string
	Source       *ANSource
	Detail       string // OpenAI image detail: low | high | auto
	Title        string
	Thinking     string
	Signature    string
	Data         string
	ID           string
	Name         string
	Input        json.RawMessage // a JSON object
	ToolUseID    string
	Content      []Block
	IsError      bool
	CacheControl json.RawMessage
}

// Tool is a function the model may call. Server-side tools of a protocol
// (Anthropic web_search, OpenAI built-ins) have no InputSchema and are
// dropped when the upstream speaks another protocol.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Strict is OpenAI strict schema mode (nil = not given).
	Strict *bool
	// Custom marks a Responses freeform tool; Format is its grammar.
	Custom bool
	Format json.RawMessage
	// Namespace is the Responses namespace the tool was declared in.
	Namespace string
	// ServerType is the Anthropic server tool type (web_search_20250305 ...).
	ServerType   string
	CacheControl json.RawMessage
}

// ToolChoice: Mode is auto | any | none | tool (with Name).
type ToolChoice struct {
	Mode string
	Name string
}

// Response is a protocol-neutral completion.
type Response struct {
	ID      string
	Model   string
	Content []Block // text, thinking, redacted_thinking, tool_use
	// StopReason uses Anthropic's vocabulary: end_turn | max_tokens |
	// tool_use | stop_sequence | refusal.
	StopReason   string
	StopSequence string
	Usage        Usage
}

// ---------- block helpers ----------

func textBlock(text string, cc json.RawMessage) Block {
	return Block{Type: "text", Text: text, CacheControl: cc}
}

// hasTool reports whether the response calls a tool.
func (r *Response) hasTool() bool {
	for _, b := range r.Content {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// textOf joins the text blocks of a slice with sep.
func textOf(blocks []Block, sep string) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, sep)
}

// appendTurn adds blocks to the last message when it has the same role
// (Anthropic requires alternating roles) or starts a new one.
func appendTurn(msgs []Message, role string, blocks []Block) []Message {
	if len(blocks) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, Message{Role: role, Content: blocks})
}

// withTrailingCache applies a message-level breakpoint to the last block
// unless it carries its own.
func withTrailingCache(blocks []Block, cc json.RawMessage) []Block {
	if n := len(blocks); n > 0 && !isNullOrEmpty(cc) && isNullOrEmpty(blocks[n-1].CacheControl) {
		blocks[n-1].CacheControl = cc
	}
	return blocks
}

// dataURLSource turns an OpenAI image URL into an Anthropic source.
func dataURLSource(url string) *ANSource {
	if strings.HasPrefix(url, "data:") {
		// data:image/png;base64,XXXX
		if meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ","); ok {
			return &ANSource{Type: "base64", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}
		}
	}
	return &ANSource{Type: "url", URL: url}
}

// sourceURL is the inverse: an image URL (data URL for base64 sources).
func sourceURL(src *ANSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		return "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		return src.URL
	}
	return ""
}

// imageBlock builds an image (or, for a PDF data URL, a document) block.
func imageBlock(url, detail string, cc json.RawMessage) Block {
	src := dataURLSource(url)
	if src.Type == "base64" && !strings.HasPrefix(src.MediaType, "image/") {
		return Block{Type: "document", Source: src, CacheControl: cc}
	}
	return Block{Type: "image", Source: src, Detail: detail, CacheControl: cc}
}

// toolInput normalizes tool-call arguments to a JSON object: empty or
// invalid arguments become {}, a non-object value is wrapped.
func toolInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage(`{}`)
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return json.RawMessage(`{}`)
	}
	if _, ok := v.(map[string]any); ok {
		return json.RawMessage(strings.TrimSpace(args))
	}
	b, _ := json.Marshal(map[string]any{"value": v})
	return b
}

// defaultSchema is the input schema of a tool declared without one.
var defaultSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// effortFromThinking derives an OpenAI effort level from an Anthropic
// thinking parameter, for upstreams that only know reasoning_effort.
func effortFromThinking(th *ANThinking, outputEffort string) string {
	if th == nil {
		return ""
	}
	switch th.Type {
	case "adaptive":
		if outputEffort != "" {
			return outputEffort
		}
		return "medium"
	case "enabled":
		switch {
		case th.BudgetTokens <= 0:
			return "medium"
		case th.BudgetTokens <= 2048:
			return "low"
		case th.BudgetTokens <= 8192:
			return "medium"
		}
		return "high"
	}
	return ""
}
