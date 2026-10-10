package convert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func randID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ExtractUsage reads token usage from a non-stream response in its own protocol.
func ExtractUsage(body []byte, proto string) Usage {
	if proto == ProtoRerank {
		return rerankUsage(body)
	}
	if proto == ProtoResponses {
		var r struct {
			Usage *rsUsage `json:"usage"`
		}
		_ = json.Unmarshal(body, &r)
		return r.Usage.toUsage()
	}
	if proto == ProtoAnthropic {
		var r struct {
			Usage ANUsage `json:"usage"`
		}
		_ = json.Unmarshal(body, &r)
		return r.Usage.toUsage()
	}
	var r struct {
		Usage *OAUsage `json:"usage"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Usage.toUsage()
}

// rerankUsage reads the input tokens of a rerank response: usage.total_tokens
// (Jina, vLLM), meta.tokens.input_tokens (SiliconFlow) or
// meta.billed_units.input_tokens (Cohere).
func rerankUsage(body []byte) Usage {
	var r struct {
		Usage *struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
		Meta *struct {
			Tokens *struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"tokens"`
			BilledUnits *struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"billed_units"`
		} `json:"meta"`
	}
	_ = json.Unmarshal(body, &r)
	switch {
	case r.Usage != nil && r.Usage.PromptTokens > 0:
		return Usage{Input: r.Usage.PromptTokens}
	case r.Usage != nil:
		return Usage{Input: r.Usage.TotalTokens}
	case r.Meta != nil && r.Meta.Tokens != nil:
		return Usage{Input: r.Meta.Tokens.InputTokens}
	case r.Meta != nil && r.Meta.BilledUnits != nil:
		return Usage{Input: r.Meta.BilledUnits.InputTokens}
	}
	return Usage{}
}

// ValidateResponse checks that a 2xx non-stream body is a real completion and
// not an error disguised as success (some providers do that).
func ValidateResponse(body []byte, proto string) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	if e, ok := probe["error"]; ok && !isNullOrEmpty(e) {
		return fmt.Errorf("upstream error: %s", truncate(string(e), 500))
	}
	if proto == ProtoAnthropic {
		if string(probe["type"]) == `"error"` {
			return fmt.Errorf("upstream error: %s", truncate(string(body), 500))
		}
		if _, ok := probe["content"]; !ok {
			return fmt.Errorf("response has no content: %s", truncate(string(body), 300))
		}
		return nil
	}
	if proto == ProtoResponses {
		if _, ok := probe["output"]; !ok {
			return fmt.Errorf("response has no output: %s", truncate(string(body), 300))
		}
		if string(probe["status"]) == `"failed"` {
			return fmt.Errorf("upstream response failed: %s", truncate(string(body), 500))
		}
		return nil
	}
	if proto == ProtoRerank {
		if _, ok := probe["results"]; !ok {
			return fmt.Errorf("response has no results: %s", truncate(string(body), 300))
		}
		return nil
	}
	if proto == ProtoEmbeddings {
		if _, ok := probe["data"]; !ok {
			return fmt.Errorf("response has no data: %s", truncate(string(body), 300))
		}
		return nil
	}
	if _, ok := probe["choices"]; !ok {
		return fmt.Errorf("response has no choices: %s", truncate(string(body), 300))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func anStopToOpenAI(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func oaUsageFrom(u Usage) map[string]any {
	m := map[string]any{
		"prompt_tokens":     u.Input,
		"completion_tokens": u.Output,
		"total_tokens":      u.Input + u.Output,
	}
	if u.Cached > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	return m
}

func anUsageFrom(u Usage) map[string]any {
	m := map[string]any{
		"input_tokens":  u.Input - u.Cached,
		"output_tokens": u.Output,
	}
	if u.Cached > 0 {
		m["cache_read_input_tokens"] = u.Cached
	}
	return m
}

// ErrorBody builds an error response body in the client's protocol.
func ErrorBody(proto string, status int, message string) []byte {
	if proto == ProtoAnthropic {
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anErrorType(status), "message": message},
		})
		return b
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": oaErrorType(status), "code": status},
	})
	return b
}

func anErrorType(status int) string {
	switch {
	case status == 400:
		return "invalid_request_error"
	case status == 401:
		return "authentication_error"
	case status == 402:
		return "billing_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func oaErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 402:
		return "insufficient_quota"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}
