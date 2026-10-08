package gateway

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Regression tests for the security review.

func TestHugeModelNameIsRejectedAndNotLogged(t *testing.T) {
	h := newHarness(t)
	resp, body := h.post("/v1/chat/completions", oaReq(strings.Repeat("x", 1<<20), false))
	if resp.StatusCode != 400 || !strings.Contains(body, "model name too long") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if l := h.logs()[0]; len(l.RequestedModel) > 300 {
		t.Fatalf("logged %d bytes of model name", len(l.RequestedModel))
	}
}

func TestClientAbortStillRecordsUsage(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/slowstream")
	b, _ := json.Marshal(oaReq("coder", true))
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n') // read the first event, then hang up
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if logs := h.logs(); len(logs) > 0 {
			l := logs[0]
			if !strings.Contains(l.Error, "client disconnected") || l.InputTokens != 77 || l.OutputTokens != 20 {
				t.Fatalf("aborted stream should still be billed from the drained usage: %+v", l)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no log entry")
}

func TestRequestSpecific429DoesNotCoolProvider(t *testing.T) {
	h := newHarness(t)
	h.model("big", "oa/fail429big", "an/ok")
	h.model("short", "oa/fail429short", "an/ok")
	h.post("/v1/chat/completions", oaReq("big", false))
	h.post("/v1/chat/completions", oaReq("short", false))
	for _, s := range h.gw.Breaker.Status() {
		if s.Kind == "provider" && s.Open {
			t.Fatalf("one client's request cooled down provider %s", s.Name)
		}
	}
}

func TestKeyConcurrencyCap(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/slow")
	key := h.limitedKey(store.APIKey{MaxConcurrency: 1})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = h.postAs(key, "/v1/chat/completions", oaReq("coder", false))
		}(i)
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 429 {
		t.Fatalf("codes: %v", codes)
	}
	if code, _, _ := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
		t.Fatal("slot not released")
	}
}

func TestCountTokensIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "an/ok")
	key := h.limitedKey(store.APIKey{RPM: 1})
	h.postAs(key, "/v1/messages/count_tokens", anReq("coder", false))
	if code, _, _ := h.postAs(key, "/v1/messages/count_tokens", anReq("coder", false)); code != 429 {
		t.Fatalf("count_tokens bypassed RPM: %d", code)
	}
}

func TestClientErrorsHideUpstreamDetails(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail500")
	resp, body := h.post("/v1/chat/completions", oaReq("coder", false))
	if strings.Contains(body, "boom") || !strings.Contains(body, "oa/fail500 HTTP 500") ||
		!strings.Contains(body, resp.Header.Get("X-Route-Request-Id")) {
		t.Fatalf("client error: %s", body)
	}
	if l := h.logs()[0]; !strings.Contains(l.Attempts[0].Error, "boom") {
		t.Fatal("the log should keep the upstream detail")
	}
}

func TestAccountSelectingHeadersNotForwarded(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{
		"OpenAI-Organization": "org-other", "OpenAI-Project": "proj-other", "Api-Key": "x", "X-Goog-Api-Key": "y"})
	hdr := h.mock.lastHdr["ok"]
	for _, k := range []string{"Openai-Organization", "Openai-Project", "Api-Key", "X-Goog-Api-Key"} {
		if hdr.Get(k) != "" {
			t.Errorf("%s forwarded", k)
		}
	}
}
