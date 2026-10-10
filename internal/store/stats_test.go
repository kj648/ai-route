package store

import (
	"math"
	"testing"
	"time"
)

func sampleLogs(now int64) []*RequestLog {
	h := hourMs
	return []*RequestLog{
		{CreatedAt: now - 3*h - 100, KeyID: 1, KeyName: "a", PublicModel: "coder", Provider: "kimi", UpstreamModel: "k", Success: true, InputTokens: 100, OutputTokens: 10, CachedTokens: 20, LatencyMs: 400, TTFBMs: 100, Cost: 0.5, Currency: "USD", CostSource: "price"},
		{CreatedAt: now - 3*h - 50, KeyID: 1, KeyName: "a", PublicModel: "coder", Provider: "kimi", UpstreamModel: "k", Success: true, InputTokens: 50, OutputTokens: 5, LatencyMs: 200, TTFBMs: 50, Cost: 2, Currency: "CNY", CostSource: "price"},
		{CreatedAt: now - 2*h, KeyID: 1, KeyName: "a", PublicModel: "coder", Provider: "glm", UpstreamModel: "g", Success: false, Fallback: true, LatencyMs: 900, HTTPStatus: 502},
		{CreatedAt: now - h, KeyID: 2, KeyName: "b", PublicModel: "fast", Provider: "glm", UpstreamModel: "flash", Success: true, InputTokens: 7, OutputTokens: 3, LatencyMs: 100, TTFBMs: 40}, // unpriced
		{CreatedAt: now - 10, KeyID: 2, KeyName: "b", PublicModel: "fast", Success: false, HTTPStatus: 429},                                                                                    // rejected: no provider
	}
}

// sameStats compares everything but the timeline (its granularity differs).
func sameStats(t *testing.T, what string, a, b *Stats) {
	t.Helper()
	eq := func(x, y StatRow) bool {
		return x.Key == y.Key && x.Requests == y.Requests && x.Success == y.Success && x.Failed == y.Failed && x.Fallback == y.Fallback &&
			x.InputTokens == y.InputTokens && x.OutputTokens == y.OutputTokens && x.CachedTokens == y.CachedTokens &&
			math.Abs(x.AvgLatencyMs-y.AvgLatencyMs) < 1e-9 && math.Abs(x.AvgTTFBMs-y.AvgTTFBMs) < 1e-9 &&
			math.Abs(x.Cost-y.Cost) < 1e-9 && x.Unpriced == y.Unpriced
	}
	if !eq(a.Total, b.Total) {
		t.Fatalf("%s: total %+v != %+v", what, a.Total, b.Total)
	}
	for name, pair := range map[string][2][]StatRow{"model": {a.ByModel, b.ByModel}, "provider": {a.ByProvider, b.ByProvider}, "target": {a.ByTarget, b.ByTarget}, "key": {a.ByKey, b.ByKey}} {
		if len(pair[0]) != len(pair[1]) {
			t.Fatalf("%s: by %s: %+v != %+v", what, name, pair[0], pair[1])
		}
		for i := range pair[0] {
			if !eq(pair[0][i], pair[1][i]) {
				t.Fatalf("%s: by %s row %d: %+v != %+v", what, name, i, pair[0][i], pair[1][i])
			}
		}
	}
	var sa, sb int64
	for _, r := range a.Timeline {
		sa += r.Requests
	}
	for _, r := range b.Timeline {
		sb += r.Requests
	}
	if sa != sb {
		t.Fatalf("%s: timeline sums %d != %d", what, sa, sb)
	}
}

// The hourly rollup, maintained with every log insert, gives the same
// overview as scanning the raw logs.
func TestStatsRollupMatchesRawLogs(t *testing.T) {
	st, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings := DefaultSettings()
	settings.USDToCNY = 7
	mustNil(t, st.UpdateSettings(settings))
	now := time.Now().UnixMilli()
	mustNil(t, st.insertLogs(sampleLogs(now)[:3]))
	mustNil(t, st.insertLogs(sampleLogs(now)[3:]))
	since := now - 24*hourMs
	raw, err := st.GetStats(since, 5*60*1000) // sub-hour buckets: raw logs
	mustNil(t, err)
	rolled, err := st.GetStats(since, hourMs) // hourly buckets: rollup
	mustNil(t, err)
	sameStats(t, "rollup vs raw", raw, rolled)
	if raw.Total.Requests != 5 || raw.Total.Failed != 2 || raw.Total.Fallback != 1 || raw.Total.Unpriced != 1 ||
		math.Abs(raw.Total.Cost-(0.5*7+2)) > 1e-9 || raw.Total.CachedTokens != 20 {
		t.Fatalf("total: %+v", raw.Total)
	}
	// averages come from sums and counts, not from averaging averages
	for _, r := range rolled.ByKey {
		if r.Key == "a" && (math.Abs(r.AvgLatencyMs-500) > 1e-9 || math.Abs(r.AvgTTFBMs-75) > 1e-9) {
			t.Fatalf("key a averages: %+v", r)
		}
	}
	// the rollup also feeds the monthly budgets
	spend, err := st.KeySpend(since)
	mustNil(t, err)
	if math.Abs(spend[1]-5.5) > 1e-9 || spend[2] != 0 {
		t.Fatalf("spend: %v", spend)
	}
}

// Logs written before the rollup existed are folded in once at startup,
// and the overview survives the raw logs being pruned.
func TestStatsBackfillAndSurvivesLogRetention(t *testing.T) {
	st, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	mustNil(t, st.insertLogs(sampleLogs(now)))
	before, err := st.GetStats(now-24*hourMs, hourMs)
	mustNil(t, err)

	// pretend this database predates the rollup
	if _, err := st.db.Exec(`DELETE FROM request_stats`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM settings WHERE k = ?`, statsBackfilledKey); err != nil {
		t.Fatal(err)
	}
	if empty, err := st.GetStats(now-24*hourMs, hourMs); err != nil || empty.Total.Requests != 0 {
		t.Fatalf("rollup not cleared: %+v %v", empty.Total, err)
	}
	mustNil(t, st.ensureStatsBackfilled())
	after, err := st.GetStats(now-24*hourMs, hourMs)
	mustNil(t, err)
	sameStats(t, "backfill", before, after)
	mustNil(t, st.ensureStatsBackfilled()) // a second start must not double count
	again, err := st.GetStats(now-24*hourMs, hourMs)
	mustNil(t, err)
	sameStats(t, "idempotent", before, again)

	// raw logs pruned: the overview and the budgets still see the month
	if _, err := st.db.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	pruned, err := st.GetStats(now-24*hourMs, hourMs)
	mustNil(t, err)
	sameStats(t, "after prune", before, pruned)
	if spend, err := st.KeySpend(now - 24*hourMs); err != nil || spend[1] == 0 {
		t.Fatalf("spend after prune: %v %v", spend, err)
	}
}
