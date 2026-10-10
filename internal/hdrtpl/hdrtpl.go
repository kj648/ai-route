// Package hdrtpl parses and evaluates the values of provider request
// headers. A value is literal text with optional {{ ... }} expressions:
//
//	X-Fixed: abc                                        fixed value
//	X-Session: {{header.X-Session-Id}}                  from the caller, required
//	X-Session: {{header.X-Session-Id?}}                 from the caller, optional
//	X-Session: {{header.X-Session-Id ?? $conversation}} caller's value, else generated
//	X-Session: {{$session}}                             the gateway's session id
//	X-Trace: ai-route-{{$requestId}}                    built-in variable
//	X-Agent: {{header.X-Agent ?? "ai-route"}}           quoted literal fallback
//
// An expression tries its sources left to right and uses the first one that
// has a value. When none has one the header is omitted; when the last source
// is a header without "?", the header is "required" from the caller.
package hdrtpl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Vars lists the built-in variables and what they hold.
var Vars = map[string]string{
	"session":      "stable per session: ses_ + hash of API key and the caller's X-Session-Id (or its client's native session header); $conversation when the caller sent none",
	"conversation": "stable per conversation: ses_ + hash of API key and first user message (random when there is none)",
	"uuid":         "new random UUID for every request",
	"requestId":    "the gateway's request id (shown in the request log)",
	"timestamp":    "unix seconds",
	"keyName":      "name of the caller's API key (URL-encoded)",
	"keyId":        "id of the caller's API key",
	"model":        "upstream model id",
}

// forbidden caller headers must never be copied upstream: they carry the
// gateway's own credentials or the user's private data.
var forbidden = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Cookie": true, "Proxy-Authorization": true,
}

type sourceKind int

const (
	srcHeader sourceKind = iota
	srcVar
	srcLiteral
)

type source struct {
	kind     sourceKind
	name     string // header name (canonical) / variable / literal text
	optional bool   // header source with "?"
}

type segment struct {
	text    string   // literal text when sources is empty
	sources []source // expression
}

// Template is a parsed header value.
type Template struct {
	segs []segment
}

// Parse parses a header value.
func Parse(value string) (*Template, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("header value must not contain line breaks")
	}
	t := &Template{}
	rest := value
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if strings.Contains(rest, "}}") {
				return nil, fmt.Errorf("unmatched }} in %q", value)
			}
			if rest != "" {
				t.segs = append(t.segs, segment{text: rest})
			}
			return t, nil
		}
		if i > 0 {
			t.segs = append(t.segs, segment{text: rest[:i]})
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unclosed {{ in %q", value)
		}
		srcs, err := parseExpr(rest[i+2 : i+j])
		if err != nil {
			return nil, fmt.Errorf("%w in %q", err, value)
		}
		t.segs = append(t.segs, segment{sources: srcs})
		rest = rest[i+j+2:]
	}
}

func parseExpr(expr string) ([]source, error) {
	var out []source
	for _, part := range strings.Split(expr, "??") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			return nil, fmt.Errorf("empty expression")
		case strings.HasPrefix(part, "header."):
			name := strings.TrimPrefix(part, "header.")
			opt := strings.HasSuffix(name, "?")
			name = http.CanonicalHeaderKey(strings.TrimSuffix(name, "?"))
			if name == "" || strings.ContainsAny(name, " \t\"{}") {
				return nil, fmt.Errorf("invalid header name in {{%s}}", part)
			}
			if forbidden[name] {
				return nil, fmt.Errorf("header %s cannot be forwarded", name)
			}
			out = append(out, source{kind: srcHeader, name: name, optional: opt})
		case strings.HasPrefix(part, "$"):
			name := part[1:]
			if _, ok := Vars[name]; !ok {
				return nil, fmt.Errorf("unknown variable $%s", name)
			}
			out = append(out, source{kind: srcVar, name: name})
		case len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"':
			out = append(out, source{kind: srcLiteral, name: part[1 : len(part)-1]})
		default:
			return nil, fmt.Errorf("unknown expression {{%s}} (use header.Name, $variable or \"text\")", part)
		}
	}
	return out, nil
}

// Required returns the caller headers this value cannot do without.
func (t *Template) Required() []string {
	var out []string
	for _, s := range t.segs {
		if n := len(s.sources); n > 0 {
			if last := s.sources[n-1]; last.kind == srcHeader && !last.optional {
				out = append(out, last.name)
			}
		}
	}
	return out
}

// Dynamic reports whether the value has any expression.
func (t *Template) Dynamic() bool {
	for _, s := range t.segs {
		if s.sources != nil {
			return true
		}
	}
	return false
}

// Context is what expressions are evaluated against. Any field may be empty.
type Context struct {
	Header       http.Header // the caller's request headers
	Session      string      // already formatted, e.g. ses_...
	Conversation string      // already formatted, e.g. ses_...
	RequestID    string
	KeyName      string
	KeyID        int64
	Model        string
	Now          time.Time
}

// Eval resolves the value; ok is false when an expression has no value, in
// which case the header should be omitted.
func (t *Template) Eval(c *Context) (value string, ok bool) {
	var sb strings.Builder
	for _, s := range t.segs {
		if s.sources == nil {
			sb.WriteString(s.text)
			continue
		}
		v, found := "", false
		for _, src := range s.sources {
			if v, found = c.lookup(src); found {
				break
			}
		}
		if !found {
			return "", false
		}
		sb.WriteString(v)
	}
	return sb.String(), true
}

func (c *Context) lookup(s source) (string, bool) {
	if c == nil {
		c = &Context{}
	}
	switch s.kind {
	case srcLiteral:
		return s.name, true
	case srcHeader:
		if c.Header == nil {
			return "", false
		}
		v := c.Header.Get(s.name)
		return v, v != ""
	}
	switch s.name {
	case "session":
		return c.Session, c.Session != ""
	case "conversation":
		return c.Conversation, c.Conversation != ""
	case "uuid":
		return newUUID(), true
	case "requestId":
		return c.RequestID, c.RequestID != ""
	case "timestamp":
		now := c.Now
		if now.IsZero() {
			now = time.Now()
		}
		return strconv.FormatInt(now.Unix(), 10), true
	case "keyName":
		return url.QueryEscape(c.KeyName), c.KeyName != ""
	case "keyId":
		return strconv.FormatInt(c.KeyID, 10), c.KeyID > 0
	case "model":
		return c.Model, c.Model != ""
	}
	return "", false
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
