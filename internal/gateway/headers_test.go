package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-route/internal/store"
	"ai-route/internal/version"
)

// Caller headers are forwarded (minus credentials and hop-by-hop / private
// ones); provider headers can be fixed, taken from the caller or generated.

func TestClientHeadersForwarded(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.Headers = map[string]string{"X-Override": "platform"} })
	h.model("coder", "oa/ok")
	resp, body := h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{
		"X-Custom": "1", "X-Stainless-Lang": "python", "X-Override": "caller",
		"Cookie": "sid=secret", "X-Forwarded-For": "10.0.0.1", "Origin": "https://evil.example", "Sec-Fetch-Mode": "cors",
		"anthropic-beta": "tools-2024", // meaningless for an OpenAI upstream
		"Connection":     "X-Hop", "X-Hop": "1", "Http2-Settings": "AAMAAABkAAQAoAAAAAIAAAAA",
	})
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	hdr := h.mock.lastHdr["ok"]
	for k, want := range map[string]string{
		"X-Custom": "1", "X-Stainless-Lang": "python", "X-Override": "platform", "Authorization": "Bearer k-oa",
		"Cookie": "", "X-Forwarded-For": "", "Origin": "", "Sec-Fetch-Mode": "", "Anthropic-Beta": "",
		"X-Hop": "", "Http2-Settings": "",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	h.setProvider("oa", func(p *store.Provider) { p.DropClientHeaders = true })
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"X-Custom": "1"})
	if h.mock.lastHdr["ok"].Get("X-Custom") != "" {
		t.Fatal("caller headers forwarded although switched off")
	}
}

func TestRequiredHeaderOnFirstPriority(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) {
		p.Headers = map[string]string{"x-opencode-session": "{{header.x-opencode-session}}"}
	})
	h.model("coder", "oa/ok", "an/ok")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 400 || !strings.Contains(body, "missing required request header: X-Opencode-Session (required by provider oa)") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if h.mock.Hits("ok") != 0 {
		t.Fatal("rejected request reached an upstream")
	}
	if l := h.logs()[0]; l.HTTPStatus != 400 || l.RequestID == "" || !strings.Contains(l.Error, "X-Opencode-Session") {
		t.Fatalf("log: %+v", l)
	}
	// even when the first priority is cooling down: the rule follows the
	// configured order, not the runtime one
	h.gw.Breaker.Failure("oa", "oa/ok", failProviderHard, 0, "quota")
	if resp, _ := h.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 400 {
		t.Fatalf("cooling primary: %d", resp.StatusCode)
	}
	h.gw.Breaker.Reset("")
	resp, _ = h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"x-opencode-session": "conv_9"})
	if resp.StatusCode != 200 || h.mock.lastHdr["ok"].Get("x-opencode-session") != "conv_9" {
		t.Fatalf("with header: %d %v", resp.StatusCode, h.mock.lastHdr["ok"])
	}
	// the admin test fills in required headers
	if r := h.gw.TestModel(t.Context(), "coder", "openai", false, ""); !r.OK {
		t.Fatalf("admin test: %+v", r)
	}
}

func TestRequiredHeaderOnFallbackIsOptional(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) {
		p.Headers = map[string]string{"x-opencode-session": "{{header.x-opencode-session}}"}
	})
	h.model("coder", "an/fail500", "oa/ok")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "oa/ok" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if v, ok := h.mock.lastHdr["ok"]["X-Opencode-Session"]; ok {
		t.Fatalf("missing header should be omitted, got %v", v)
	}
	if a := h.logs()[0].Attempts; a[len(a)-1].Headers["x-opencode-session"] != "(omitted)" {
		t.Fatalf("attempt headers: %+v", a)
	}
}

func TestGeneratedSessionHeader(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) {
		p.Headers = map[string]string{"x-opencode-session": store.OpenCodeSessionHeader, "X-Req": "{{$uuid}}"}
	})
	h.model("coder", "oa/ok")
	send := func(first string, extra ...map[string]any) (string, string) {
		req := oaReq("coder", false)
		msgs := []map[string]any{{"role": "system", "content": "s"}, {"role": "user", "content": first}}
		req["messages"] = append(msgs, extra...)
		resp, _ := h.post("/v1/chat/completions", req)
		hdr := h.mock.lastHdr["ok"]
		return hdr.Get("x-opencode-session"), hdr.Get("X-Req") + "|" + resp.Header.Get("X-Route-Request-Id")
	}
	s1, r1 := send("task A")
	s2, r2 := send("task A", map[string]any{"role": "assistant", "content": "ok"}, map[string]any{"role": "user", "content": "next"})
	s3, _ := send("task B")
	if !strings.HasPrefix(s1, "ses_") || s1 != s2 || s1 == s3 {
		t.Fatalf("sessions: %s %s %s", s1, s2, s3)
	}
	if r1 == r2 {
		t.Fatal("$uuid and request ids must differ per request")
	}
	l := h.logs()[0]
	if l.Attempts[0].Headers["x-opencode-session"] != s3 || !strings.HasPrefix(l.RequestID, "req_") {
		t.Fatalf("log: %+v", l)
	}
	// a caller's session id gives the same $session whatever the messages,
	// and the raw id never reaches the upstream
	sessionOf := func(hdr map[string]string, first string) string {
		req := oaReq("coder", false)
		req["messages"] = []map[string]any{{"role": "user", "content": first}}
		resp, _ := h.postWith("/v1/chat/completions", req, hdr)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		got := h.mock.lastHdr["ok"]
		if got.Get("X-Session-Id") != "" {
			t.Fatal("X-Session-Id forwarded upstream")
		}
		return got.Get("x-opencode-session")
	}
	job1 := sessionOf(map[string]string{"X-Session-Id": "job-1"}, "outline")
	if job1 == "job-1" || !strings.HasPrefix(job1, "ses_") || job1 == s3 {
		t.Fatalf("job session: %q", job1)
	}
	if sessionOf(map[string]string{"X-Session-Id": "job-1"}, "write page 2") != job1 {
		t.Fatal("one session id must keep one $session across different prompts")
	}
	if sessionOf(map[string]string{"X-Session-Id": "job-2"}, "outline") == job1 {
		t.Fatal("different session ids must differ")
	}
	// clients' native session headers count too; X-Session-Id wins over them
	cc := sessionOf(map[string]string{"X-Claude-Code-Session-Id": "cc-123"}, "x")
	codex := sessionOf(map[string]string{"Session-Id": "codex-9"}, "x")
	oc := sessionOf(map[string]string{"x-opencode-session": "conv_X"}, "x")
	if cc == codex || cc == oc || !strings.HasPrefix(cc, "ses_") || oc == "conv_X" {
		t.Fatalf("native sessions: %q %q %q", cc, codex, oc)
	}
	if sessionOf(map[string]string{"X-Session-Id": "job-1", "X-Claude-Code-Session-Id": "cc-123"}, "y") != job1 {
		t.Fatal("X-Session-Id must win over native session headers")
	}
	// the log keeps the caller's own id, and can be filtered by it
	h.logs() // flush
	logs, total, err := h.st.QueryLogs(store.LogQuery{Session: "job-1"})
	if err != nil || total != 3 || logs[0].SessionID != "job-1" {
		t.Fatalf("session logs: %d %v", total, err)
	}
}

// The same session id sent with two API keys is two sessions.
func TestSessionScopedToKey(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) {
		p.Headers = map[string]string{"x-opencode-session": store.OpenCodeSessionHeader}
	})
	h.model("coder", "oa/ok")
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"X-Session-Id": "job-1"})
	a := h.mock.lastHdr["ok"].Get("x-opencode-session")
	other := &store.APIKey{Name: "other", Enabled: true}
	if err := h.st.CreateKey(other); err != nil {
		t.Fatal(err)
	}
	h.key = other.Key
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"X-Session-Id": "job-1"})
	if b := h.mock.lastHdr["ok"].Get("x-opencode-session"); a == "" || a == b {
		t.Fatalf("sessions across keys: %q %q", a, b)
	}
}

func TestPlatformUserAgent(t *testing.T) {
	h := newHarness(t)
	h.setProvider("oa", func(p *store.Provider) { p.UAMode = "platform" })
	h.model("coder", "oa/ok")
	h.post("/v1/chat/completions", oaReq("coder", false)) // harness sends claude-cli/9.9
	if got := h.mock.lastHdr["ok"].Get("User-Agent"); got != version.UserAgent() || !strings.HasPrefix(got, "ai-route/") {
		t.Fatalf("UA %q", got)
	}
}

// The gateway's own upstream calls (admin "test" buttons, health checks)
// and requests without a user message still send a session header:
// OpenCode Go rejects requests without one.
func TestSessionHeaderOnGatewayOwnCalls(t *testing.T) {
	h := newHarness(t)
	var probeSession atomic.Value
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeSession.Store(r.Header.Get("x-opencode-session"))
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(hc.Close)
	h.setProvider("oa", func(p *store.Provider) {
		p.Headers = map[string]string{"x-opencode-session": store.OpenCodeSessionHeader}
		p.HealthCheckSeconds, p.HealthCheckURL = 10, hc.URL
	})
	h.model("coder", "oa/ok")
	p := h.st.Snapshot().Providers["oa"]

	// "test" on the provider page
	res := h.gw.TestTarget(context.Background(), p, "ok", "", false)
	s1 := h.mock.lastHdr["ok"].Get("x-opencode-session")
	if !res.OK || !strings.HasPrefix(s1, "ses_") {
		t.Fatalf("provider test: ok=%v session=%q", res.OK, s1)
	}
	// "test" on the model page
	if res := h.gw.TestModel(context.Background(), "coder", "openai", false, ""); !res.OK || !strings.HasPrefix(h.mock.lastHdr["ok"].Get("x-opencode-session"), "ses_") {
		t.Fatalf("model test: %+v", res)
	}
	// health check
	h.gw.Health.CheckDue(context.Background())
	if s, _ := probeSession.Load().(string); !strings.HasPrefix(s, "ses_") {
		t.Fatalf("health check session: %q", s)
	}
	// a request with no user message gets a fresh id each time
	req := oaReq("coder", false)
	req["messages"] = []map[string]any{{"role": "system", "content": "only a system prompt"}}
	h.post("/v1/chat/completions", req)
	a := h.mock.lastHdr["ok"].Get("x-opencode-session")
	h.post("/v1/chat/completions", req)
	b := h.mock.lastHdr["ok"].Get("x-opencode-session")
	if !strings.HasPrefix(a, "ses_") || !strings.HasPrefix(b, "ses_") || a == b {
		t.Fatalf("no-user-message sessions: %q %q", a, b)
	}
}
