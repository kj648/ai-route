package convert

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// SSEEvent is one server-sent event. Event may be empty (OpenAI style).
type SSEEvent struct {
	Event string
	Data  string
}

// SSEReader parses a text/event-stream body.
type SSEReader struct {
	r *bufio.Reader
	// Raw collects non-SSE lines (e.g. a JSON error body returned instead of
	// a stream) so the caller can report them.
	Raw strings.Builder
}

func NewSSEReader(r io.Reader) *SSEReader {
	// small buffer: it is held for the whole life of every stream; longer
	// lines are assembled by readLine
	return &SSEReader{r: bufio.NewReaderSize(r, 8*1024)}
}

// MaxEventBytes caps a single SSE line and a single event, so a broken or
// hostile upstream cannot make the gateway buffer unbounded memory.
const MaxEventBytes = 8 << 20

// ErrEventTooLarge is returned when an event exceeds MaxEventBytes.
var ErrEventTooLarge = errors.New("upstream stream event too large")

// readLine reads one line without its terminator, refusing lines longer
// than MaxEventBytes.
func (s *SSEReader) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxEventBytes {
			return "", ErrEventTooLarge
		}
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}

// Next returns the next event, or io.EOF when the stream ends.
func (s *SSEReader) Next() (SSEEvent, error) {
	var ev SSEEvent
	var data []string
	size := 0
	hasData := false
	for {
		line, err := s.readLine()
		if err == ErrEventTooLarge {
			return SSEEvent{}, err
		}
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if hasData {
					ev.Data = strings.Join(data, "\n")
					return ev, nil
				}
				ev = SSEEvent{}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			case strings.HasPrefix(line, "data:"):
				d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
				if size += len(d); size > MaxEventBytes {
					return SSEEvent{}, ErrEventTooLarge
				}
				data = append(data, d)
				hasData = true
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			default:
				if s.Raw.Len() < 4096 {
					s.Raw.WriteString(line)
					s.Raw.WriteString("\n")
				}
			}
		}
		if err != nil {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return SSEEvent{}, err
		}
	}
}

// WriteSSE serializes an event.
func WriteSSE(w io.Writer, ev SSEEvent) error {
	var buf bytes.Buffer
	if ev.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(ev.Event)
		buf.WriteString("\n")
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	_, err := w.Write(buf.Bytes())
	return err
}

// StreamErrorMessage reports whether an upstream event is an error event.
func StreamErrorMessage(ev SSEEvent, proto string) (string, bool) {
	if ev.Data == "[DONE]" {
		return "", false
	}
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(ev.Data), &probe) != nil {
		return "", false
	}
	if proto == ProtoAnthropic && (ev.Event == "error" || probe.Type == "error") {
		return truncate(ev.Data, 500), true
	}
	if proto == ProtoResponses {
		return isResponsesErrorEvent(ev)
	}
	if !isNullOrEmpty(probe.Error) {
		return truncate(string(probe.Error), 500), true
	}
	return "", false
}

// StreamConverter transforms upstream events into client events.
type StreamConverter interface {
	Process(ev SSEEvent) []SSEEvent
	Finish() []SSEEvent
	Usage() Usage
	// Complete reports whether the upstream sent a terminal event; a stream
	// that hits EOF without one was cut off.
	Complete() bool
	// Err returns an error reported by the upstream inside the stream.
	Err() string
}

// ConvertedSignature marks thinking blocks synthesized from OpenAI
// reasoning_content. They carry no real signature, so they are stripped
// before a request is forwarded to an Anthropic upstream.
const ConvertedSignature = "ai-route-converted"

func jsonEvent(name string, v any) SSEEvent {
	b, _ := json.Marshal(v)
	return SSEEvent{Event: name, Data: string(b)}
}

// ---------- pass-through: OpenAI ----------

type oaPass struct {
	clientUsage bool
	usage       Usage
	done        bool
	finished    bool
	err         string
}

// NewOpenAIPassthrough forwards an OpenAI stream as is, recording usage.
// When the client did not request usage, injected usage-only chunks are dropped.
func NewOpenAIPassthrough(clientUsage bool) StreamConverter { return &oaPass{clientUsage: clientUsage} }

func (p *oaPass) Process(ev SSEEvent) []SSEEvent {
	if ev.Data == "[DONE]" {
		p.done = true
		return []SSEEvent{{Data: "[DONE]"}}
	}
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *OAUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal([]byte(ev.Data), &chunk)
	if !isNullOrEmpty(chunk.Error) {
		p.err = truncate(string(chunk.Error), 500)
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			p.finished = true
		}
	}
	if chunk.Usage != nil {
		if u := chunk.Usage.toUsage(); u.Input > 0 || u.Output > 0 {
			p.usage = u
		}
		if !p.clientUsage && len(chunk.Choices) == 0 {
			return nil
		}
	}
	return []SSEEvent{{Data: ev.Data}}
}

func (p *oaPass) Finish() []SSEEvent {
	if p.done {
		return nil
	}
	p.done = true
	return []SSEEvent{{Data: "[DONE]"}}
}

func (p *oaPass) Usage() Usage   { return p.usage }
func (p *oaPass) Complete() bool { return p.done || p.finished }
func (p *oaPass) Err() string    { return p.err }

// ---------- pass-through: Anthropic ----------

type anPass struct {
	usage    ANUsage
	finished bool
	err      string
}

// NewAnthropicPassthrough forwards an Anthropic stream as is, recording usage.
func NewAnthropicPassthrough() StreamConverter { return &anPass{} }

func mergeANUsage(dst *ANUsage, src ANUsage) {
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.Cost != nil {
		dst.Cost = src.Cost
	}
}

func (p *anPass) Process(ev SSEEvent) []SSEEvent {
	var d struct {
		Type    string `json:"type"`
		Message struct {
			Usage ANUsage `json:"usage"`
		} `json:"message"`
		Usage ANUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(ev.Data), &d) == nil {
		switch d.Type {
		case "message_start":
			mergeANUsage(&p.usage, d.Message.Usage)
		case "message_delta":
			mergeANUsage(&p.usage, d.Usage)
		case "message_stop":
			p.finished = true
		}
	}
	name := ev.Event
	if name == "" {
		name = d.Type
	}
	if name == "error" || d.Type == "error" {
		p.err = truncate(ev.Data, 500)
	}
	return []SSEEvent{{Event: name, Data: ev.Data}}
}

func (p *anPass) Finish() []SSEEvent { return nil }
func (p *anPass) Usage() Usage       { return p.usage.toUsage() }
func (p *anPass) Complete() bool     { return p.finished }
func (p *anPass) Err() string        { return p.err }

// ---------- Anthropic upstream -> OpenAI client ----------
