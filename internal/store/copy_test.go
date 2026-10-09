package store

import (
	"math"
	"reflect"
	"testing"
	"time"
)

// SQLite -> target (PostgreSQL when AI_ROUTE_TEST_DATABASE_URL is set) ->
// SQLite again: everything survives, ids included.
func TestCopyBetweenBackends(t *testing.T) {
	src, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	mustNil(t, src.CreateProvider(&Provider{Prefix: "a", OpenAIBaseURL: "http://a/v1", APIKey: "ka", Headers: map[string]string{"X-A": "1"},
		Prices: map[string]Price{"*": {Input: 2, Output: 8}}}))
	mustNil(t, src.CreateModel(&Model{Name: "m", Targets: []string{"a/x*2 | a/y", "a/z"}, Tags: []string{"code"}}))
	gone, k1, k2 := &APIKey{Name: "gone"}, &APIKey{Name: "k1", MonthlyBudget: 200, RPM: 60}, &APIKey{Name: "k2", MonthlyBudget: 12.5}
	mustNil(t, src.CreateKey(gone))
	mustNil(t, src.CreateKey(k1))
	mustNil(t, src.CreateKey(k2))
	mustNil(t, src.DeleteKey(gone.ID))
	st := src.GetSettings()
	st.Currency = "USD"
	mustNil(t, src.UpdateSettings(st))
	mustNil(t, src.SetKV("admin_token", "admin-xyz"))
	now := time.Now().UnixMilli()
	var logs []*RequestLog
	for i := 0; i < 12345; i++ { // more than one page
		logs = append(logs, &RequestLog{CreatedAt: now - int64(i), KeyID: []int64{k1.ID, k2.ID}[i%2], PublicModel: "m", Success: i%7 != 0,
			Cost: 0.25, Currency: "CNY", InputTokens: 100, Attempts: []Attempt{{Target: "a/x", HTTPStatus: 200}}})
	}
	mustNil(t, src.insertLogs(logs))

	mid, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer mid.Close()
	copied := map[string]int64{}
	mustNil(t, src.CopyTo(mid, func(table string, n int64) { copied[table] = n }))
	if copied["request_logs"] != 12345 || copied["api_keys"] != 2 || copied["providers"] != 1 {
		t.Fatalf("progress: %v", copied)
	}
	back, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	mustNil(t, mid.CopyTo(back, nil))

	for _, dst := range []*Store{mid, back} {
		a, b := src.Snapshot(), dst.Snapshot()
		if !reflect.DeepEqual(a.Providers, b.Providers) || !reflect.DeepEqual(a.Keys, b.Keys) || !reflect.DeepEqual(a.Settings, b.Settings) {
			t.Fatalf("%s: configuration differs", dst.Backend())
		}
		if tok, _ := dst.GetKV("admin_token"); tok != "admin-xyz" {
			t.Fatalf("%s: admin token %q", dst.Backend(), tok)
		}
		ws, _ := src.KeySpend(MonthStart(time.Now()))
		gs, _ := dst.KeySpend(MonthStart(time.Now()))
		for id, v := range ws {
			if math.Abs(gs[id]-v) > 1e-6 || len(gs) != len(ws) {
				t.Fatalf("%s: spend %v != %v", dst.Backend(), gs, ws)
			}
		}
		sa, _ := src.GetStats(0, 3600_000)
		sb, _ := dst.GetStats(0, 3600_000)
		if sa.Total != sb.Total {
			t.Fatalf("%s: stats %+v != %+v", dst.Backend(), sb.Total, sa.Total)
		}
		l, total, _ := dst.QueryLogs(LogQuery{Limit: 1})
		if total != 12345 || len(l[0].Attempts) != 1 {
			t.Fatalf("%s: logs %d %+v", dst.Backend(), total, l)
		}
		// new rows continue after the copied ids
		k := &APIKey{Name: "new"}
		mustNil(t, dst.CreateKey(k))
		if k.ID <= k2.ID {
			t.Fatalf("%s: new key id %d", dst.Backend(), k.ID)
		}
	}
	if err := src.CopyTo(back, nil); err == nil {
		t.Fatal("copied into a non-empty database")
	}
}
