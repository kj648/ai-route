package gateway

import (
	"strings"
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
	// Codex's native session-id is used as well
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"Session-Id": "codex-9"})
	if got := h.mock.lastHdr["ok"].Get("x-opencode-session"); got != "codex-9" {
		t.Fatalf("Codex session: %q", got)
	}
	// Claude Code's own session id beats the generated one
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"X-Claude-Code-Session-Id": "cc-123"})
	if got := h.mock.lastHdr["ok"].Get("x-opencode-session"); got != "cc-123" {
		t.Fatalf("Claude Code session: %q", got)
	}
	// and the caller's explicit x-opencode-session beats both
	resp, _ := h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"x-opencode-session": "conv_X", "X-Claude-Code-Session-Id": "cc-123"})
	if resp.StatusCode != 200 || h.mock.lastHdr["ok"].Get("x-opencode-session") != "conv_X" {
		t.Fatal("caller session not forwarded")
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
