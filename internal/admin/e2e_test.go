package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"ai-route/internal/gateway"
	"ai-route/internal/store"
)

// End-to-end through the HTTP APIs: configure providers, a mapping and a key
// via the admin API, call the public API, then read logs and stats back.

type e2e struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newE2E(t *testing.T) *e2e {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	settings := store.DefaultSettings()
	settings.RetryBackoffMs = 5
	if err := st.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st)
	mux := http.NewServeMux()
	gw.Register(mux)
	New(st, gw, "tok", fstest.MapFS{"index.html": {Data: []byte("x")}}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &e2e{t: t, srv: srv, st: st}
}

func (e *e2e) do(method, path, auth string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func (e *e2e) admin(method, path string, body any, out any) int {
	e.t.Helper()
	return e.do(method, "/admin/api"+path, "tok", body, out)
}

// upstream answers in OpenAI format; model "down" always fails.
func newUpstream(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "k3"}, {"id": "down"}}})
			return
		}
		if req.Model == "down" {
			http.Error(w, `{"error":{"message":"upstream down"}}`, 503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi from " + req.Model}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 3},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestE2EMappingAndLogs(t *testing.T) {
	e := newE2E(t)
	up := newUpstream(t)

	// providers: prefix generated from the preset value sent by the UI, deduplicated
	var p1, p2 struct {
		ID     int64  `json:"id"`
		Prefix string `json:"prefix"`
	}
	if c := e.admin("POST", "/providers", map[string]any{"prefix": "kimi", "name": "Kimi A", "openai_base_url": up.URL + "/v1", "api_key": "k1", "enabled": true}, &p1); c != 200 {
		t.Fatalf("create provider: %d", c)
	}
	e.admin("POST", "/providers", map[string]any{"prefix": "kimi", "name": "Kimi B", "openai_base_url": up.URL + "/v1", "api_key": "k2", "enabled": true}, &p2)
	if p1.Prefix != "kimi" || p2.Prefix != "kimi-2" {
		t.Fatalf("prefixes %q %q", p1.Prefix, p2.Prefix)
	}
	// model list fetched automatically on create
	var ps []map[string]any
	e.admin("GET", "/providers", nil, &ps)
	if models, _ := json.Marshal(ps[0]["models"]); string(models) != `["down","k3"]` {
		t.Fatalf("auto-fetched models: %s", models)
	}

	// mapping: first target always fails -> falls back to kimi-2/k3
	if c := e.admin("POST", "/models", map[string]any{"name": "dess", "targets": []string{"kimi/down", "kimi-2/k3"}, "enabled": true}, nil); c != 200 {
		t.Fatalf("create model: %d", c)
	}

	// key: custom value is ignored, a key is generated
	var key store.APIKey
	e.admin("POST", "/keys", map[string]any{"name": "alice", "key": "my-custom-key", "enabled": true}, &key)
	if key.Key == "my-custom-key" || !strings.HasPrefix(key.Key, "sk-route-") || len(key.Key) < 40 {
		t.Fatalf("key not generated: %q", key.Key)
	}

	// call the public API
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if c := e.do("POST", "/v1/chat/completions", key.Key, map[string]any{"model": "dess", "messages": []map[string]any{{"role": "user", "content": "hi"}}}, &chat); c != 200 {
		t.Fatalf("chat: %d", c)
	}
	if chat.Choices[0].Message.Content != "hi from k3" {
		t.Fatalf("reply %+v", chat)
	}
	if c := e.do("POST", "/v1/chat/completions", key.Key, map[string]any{"model": "nope", "messages": []map[string]any{}}, nil); c != 404 {
		t.Fatalf("unknown model: %d", c)
	}
	e.st.FlushLogs()

	// logs via the admin API, with filters
	var logs struct {
		Total int64               `json:"total"`
		Items []*store.RequestLog `json:"items"`
	}
	e.admin("GET", "/logs", nil, &logs)
	if logs.Total != 2 {
		t.Fatalf("logs total %d", logs.Total)
	}
	e.admin("GET", "/logs?"+url.Values{"model": {"dess"}, "fallback": {"1"}}.Encode(), nil, &logs)
	if logs.Total != 1 {
		t.Fatalf("filtered logs %d", logs.Total)
	}
	l := logs.Items[0]
	if l.KeyName != "alice" || l.Provider != "kimi-2" || l.UpstreamModel != "k3" || !l.Fallback || !l.Success ||
		l.InputTokens != 11 || len(l.Attempts) != 4 || l.Attempts[0].HTTPStatus != 503 || l.Attempts[2].Retry != 2 {
		t.Fatalf("log entry: %+v", l)
	}
	e.admin("GET", "/logs?status=failed", nil, &logs)
	if logs.Total != 1 || logs.Items[0].RequestedModel != "nope" {
		t.Fatalf("failed logs: %+v", logs)
	}

	// stats via the admin API
	var st store.Stats
	e.admin("GET", "/stats?range=1h", nil, &st)
	if st.Total.Requests != 2 || st.Total.Fallback != 1 || st.Total.InputTokens != 11 || len(st.Timeline) < 12 {
		t.Fatalf("stats: %+v (timeline %d)", st.Total, len(st.Timeline))
	}

	// rotating the key: old value stops working, new one works
	var rotated struct {
		Key string `json:"key"`
	}
	e.admin("POST", "/keys/1/rotate", nil, &rotated)
	if rotated.Key == "" || rotated.Key == key.Key {
		t.Fatalf("rotate: %+v", rotated)
	}
	body := map[string]any{"model": "dess", "messages": []map[string]any{{"role": "user", "content": "hi"}}}
	if c := e.do("POST", "/v1/chat/completions", key.Key, body, nil); c != 401 {
		t.Fatalf("old key still works: %d", c)
	}
	if c := e.do("POST", "/v1/chat/completions", rotated.Key, body, nil); c != 200 {
		t.Fatalf("new key rejected: %d", c)
	}
	// editing a key never changes its value
	e.admin("PUT", "/keys/1", map[string]any{"name": "alice2", "key": "hijack", "enabled": true}, nil)
	if c := e.do("POST", "/v1/chat/completions", rotated.Key, body, nil); c != 200 {
		t.Fatalf("key changed by update: %d", c)
	}
}

func TestE2EPrefixDerivedWhenEmpty(t *testing.T) {
	e := newE2E(t)
	var out struct {
		Prefix string `json:"prefix"`
	}
	e.admin("POST", "/providers", map[string]any{"name": "My Relay", "openai_base_url": "https://api.deepseek.com/v1", "enabled": true}, &out)
	if out.Prefix != "deepseek" {
		t.Fatalf("prefix %q", out.Prefix)
	}
	e.admin("POST", "/providers", map[string]any{"name": "DS backup", "anthropic_base_url": "https://api.deepseek.com/anthropic", "enabled": true}, &out)
	if out.Prefix != "deepseek-2" {
		t.Fatalf("prefix %q", out.Prefix)
	}
}
