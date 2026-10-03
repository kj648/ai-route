package gateway

import (
	"strings"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Model-mapping behaviour: how a requested model name resolves to a public
// model, and how its targets are ordered, skipped and replaced.

func attemptTargets(l *store.RequestLog) string {
	var parts []string
	for _, a := range l.Attempts {
		p := a.Target
		if a.Retry > 0 {
			p += "#" + string(rune('0'+a.Retry))
		}
		if a.Cooling {
			p += "(cooling)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

func TestMappingNameResolution(t *testing.T) {
	h := newHarness(t)
	mustNil(t, h.st.CreateModel(&store.Model{Name: "dess", Targets: []string{"oa/up-dess"}, Enabled: true}))
	mustNil(t, h.st.CreateModel(&store.Model{Name: "fast", Aliases: []string{"claude-*haiku*", "gpt-mini"}, Targets: []string{"oa/up-fast"}, Enabled: true}))
	mustNil(t, h.st.CreateModel(&store.Model{Name: "coder", Aliases: []string{"claude-haiku-special"}, Targets: []string{"oa/up-coder"}, Enabled: true}))
	mustNil(t, h.st.CreateModel(&store.Model{Name: "retired", Aliases: []string{"old-*"}, Targets: []string{"oa/up-retired"}, Enabled: false}))

	cases := []struct {
		requested  string
		wantStatus int
		wantPublic string
		wantUp     string
	}{
		{"dess", 200, "dess", "up-dess"},                      // exact name
		{"gpt-mini", 200, "fast", "up-fast"},                  // exact alias
		{"claude-3-5-haiku-20241022", 200, "fast", "up-fast"}, // glob alias
		{"claude-haiku-special", 200, "coder", "up-coder"},    // exact alias beats glob
		{"retired", 404, "", ""},                              // disabled model
		{"old-model", 404, "", ""},                            // alias of disabled model
		{"kimi/k3", 404, "", ""},                              // raw upstream names are not exposed
		{"DESS", 404, "", ""},                                 // names are case-sensitive
	}
	for _, c := range cases {
		resp, body := h.post("/v1/chat/completions", oaReq(c.requested, false))
		if resp.StatusCode != c.wantStatus {
			t.Errorf("%s: status %d, want %d (%s)", c.requested, resp.StatusCode, c.wantStatus, body)
			continue
		}
		if c.wantUp != "" {
			if got := h.mock.Body(c.wantUp)["model"]; got != c.wantUp {
				t.Errorf("%s: upstream got model %v, want %s", c.requested, got, c.wantUp)
			}
			if !strings.Contains(body, "hello from "+c.wantUp) {
				t.Errorf("%s: unexpected body %s", c.requested, body)
			}
		}
	}
	logs := h.logs()
	if len(logs) != len(cases) {
		t.Fatalf("want %d logs, got %d", len(cases), len(logs))
	}
	// logs are newest first
	for i, c := range cases {
		l := logs[len(cases)-1-i]
		if l.RequestedModel != c.requested || l.PublicModel != c.wantPublic {
			t.Errorf("log %d: requested=%q public=%q, want %q/%q", i, l.RequestedModel, l.PublicModel, c.requested, c.wantPublic)
		}
	}
}

func TestMappingOrderSkipsUnusableTargets(t *testing.T) {
	h := newHarness(t)
	// missing prefix and disabled provider are skipped without an attempt
	h.model("dess", "missing/k3", "off/k3", "oa/first", "an/second")
	resp, _ := h.post("/v1/chat/completions", oaReq("dess", false))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "oa/first" {
		t.Fatalf("status %d target %s", resp.StatusCode, resp.Header.Get("X-Route-Target"))
	}
	l := h.logs()[0]
	if got := attemptTargets(l); got != "oa/first" {
		t.Fatalf("attempts: %s", got)
	}
	if l.Fallback {
		t.Fatal("first usable target is not a fallback")
	}
}

func TestMappingFallbackChainOrder(t *testing.T) {
	h := newHarness(t)
	// 500 is retried (2 retries), a long 429 and a 400 switch immediately
	h.model("dess", "oa/fail500", "an/fail429", "both/fail400", "oa/ok")
	resp, body := h.post("/v1/messages", anReq("dess", false))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "oa/ok" {
		t.Fatalf("status %d target %s: %s", resp.StatusCode, resp.Header.Get("X-Route-Target"), body)
	}
	l := h.logs()[0]
	want := "oa/fail500 oa/fail500#1 oa/fail500#2 an/fail429 both/fail400 oa/ok"
	if got := attemptTargets(l); got != want {
		t.Fatalf("attempts:\n got  %s\n want %s", got, want)
	}
	if !l.Fallback || l.Provider != "oa" || l.UpstreamModel != "ok" || l.UpstreamProtocol != "openai" || l.Inbound != "anthropic" {
		t.Fatalf("log: %+v", l)
	}
}

func TestMappingCoolingTargetsGoLast(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "an/fail429", "oa/ok")
	h.post("/v1/chat/completions", oaReq("dess", false)) // an cools down (Retry-After 120)

	// healthy target first now, cooling one is not touched
	h.post("/v1/chat/completions", oaReq("dess", false))
	if got := attemptTargets(h.logs()[0]); got != "oa/ok" {
		t.Fatalf("attempts after cooldown: %s", got)
	}

	// if the healthy one fails too, the cooling one is tried last, once, no retries
	mustNil(t, h.st.UpdateModel(&store.Model{ID: 1, Name: "dess", Targets: []string{"an/fail429", "oa/fail500"}, Enabled: true}))
	h.gw.Breaker.Reset("t:oa/fail500")
	h.post("/v1/chat/completions", oaReq("dess", false))
	if got := attemptTargets(h.logs()[0]); got != "oa/fail500 oa/fail500#1 oa/fail500#2 an/fail429(cooling)" {
		t.Fatalf("attempts with everything failing: %s", got)
	}
}

func TestMappingChangesApplyImmediately(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/v1-model")
	h.post("/v1/chat/completions", oaReq("dess", false))
	ms, _ := h.st.ListModels()
	m := ms[0]
	m.Targets = []string{"an/v2-model", "oa/v1-model"}
	mustNil(t, h.st.UpdateModel(m))
	resp, _ := h.post("/v1/chat/completions", oaReq("dess", false))
	if resp.Header.Get("X-Route-Target") != "an/v2-model" {
		t.Fatalf("new mapping not used: %s", resp.Header.Get("X-Route-Target"))
	}
	m.Enabled = false
	mustNil(t, h.st.UpdateModel(m))
	if resp, _ := h.post("/v1/chat/completions", oaReq("dess", false)); resp.StatusCode != 404 {
		t.Fatalf("disabled mapping still served: %d", resp.StatusCode)
	}
}

func TestMappingKeyRestrictions(t *testing.T) {
	h := newHarness(t)
	h.model("dess", "oa/ok")
	h.model("secret", "oa/ok")
	mustNil(t, h.st.CreateModel(&store.Model{Name: "fast", Aliases: []string{"claude-*haiku*"}, Targets: []string{"oa/ok"}, Enabled: true}))
	limited := &store.APIKey{Name: "limited", Enabled: true, AllowedModels: []string{"dess", "fast"}}
	mustNil(t, h.st.CreateKey(limited))
	disabled := &store.APIKey{Name: "disabled", Enabled: false}
	mustNil(t, h.st.CreateKey(disabled))
	expired := &store.APIKey{Name: "expired", Enabled: true, ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	mustNil(t, h.st.CreateKey(expired))

	cases := []struct {
		key, model string
		want       int
	}{
		{limited.Key, "dess", 200},
		{limited.Key, "claude-haiku-4-5", 200}, // alias resolves to an allowed public model
		{limited.Key, "secret", 403},
		{disabled.Key, "dess", 401},
		{expired.Key, "dess", 401},
		{"", "dess", 401},
	}
	for _, c := range cases {
		hdr := map[string]string{"Authorization": "Bearer " + c.key}
		if c.key == "" {
			hdr["Authorization"] = ""
		}
		if resp, body := h.postWith("/v1/chat/completions", oaReq(c.model, false), hdr); resp.StatusCode != c.want {
			t.Errorf("key %s model %s: got %d want %d (%s)", c.key, c.model, resp.StatusCode, c.want, body)
		}
	}
}
