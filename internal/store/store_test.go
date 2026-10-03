package store

import (
	"strings"
	"testing"
	"time"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"claude-*haiku*", "claude-3-5-haiku-20241022", true},
		{"claude-*haiku*", "claude-haiku-4-5", true},
		{"claude-*haiku*", "claude-sonnet-4-5", false},
		{"qwen*", "qwen3.7-plus", true},
		{"*-flash", "glm-5.3-flash", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"exact", "exact", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v", c.pat, c.s, got)
		}
	}
}

func TestStatsTimelineFillsGaps(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	st.insertLogs([]*RequestLog{
		{CreatedAt: now - 2*3600*1000, PublicModel: "coder", Provider: "kimi", UpstreamModel: "k", Success: true, InputTokens: 10, OutputTokens: 2},
		{CreatedAt: now, PublicModel: "coder", Provider: "glm", UpstreamModel: "g", Success: false, Fallback: true},
	})
	s, err := st.GetStats(now-24*3600*1000, 3600*1000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Requests != 2 || s.Total.Failed != 1 || s.Total.Fallback != 1 || s.Total.InputTokens != 10 {
		t.Fatalf("total: %+v", s.Total)
	}
	if n := len(s.Timeline); n < 24 || n > 26 {
		t.Fatalf("timeline buckets: %d", n)
	}
	var sum int64
	for _, r := range s.Timeline {
		sum += r.Requests
	}
	if sum != 2 || len(s.ByTarget) != 2 {
		t.Fatalf("sum=%d targets=%v", sum, s.ByTarget)
	}
}

func TestResolveModelPriority(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateModel(&Model{Name: "fast", Aliases: []string{"claude-*"}, Targets: []string{"a/x"}, Enabled: true}))
	must(st.CreateModel(&Model{Name: "coder", Aliases: []string{"claude-sonnet-4-5"}, Targets: []string{"a/y"}, Enabled: true}))
	must(st.CreateModel(&Model{Name: "off", Targets: []string{"a/z"}, Enabled: false}))
	snap := st.Snapshot()
	if m := snap.ResolveModel("claude-sonnet-4-5"); m == nil || m.Name != "coder" {
		t.Fatal("exact alias should beat glob alias")
	}
	if m := snap.ResolveModel("claude-opus-4"); m == nil || m.Name != "fast" {
		t.Fatal("glob alias")
	}
	if snap.ResolveModel("off") != nil {
		t.Fatal("disabled model resolved")
	}
	if err := st.CreateModel(&Model{Name: "bad", Targets: []string{"noslash"}}); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateProvider(&Provider{Prefix: "oc", OpenAIBaseURL: "http://a/v1", AnthropicBaseURL: "http://a", APIKey: "k", Enabled: true,
		ModelProtocols: map[string]string{"mini*": "anthropic"}, Headers: map[string]string{"X-A": "1"}}); err != nil {
		t.Fatal(err)
	}
	e, _ := st.Export()
	e.Settings = Settings{FailureThreshold: 5} // partial settings from an old export
	if err := st.Import(e); err != nil {
		t.Fatal(err)
	}
	p := st.Snapshot().Providers["oc"]
	if p == nil || p.ModelProtocols["mini*"] != "anthropic" || p.Headers["X-A"] != "1" {
		t.Fatalf("provider after import: %+v", p)
	}
	got := st.GetSettings()
	if got.FailureThreshold != 5 || got.MaxCooldownSeconds <= 0 || got.DefaultMaxTokens <= 0 {
		t.Fatalf("settings not normalized: %+v", got)
	}
}

func TestDerivePrefix(t *testing.T) {
	cases := []struct {
		openai, anthropic, name, want string
	}{
		{"https://api.deepseek.com/v1", "", "", "deepseek"},
		{"https://api.kimi.com/coding/v1", "", "", "kimi"},
		{"https://coding.dashscope.aliyuncs.com/v1", "", "", "dashscope"},
		{"https://ark.cn-beijing.volces.com/api/coding/v3", "", "", "ark"},
		{"https://open.bigmodel.cn/api/coding/paas/v4", "", "", "bigmodel"},
		{"https://openrouter.ai/api/v1", "", "", "openrouter"},
		{"", "https://api.anthropic.com", "", "anthropic"},
		{"http://127.0.0.1:9999/v1", "", "My Relay!", "my-relay"},
		{"http://localhost:8000/v1", "", "", "provider"},
		{"", "", "中文名", "provider"},
	}
	for _, c := range cases {
		if got := DerivePrefix(&Provider{OpenAIBaseURL: c.openai, AnthropicBaseURL: c.anthropic, Name: c.name}); got != c.want {
			t.Errorf("DerivePrefix(%q,%q,%q) = %q, want %q", c.openai, c.anthropic, c.name, got, c.want)
		}
	}
}

func TestCreateProviderPrefixUnique(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var got []string
	for i := 0; i < 3; i++ {
		p := &Provider{Prefix: "Kimi Code", OpenAIBaseURL: "http://x/v1", Enabled: true}
		if err := st.CreateProvider(p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p.Prefix)
	}
	if got[0] != "kimi-code" || got[1] != "kimi-code-2" || got[2] != "kimi-code-3" {
		t.Fatalf("%v", got)
	}
}

func TestAPIKeysAlwaysGenerated(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &APIKey{Name: "a", Key: "custom", Enabled: true}
	b := &APIKey{Name: "b", Enabled: true}
	if err := st.CreateKey(a); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(b); err != nil {
		t.Fatal(err)
	}
	if a.Key == "custom" || a.Key == b.Key || len(a.Key) != len("sk-route-")+48 {
		t.Fatalf("keys %q %q", a.Key, b.Key)
	}
	old := a.Key
	a.Key = "changed"
	if err := st.UpdateKey(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Snapshot().Keys[old]; !ok {
		t.Fatal("UpdateKey must not change the key value")
	}
	nv, err := st.RotateKey(a.ID)
	if err != nil || nv == old {
		t.Fatal(err)
	}
	if _, ok := st.Snapshot().Keys[old]; ok {
		t.Fatal("old key still valid after rotate")
	}
	if k := st.Snapshot().Keys[nv]; k == nil || k.Name != "a" {
		t.Fatal("rotated key not active")
	}
}

func TestModelTagsPersistAndExport(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &Model{Name: "dess", Tags: []string{"chat", " vision ", "chat", "", "ctx-1m"}, Targets: []string{"a/x"}, Enabled: true}
	if err := st.CreateModel(m); err != nil {
		t.Fatal(err)
	}
	ms, _ := st.ListModels()
	if got := strings.Join(ms[0].Tags, ","); got != "chat,vision,ctx-1m" {
		t.Fatalf("tags %q", got)
	}
	e, _ := st.Export()
	if err := st.Import(e); err != nil {
		t.Fatal(err)
	}
	ms, _ = st.ListModels()
	if got := strings.Join(ms[0].Tags, ","); got != "chat,vision,ctx-1m" {
		t.Fatalf("tags after import %q", got)
	}
}
