package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"ai-route/internal/convert"
	"ai-route/internal/hdrtpl"
	"ai-route/internal/store"
)

// TestResult is returned by the admin "test" actions.
type TestResult struct {
	OK         bool            `json:"ok"`
	HTTPStatus int             `json:"http_status"`
	LatencyMs  int64           `json:"latency_ms"`
	Target     string          `json:"target,omitempty"`
	Attempts   []store.Attempt `json:"attempts,omitempty"`
	Reply      string          `json:"reply"`
	Raw        string          `json:"raw"`
	Usage      convert.Usage   `json:"usage"`
}

func testBody(model string, stream bool, prompt string) []byte {
	if prompt == "" {
		prompt = "Reply with the single word: OK"
	}
	// the same minimal body is valid for both protocols
	b, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 256, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": prompt}}})
	return b
}

// extractReply pulls the assistant text out of a (possibly streamed) response.
func extractReply(proto string, stream bool, body string) string {
	var sb strings.Builder
	if !stream {
		if proto == convert.ProtoAnthropic {
			var r convert.ANResponse
			if json.Unmarshal([]byte(body), &r) == nil {
				thinking := ""
				for _, b := range r.Content {
					if b.Type == "text" {
						sb.WriteString(b.Text)
					} else if b.Type == "thinking" {
						thinking += b.Thinking
					}
				}
				if sb.Len() == 0 && thinking != "" {
					sb.WriteString("[thinking] " + truncate(thinking, 200))
				}
			}
		} else {
			var r convert.OAResponse
			if json.Unmarshal([]byte(body), &r) == nil && len(r.Choices) > 0 && r.Choices[0].Message != nil {
				var s string
				if json.Unmarshal(r.Choices[0].Message.Content, &s) == nil {
					sb.WriteString(s)
				}
				if sb.Len() == 0 && r.Choices[0].Message.ReasoningContent != "" {
					sb.WriteString("[thinking] " + truncate(r.Choices[0].Message.ReasoningContent, 200))
				}
			}
		}
		return sb.String()
	}
	reader := convert.NewSSEReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err != nil {
			break
		}
		if proto == convert.ProtoAnthropic {
			var d struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(ev.Data), &d) == nil {
				sb.WriteString(d.Delta.Text)
			}
		} else {
			var d struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(ev.Data), &d) == nil && len(d.Choices) > 0 {
				sb.WriteString(d.Choices[0].Delta.Content)
			}
		}
	}
	return sb.String()
}

// TestModel sends a tiny request through the normal routing path of a public
// model (breakers and fallbacks included). The request is logged.
func (g *Gateway) TestModel(ctx context.Context, model, proto string, stream bool, prompt string) TestResult {
	if proto != convert.ProtoAnthropic {
		proto = convert.ProtoOpenAI
	}
	body := testBody(model, stream, prompt)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")).WithContext(ctx)
	r.Header.Set("User-Agent", "ai-route-admin-test")
	// headers the providers require from callers get a test value
	for _, p := range g.store.Snapshot().Providers {
		for _, v := range p.Headers {
			if t, err := hdrtpl.Parse(v); err == nil {
				for _, name := range t.Required() {
					r.Header.Set(name, "ai-route-admin-test")
				}
			}
		}
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	key := &store.APIKey{Name: "(admin test)", Enabled: true}
	entry := g.route(rec, r, proto, body, key)
	res := TestResult{
		Attempts:   entry.Attempts,
		HTTPStatus: rec.Code,
		LatencyMs:  time.Since(start).Milliseconds(),
		Target:     rec.Header().Get("X-Route-Target"),
		Raw:        truncate(rec.Body.String(), 4000),
	}
	res.OK = rec.Code == 200
	res.Reply = extractReply(proto, stream, rec.Body.String())
	return res
}

// TestTarget sends a tiny request directly to one provider/model using the
// given upstream protocol (or the auto-selected one), bypassing breakers.
func (g *Gateway) TestTarget(ctx context.Context, p *store.Provider, model, proto string, stream bool) TestResult {
	if proto == "" {
		proto = chooseProtocol(p, model, convert.ProtoOpenAI)
	}
	c := candidate{target: p.Prefix + "/" + model, prefix: p.Prefix, model: model, provider: p, proto: proto}
	body := testBody(model, stream, "")
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")).WithContext(ctx)
	r.Header.Set("User-Agent", "ai-route-admin-test")
	rec := httptest.NewRecorder()
	start := time.Now()
	inbound := proto
	if proto == convert.ProtoResponses {
		inbound = convert.ProtoOpenAI // a chat request, converted on the way
	}
	tr := g.try(ctx, rec, r, c, inbound, body, stream, model, g.store.GetSettings())
	res := TestResult{
		LatencyMs: time.Since(start).Milliseconds(),
		Target:    c.target,
		Attempts:  []store.Attempt{tr.attempt},
		Usage:     tr.usage,
	}
	if tr.committed {
		res.HTTPStatus = rec.Code
		res.OK = tr.streamErr == ""
		res.Raw = truncate(rec.Body.String(), 4000)
		res.Reply = extractReply(inbound, stream, rec.Body.String())
	} else {
		res.HTTPStatus = tr.attempt.HTTPStatus
		res.Raw = tr.attempt.Error
	}
	return res
}

// FetchUpstreamModels lists the provider's models via GET .../models,
// trying the OpenAI endpoint first and then the Anthropic one.
func (g *Gateway) FetchUpstreamModels(ctx context.Context, p *store.Provider) ([]string, error) {
	var urls []string
	if p.OpenAIBaseURL != "" {
		urls = append(urls, strings.TrimSuffix(p.OpenAIBaseURL, "/chat/completions")+"/models")
	}
	if p.AnthropicBaseURL != "" {
		urls = append(urls, strings.TrimSuffix(anthropicURL(p.AnthropicBaseURL), "/messages")+"/models")
	}
	var lastErr error
	for _, u := range urls {
		ids, err := g.fetchModels(ctx, p, u)
		if err == nil && len(ids) > 0 {
			return ids, nil
		}
		if err == nil {
			err = errors.New("upstream returned an empty model list")
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no base URL configured")
	}
	return nil, lastErr
}

func (g *Gateway) fetchModels(ctx context.Context, p *store.Provider, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", upstreamUA(p, ""))
	applyProviderHeaders(req.Header, p, "", nil)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s: HTTP %d %s", url, resp.StatusCode, truncate(upstreamErrorMessage(b), 200))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
