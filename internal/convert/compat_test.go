package convert

// The pairwise converter names the tests were written against, as thin
// wrappers over the IR parsers and emitters.

func OpenAIToAnthropicRequest(body []byte, model string, defaultMaxTokens int, nativeStructured bool) ([]byte, error) {
	r, err := ParseRequest(ProtoOpenAI, body)
	if err != nil {
		return nil, err
	}
	return EmitRequest(ProtoAnthropic, r, EmitOptions{Model: model, DefaultMaxTokens: defaultMaxTokens, NativeStructured: nativeStructured})
}

func AnthropicToOpenAIRequest(body []byte, model string) ([]byte, error) {
	r, err := ParseRequest(ProtoAnthropic, body)
	if err != nil {
		return nil, err
	}
	return EmitRequest(ProtoOpenAI, r, EmitOptions{Model: model})
}

func ResponsesToChatRequest(body []byte, model string) ([]byte, error) {
	r, err := ParseRequest(ProtoResponses, body)
	if err != nil {
		return nil, err
	}
	return EmitRequest(ProtoOpenAI, r, EmitOptions{Model: model})
}

func ChatToResponsesRequest(body []byte, model string) ([]byte, error) {
	r, err := ParseRequest(ProtoOpenAI, body)
	if err != nil {
		return nil, err
	}
	return EmitRequest(ProtoResponses, r, EmitOptions{Model: model})
}

func OpenAIToAnthropicResponse(body []byte, publicModel string) ([]byte, Usage, error) {
	return ConvertResponse(ProtoAnthropic, ProtoOpenAI, body, publicModel, nil)
}

func ResponsesToChatResponse(body []byte, publicModel string) ([]byte, Usage, error) {
	return ConvertResponse(ProtoOpenAI, ProtoResponses, body, publicModel, nil)
}

func ChatToResponsesResponse(body []byte, publicModel string, tools ResponsesToolInfo) ([]byte, error) {
	r, err := ParseResponse(ProtoOpenAI, body)
	if err != nil {
		return nil, err
	}
	return EmitResponse(ProtoResponses, r, publicModel, tools)
}

func NewOpenAIToAnthropicStream(publicModel string) StreamConverter {
	return NewStreamConverter(ProtoAnthropic, ProtoOpenAI, publicModel, false, nil)
}

func NewAnthropicToOpenAIStream(publicModel string, includeUsage bool) StreamConverter {
	return NewStreamConverter(ProtoOpenAI, ProtoAnthropic, publicModel, includeUsage, nil)
}

func NewChatToResponsesStream(publicModel string, tools ResponsesToolInfo) StreamConverter {
	return &pipeline{dec: newDecoder(ProtoOpenAI), enc: newEncoder(ProtoResponses, publicModel, false, tools)}
}
