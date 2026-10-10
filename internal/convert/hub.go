package convert

import "encoding/json"

// The gateway speaks three protocols: OpenAI Chat Completions, Anthropic
// Messages and OpenAI Responses. Every request, response and stream is
// parsed into the intermediate form (ir.go: Anthropic's block model with a
// few extensions) and emitted from it, so each protocol has one parser and
// one emitter and any pair converts without loss through a third
// protocol. Same-protocol traffic is passed through byte for byte.

// ConvertRequest turns a client body in the inbound protocol into an upstream
// body for model in the upstream protocol. clientUsage reports whether an
// OpenAI streaming client asked for usage itself.
func ConvertRequest(inbound, upstream string, body []byte, model string, defaultMaxTokens int, nativeStructured bool) ([]byte, bool, error) {
	if inbound == upstream {
		return RewriteModel(body, model, upstream)
	}
	r, err := ParseRequest(inbound, body)
	if err != nil {
		return nil, false, err
	}
	out, err := EmitRequest(upstream, r, EmitOptions{Model: model, DefaultMaxTokens: defaultMaxTokens, NativeStructured: nativeStructured})
	return out, r.StreamUsage, err
}

// ConvertResponse turns a non-stream upstream body into the inbound
// protocol; usage is read from the upstream body. clientReq is the client's
// request (Responses clients need their tool types back).
func ConvertResponse(inbound, upstream string, body []byte, publicModel string, clientReq []byte) ([]byte, Usage, error) {
	if inbound == upstream {
		return body, ExtractUsage(body, upstream), nil
	}
	r, err := ParseResponse(upstream, body)
	if err != nil {
		return nil, Usage{}, err
	}
	out, err := EmitResponse(inbound, r, publicModel, ParseResponsesTools(clientReq))
	return out, r.Usage, err
}

// NewStreamConverter returns the converter from an upstream stream to the
// client's protocol; clientReq is the client's request.
func NewStreamConverter(inbound, upstream, publicModel string, clientUsage bool, clientReq []byte) StreamConverter {
	if inbound == upstream {
		switch inbound {
		case ProtoOpenAI:
			return NewOpenAIPassthrough(clientUsage)
		case ProtoAnthropic:
			return NewAnthropicPassthrough()
		default:
			return NewResponsesPassthrough()
		}
	}
	return &pipeline{dec: newDecoder(upstream), enc: newEncoder(inbound, publicModel, clientUsage, ParseResponsesTools(clientReq))}
}

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
