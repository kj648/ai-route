package convert

import "encoding/json"

// The gateway speaks three protocols: OpenAI Chat Completions, Anthropic
// Messages and OpenAI Responses. Chat Completions is the hub: every other
// pair is converted through it (e.g. Responses -> Chat -> Anthropic), so only
// X <-> Chat converters exist. Same-protocol traffic is passed through.

// ConvertRequest turns a client body in the inbound protocol into an upstream
// body for model in the upstream protocol. clientUsage reports whether an
// OpenAI streaming client asked for usage itself.
func ConvertRequest(inbound, upstream string, body []byte, model string, defaultMaxTokens int, nativeStructured bool) ([]byte, bool, error) {
	if inbound == upstream {
		return RewriteModel(body, model, upstream)
	}
	clientUsage := inbound == ProtoOpenAI && ClientWantsUsage(body)
	// direct converters that predate the hub
	switch {
	case inbound == ProtoOpenAI && upstream == ProtoAnthropic:
		out, err := OpenAIToAnthropicRequest(body, model, defaultMaxTokens, nativeStructured)
		return out, clientUsage, err
	case inbound == ProtoAnthropic && upstream == ProtoOpenAI:
		out, err := AnthropicToOpenAIRequest(body, model)
		return out, false, err
	}
	var chat []byte
	var err error
	switch inbound {
	case ProtoOpenAI:
		chat = body
	case ProtoAnthropic:
		chat, err = AnthropicToOpenAIRequest(body, model)
	case ProtoResponses:
		chat, err = ResponsesToChatRequest(body, model)
	}
	if err != nil {
		return nil, false, err
	}
	switch upstream {
	case ProtoAnthropic:
		chat, err = OpenAIToAnthropicRequest(chat, model, defaultMaxTokens, nativeStructured)
	case ProtoResponses:
		chat, err = ChatToResponsesRequest(chat, model)
	}
	return chat, clientUsage, err
}

// ConvertResponse turns a non-stream upstream body into the inbound
// protocol; usage is read from the upstream body. clientReq is the client's
// request (Responses clients need their tool types back).
func ConvertResponse(inbound, upstream string, body []byte, publicModel string, clientReq []byte) ([]byte, Usage, error) {
	if inbound == upstream {
		return body, ExtractUsage(body, upstream), nil
	}
	switch {
	case inbound == ProtoOpenAI && upstream == ProtoAnthropic:
		return AnthropicToOpenAIResponse(body, publicModel)
	case inbound == ProtoAnthropic && upstream == ProtoOpenAI:
		return OpenAIToAnthropicResponse(body, publicModel)
	}
	var chat []byte
	var usage Usage
	var err error
	switch upstream {
	case ProtoOpenAI:
		chat, usage = body, ExtractUsage(body, ProtoOpenAI)
	case ProtoAnthropic:
		chat, usage, err = AnthropicToOpenAIResponse(body, publicModel)
	case ProtoResponses:
		chat, usage, err = ResponsesToChatResponse(body, publicModel)
	}
	if err != nil {
		return nil, usage, err
	}
	switch inbound {
	case ProtoAnthropic:
		chat, _, err = OpenAIToAnthropicResponse(chat, publicModel)
	case ProtoResponses:
		chat, err = ChatToResponsesResponse(chat, publicModel, ParseResponsesTools(clientReq))
	}
	return chat, usage, err
}

// NewStreamConverter returns the converter from an upstream stream to the
// client's protocol; clientReq is the client's request.
func NewStreamConverter(inbound, upstream, publicModel string, clientUsage bool, clientReq []byte) StreamConverter {
	switch {
	case inbound == upstream && inbound == ProtoOpenAI:
		return NewOpenAIPassthrough(clientUsage)
	case inbound == upstream && inbound == ProtoAnthropic:
		return NewAnthropicPassthrough()
	case inbound == upstream:
		return NewResponsesPassthrough()
	case inbound == ProtoOpenAI && upstream == ProtoAnthropic:
		return NewAnthropicToOpenAIStream(publicModel, clientUsage)
	case inbound == ProtoAnthropic && upstream == ProtoOpenAI:
		return NewOpenAIToAnthropicStream(publicModel)
	}
	// upstream -> chat chunks (always with usage, the next stage needs it)
	var toChat StreamConverter
	switch upstream {
	case ProtoOpenAI:
		toChat = NewOpenAIPassthrough(true)
	case ProtoAnthropic:
		toChat = NewAnthropicToOpenAIStream(publicModel, true)
	default:
		toChat = NewResponsesToChatStream(publicModel, inbound != ProtoOpenAI || clientUsage)
	}
	switch inbound {
	case ProtoOpenAI:
		return toChat
	case ProtoAnthropic:
		return &chainConverter{first: toChat, second: NewOpenAIToAnthropicStream(publicModel)}
	default:
		return &chainConverter{first: toChat, second: NewChatToResponsesStream(publicModel, ParseResponsesTools(clientReq))}
	}
}

// chainConverter feeds the events of one converter into another. Usage,
// completion and errors are those of the upstream-facing first stage.
type chainConverter struct {
	first, second StreamConverter
}

func (c *chainConverter) pipe(evs []SSEEvent) []SSEEvent {
	var out []SSEEvent
	for _, ev := range evs {
		out = append(out, c.second.Process(ev)...)
	}
	return out
}

func (c *chainConverter) Process(ev SSEEvent) []SSEEvent { return c.pipe(c.first.Process(ev)) }

func (c *chainConverter) Finish() []SSEEvent {
	return append(c.pipe(c.first.Finish()), c.second.Finish()...)
}

func (c *chainConverter) Usage() Usage   { return c.first.Usage() }
func (c *chainConverter) Complete() bool { return c.first.Complete() }
func (c *chainConverter) Err() string    { return c.first.Err() }

// isResponsesErrorEvent reports error events of a Responses stream.
func isResponsesErrorEvent(ev SSEEvent) (string, bool) {
	var probe struct {
		Type     string          `json:"type"`
		Message  string          `json:"message"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal([]byte(ev.Data), &probe) != nil {
		return "", false
	}
	switch {
	case probe.Type == "error" || ev.Event == "error":
		if probe.Message != "" {
			return truncate(probe.Message, 500), true
		}
		return truncate(ev.Data, 500), true
	case probe.Type == "response.failed":
		var r struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(probe.Response, &r)
		if r.Error != nil && r.Error.Message != "" {
			return truncate(r.Error.Message, 500), true
		}
		return "response failed", true
	}
	return "", false
}
