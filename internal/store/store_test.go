package store

import (
	"encoding/json"
	"math"
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

func TestPriceForAndCost(t *testing.T) {
	cache := 0.1
	p := &Provider{Prices: map[string]Price{
		"glm-5":   {Input: 4, Cache: &cache, Output: 16},
		"glm-*":   {Input: 2, Output: 8},
		"glm-5-*": {Input: 3, Output: 12},
		"*":       {Input: 1, Output: 1},
	}}
	for model, want := range map[string]float64{"glm-5": 4, "glm-5-air": 3, "glm-4": 2, "kimi": 1} {
		if got, ok := p.PriceFor(model); !ok || got.Input != want {
			t.Errorf("PriceFor(%q) = %+v, %v; want input %v", model, got, ok, want)
		}
	}
	if _, ok := (&Provider{}).PriceFor("x"); ok {
		t.Error("no prices should mean no price")
	}
	// 1M input of which 400k cached, 100k output
	if got := p.Prices["glm-5"].Cost(1_000_000, 400_000, 100_000); math.Abs(got-(0.6*4+0.4*0.1+0.1*16)) > 1e-9 {
		t.Errorf("cost with cache price: %v", got)
	}
	if got := p.Prices["glm-*"].Cost(1_000_000, 400_000, 0); math.Abs(got-2) > 1e-9 {
		t.Errorf("cached tokens without cache price bill as input: %v", got)
	}
}

func TestProviderPricesPersist(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bad := []*Provider{
		{Prefix: "a", OpenAIBaseURL: "http://a/v1", Currency: "EUR"},
		{Prefix: "b", OpenAIBaseURL: "http://a/v1", Prices: map[string]Price{"m": {Input: -1}}},
	}
	for _, p := range bad {
		if err := st.CreateProvider(p); err == nil {
			t.Errorf("accepted invalid provider %+v", p)
		}
	}
	cache := 0.2
	p := &Provider{Prefix: "ds", OpenAIBaseURL: "http://a/v1", Currency: "usd", Enabled: true,
		Prices: map[string]Price{" deepseek-* ": {Input: 2, Cache: &cache, Output: 8}}}
	mustNil(t, st.CreateProvider(p))
	got := st.Snapshot().Providers["ds"]
	if got.Currency != CurrencyUSD || got.Prices["deepseek-*"].Cache == nil || *got.Prices["deepseek-*"].Cache != 0.2 {
		t.Fatalf("after create: %+v", got)
	}
	plain := &Provider{Prefix: "plan", OpenAIBaseURL: "http://b/v1", Enabled: true}
	mustNil(t, st.CreateProvider(plain))
	if g := st.Snapshot().Providers["plan"]; g.Currency != CurrencyCNY || g.Prices == nil || len(g.Prices) != 0 {
		t.Fatalf("defaults: %+v", g)
	}
	e, _ := st.Export()
	mustNil(t, st.Import(e))
	if g := st.Snapshot().Providers["ds"]; g.Currency != CurrencyUSD || g.Prices["deepseek-*"].Output != 8 {
		t.Fatalf("after import: %+v", g)
	}
}

func TestStatsCostInDisplayCurrency(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	st.insertLogs([]*RequestLog{
		{CreatedAt: now, KeyName: "a", Success: true, InputTokens: 10, Cost: 1, Currency: "USD", CostSource: "upstream"},
		{CreatedAt: now, KeyName: "a", Success: true, InputTokens: 10, Cost: 7, Currency: "CNY", CostSource: "price"},
		{CreatedAt: now, KeyName: "b", Success: true, InputTokens: 10},  // no price configured
		{CreatedAt: now, KeyName: "b", Success: false, HTTPStatus: 502}, // no tokens: not "unpriced"
	})
	settings := DefaultSettings()
	settings.USDToCNY = 7
	for _, c := range []struct {
		currency string
		want     float64
	}{{"CNY", 14}, {"USD", 2}} {
		settings.Currency = c.currency
		mustNil(t, st.UpdateSettings(settings))
		s, err := st.GetStats(now-3600*1000, 3600*1000)
		if err != nil {
			t.Fatal(err)
		}
		if s.Currency != c.currency || math.Abs(s.Total.Cost-c.want) > 1e-9 || s.Total.Unpriced != 1 {
			t.Fatalf("%s: currency=%s total=%+v", c.currency, s.Currency, s.Total)
		}
		byKey := map[string]StatRow{}
		for _, r := range s.ByKey {
			byKey[r.Key] = r
		}
		if math.Abs(byKey["a"].Cost-c.want) > 1e-9 || byKey["a"].Unpriced != 0 || byKey["b"].Cost != 0 || byKey["b"].Unpriced != 1 {
			t.Fatalf("by key: %+v", s.ByKey)
		}
	}
}

func TestCostColumnsAddedToOldDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a database created before cost accounting existed
	for _, q := range []string{
		`ALTER TABLE request_logs DROP COLUMN cost`, `ALTER TABLE request_logs DROP COLUMN currency`, `ALTER TABLE request_logs DROP COLUMN cost_source`,
		`ALTER TABLE providers DROP COLUMN prices`, `ALTER TABLE providers DROP COLUMN currency`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustNil(t, st.CreateProvider(&Provider{Prefix: "x", OpenAIBaseURL: "http://a/v1", Prices: map[string]Price{"m": {Input: 1, Output: 1}}}))
	st.AddLog(&RequestLog{CreatedAt: time.Now().UnixMilli(), Cost: 0.5, Currency: "CNY", CostSource: "price"})
	st.FlushLogs()
	logs, _, err := st.QueryLogs(LogQuery{})
	if err != nil || len(logs) != 1 || logs[0].Cost != 0.5 {
		t.Fatalf("logs after migration: %v %+v", err, logs)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeyLimitsPersist(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateKey(&APIKey{Name: "x", RPM: -1}); err == nil {
		t.Fatal("negative rpm accepted")
	}
	k := &APIKey{Name: "x", Enabled: true, MonthlyBudget: 12.5, RPM: 30, TPM: 100000}
	mustNil(t, st.CreateKey(k))
	got := st.Snapshot().Keys[k.Key]
	if got.MonthlyBudget != 12.5 || got.RPM != 30 || got.TPM != 100000 {
		t.Fatalf("after create: %+v", got)
	}
	k.RPM, k.MonthlyBudget = 0, 0
	mustNil(t, st.UpdateKey(k))
	if got := st.Snapshot().Keys[k.Key]; got.RPM != 0 || got.TPM != 100000 || got.MonthlyBudget != 0 {
		t.Fatalf("after update: %+v", got)
	}
	e, _ := st.Export()
	mustNil(t, st.Import(e))
	if got := st.Snapshot().Keys[k.Key]; got == nil || got.TPM != 100000 {
		t.Fatalf("after import: %+v", got)
	}
}

func TestKeySpend(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	month := MonthStart(now)
	if d := time.UnixMilli(month); d.Day() != 1 || d.Hour() != 0 || d.After(now) {
		t.Fatalf("MonthStart: %v", d)
	}
	st.insertLogs([]*RequestLog{
		{CreatedAt: now.UnixMilli(), KeyID: 1, Cost: 1, Currency: "USD"},
		{CreatedAt: now.UnixMilli(), KeyID: 1, Cost: 2, Currency: "CNY"},
		{CreatedAt: month - 1, KeyID: 1, Cost: 100, Currency: "CNY"}, // last month
		{CreatedAt: now.UnixMilli(), KeyID: 2, Cost: 3, Currency: "CNY"},
	})
	got, err := st.KeySpend(month)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got[1]-9.2) > 1e-9 || got[2] != 3 { // 1 USD * 7.2 + 2
		t.Fatalf("spend: %v", got)
	}
}

func TestAlertConfigPersistAndImport(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if d := st.GetAlerts(); !d.OnAuthFailure || d.SilenceMinutes != 30 || d.Webhooks == nil {
		t.Fatalf("defaults: %+v", d)
	}
	for _, bad := range []Webhook{{Type: "slack", URL: "https://x"}, {Type: "feishu", URL: "ftp://x"}} {
		if err := st.UpdateAlerts(AlertConfig{Webhooks: []Webhook{bad}}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	c := AlertConfig{OnAllFailed: true, Webhooks: []Webhook{
		{Type: " Feishu ", URL: " https://open.feishu.cn/open-apis/bot/v2/hook/x ", Secret: "s", Enabled: true},
		{Type: "wecom", URL: ""}, // empty rows are dropped
	}}
	mustNil(t, st.UpdateAlerts(c))
	got := st.GetAlerts()
	if len(got.Webhooks) != 1 || got.Webhooks[0].Type != "feishu" || got.Webhooks[0].URL != "https://open.feishu.cn/open-apis/bot/v2/hook/x" ||
		got.OnAuthFailure || !got.OnAllFailed || got.LongCooldownMinutes != 10 {
		t.Fatalf("saved: %+v", got)
	}

	e, _ := st.Export()
	if e.Alerts == nil || len(e.Alerts.Webhooks) != 1 {
		t.Fatalf("export: %+v", e.Alerts)
	}
	e.Alerts = nil // an export from before alerts existed
	mustNil(t, st.Import(e))
	if len(st.GetAlerts().Webhooks) != 1 {
		t.Fatal("import without alerts dropped the current alert config")
	}
}

func TestBodyRules(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, bad := range []BodyRule{
		{Model: "", Set: json.RawMessage(`{}`)},
		{Model: "m", When: "sometimes", Set: json.RawMessage(`{}`)},
		{Model: "m", Protocol: "grpc", Set: json.RawMessage(`{}`)},
		{Model: "m", Set: json.RawMessage(`[1]`)},
	} {
		if err := st.CreateProvider(&Provider{Prefix: "x", OpenAIBaseURL: "http://a/v1", BodyRules: []BodyRule{bad}}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	p := &Provider{Prefix: "bl", OpenAIBaseURL: "http://a/v1", AnthropicBaseURL: "http://a", Enabled: true, BodyRules: []BodyRule{
		{Model: "qwen3-*", When: "nonstream", Protocol: "openai", Set: json.RawMessage(`{"enable_thinking":false}`)},
		{Model: "qwen3-32b", Set: json.RawMessage(`{"top_k":20}`)},
	}}
	mustNil(t, st.CreateProvider(p))
	got := st.Snapshot().Providers["bl"]
	names := func(rs []BodyRule) (out []string) {
		for _, r := range rs {
			out = append(out, string(r.Set))
		}
		return
	}
	if r := names(got.RulesFor("qwen3-32b", "openai", false)); len(r) != 2 || r[0] != `{"enable_thinking":false}` {
		t.Fatalf("non-stream openai: %v", r)
	}
	if r := got.RulesFor("qwen3-32b", "openai", true); len(r) != 1 {
		t.Fatalf("stream: %v", names(r))
	}
	if r := got.RulesFor("qwen3-8b", "anthropic", false); len(r) != 0 {
		t.Fatalf("anthropic: %v", names(r))
	}
	if r := got.RulesFor("qwen-plus", "openai", false); len(r) != 0 {
		t.Fatalf("no match: %v", names(r))
	}
}

func TestTargetGroups(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, bad := range []string{"kimi", "kimi/k3*0 | glm/g", "kimi/k3*5000", " | "} {
		if err := st.CreateModel(&Model{Name: "x", Targets: []string{bad}}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	mustNil(t, st.CreateProvider(&Provider{Prefix: "kimi", OpenAIBaseURL: "http://a/v1"}))
	m := &Model{Name: "coder", Enabled: true, Targets: []string{" kimi/k3 *3|glm/g*1 | kimi/k3 ", "glm/g", "vertex/claude-opus-4-5@20251101"}}
	mustNil(t, st.CreateModel(m))
	if got := st.Snapshot().ResolveModel("coder").Targets; len(got) != 3 || got[0] != "kimi/k3*3 | glm/g" {
		t.Fatalf("normalized: %q", got)
	}
	if ms := ParseTargetEntry("a/m*2 | b/n"); len(ms) != 2 || ms[0] != (TargetMember{"a/m", 2}) || ms[1] != (TargetMember{"b/n", 1}) {
		t.Fatalf("parse: %+v", ms)
	}
	// renaming a prefix rewrites group members too
	p := st.Snapshot().Providers["kimi"]
	p.Prefix = "moon"
	mustNil(t, st.UpdateProvider(p))
	if got := st.Snapshot().ResolveModel("coder").Targets[0]; got != "moon/k3*3 | glm/g" {
		t.Fatalf("after rename: %q", got)
	}
}

func TestImportKeepsKeyIDs(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b := &APIKey{Name: "a", Enabled: true}, &APIKey{Name: "b", Enabled: true}
	mustNil(t, st.CreateKey(a))
	mustNil(t, st.CreateKey(b))
	mustNil(t, st.DeleteKey(a.ID)) // b keeps id 2 while it is the only key
	st.insertLogs([]*RequestLog{{CreatedAt: time.Now().UnixMilli(), KeyID: b.ID, Cost: 3, Currency: "CNY"}})
	e, _ := st.Export()
	mustNil(t, st.Import(e))
	if got := st.Snapshot().Keys[b.Key]; got == nil || got.ID != b.ID {
		t.Fatalf("key id changed on import: %+v", got)
	}
	if spend, _ := st.KeySpend(MonthStart(time.Now())); spend[b.ID] != 3 {
		t.Fatalf("spend lost: %v", spend)
	}
}

func TestProviderHeaderTemplates(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{{"X-A": "{{$typo}}"}, {"X-A": "{{header.Authorization}}"}, {"Bad Name": "x"}} {
		if err := st.CreateProvider(&Provider{Prefix: "x", OpenAIBaseURL: "http://a/v1", Headers: h}); err == nil {
			t.Errorf("accepted %v", h)
		}
	}
	if err := st.CreateProvider(&Provider{Prefix: "x", OpenAIBaseURL: "http://a/v1", UAMode: "platform"}); err != nil {
		t.Fatal(err)
	}
	// an OpenCode Go provider saved by an older version: one fixed session for everything
	mustNil(t, st.CreateProvider(&Provider{Prefix: "opencode", Vendor: "opencode-go", OpenAIBaseURL: "https://opencode.ai/zen/go/v1",
		Headers: map[string]string{"x-opencode-session": "ai-route", "X-Other": "keep"}}))
	mustNil(t, st.CreateProvider(&Provider{Prefix: "mine", Vendor: "opencode-go", OpenAIBaseURL: "https://opencode.ai/zen/go/v1",
		Headers: map[string]string{"x-opencode-session": "my-own"}}))
	mustNil(t, st.CreateProvider(&Provider{Prefix: "v030", Vendor: "opencode-go", OpenAIBaseURL: "https://opencode.ai/zen/go/v1",
		Headers: map[string]string{"x-opencode-session": "{{header.x-opencode-session ?? $conversation}}"}}))
	st.Close()
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := st.Snapshot().Providers
	if p["opencode"].Headers["x-opencode-session"] != OpenCodeSessionHeader || p["opencode"].Headers["X-Other"] != "keep" {
		t.Fatalf("not migrated: %v", p["opencode"].Headers)
	}
	if p["v030"].Headers["x-opencode-session"] != OpenCodeSessionHeader {
		t.Fatalf("earlier template not migrated: %v", p["v030"].Headers)
	}
	if p["mine"].Headers["x-opencode-session"] != "my-own" {
		t.Fatalf("custom value changed: %v", p["mine"].Headers)
	}
}
