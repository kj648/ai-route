package convert

import (
	"encoding/json"
)

// Responses streams: every event is `event: <type>` + JSON with "type" and
// "sequence_number"; there is no [DONE], the stream ends with
// response.completed / response.incomplete / response.failed.

// ---------- pass-through: Responses ----------

type rsPass struct {
	usage    Usage
	finished bool
	err      string
}

// NewResponsesPassthrough forwards a Responses stream, recording usage.
func NewResponsesPassthrough() StreamConverter { return &rsPass{} }

func (p *rsPass) Process(ev SSEEvent) []SSEEvent {
	var d struct {
		Type     string      `json:"type"`
		Response *rsResponse `json:"response"`
	}
	_ = json.Unmarshal([]byte(ev.Data), &d)
	switch d.Type {
	case "response.completed", "response.incomplete":
		p.finished = true
		if d.Response != nil {
			p.usage = d.Response.Usage.toUsage()
		}
	case "response.failed", "error":
		msg, _ := isResponsesErrorEvent(ev)
		p.err = msg
	}
	return []SSEEvent{ev}
}

func (p *rsPass) Finish() []SSEEvent { return nil }
func (p *rsPass) Usage() Usage       { return p.usage }
func (p *rsPass) Complete() bool     { return p.finished }
func (p *rsPass) Err() string        { return p.err }

// ---------- Responses upstream -> Chat chunks ----------
