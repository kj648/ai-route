package store

import (
	"sort"
	"time"
)

// Request statistics are kept twice: every request in request_logs (pruned
// after LogRetentionDays) and an hourly rollup in request_stats, updated in
// the same transaction as the log insert. The overview and the monthly
// budgets read the rollup, so they stay fast at any log volume and keep
// working after the raw rows are gone.

const (
	hourMs = int64(3600 * 1000)
	// statsRetentionDays is how long the hourly rollup is kept.
	statsRetentionDays = 400
	statsBackfilledKey = "stats_rollup_v1"
)

const statsSchema = `
CREATE TABLE IF NOT EXISTS request_stats (
	hour INTEGER NOT NULL,
	key_id INTEGER NOT NULL DEFAULT 0,
	key_name TEXT NOT NULL DEFAULT '',
	public_model TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	upstream_model TEXT NOT NULL DEFAULT '',
	requests INTEGER NOT NULL DEFAULT 0,
	success INTEGER NOT NULL DEFAULT 0,
	fallback INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	cost_cny REAL NOT NULL DEFAULT 0,
	unpriced INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttfb_ms INTEGER NOT NULL DEFAULT 0,
	ttfb_n INTEGER NOT NULL DEFAULT 0,
	user_id INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, key_id, key_name, public_model, provider, upstream_model)
);
CREATE INDEX IF NOT EXISTS idx_stats_key ON request_stats(key_id, hour);
CREATE INDEX IF NOT EXISTS idx_stats_provider ON request_stats(provider, hour);
`

// statsUpsert adds a group's counters to its rollup row (the existing row
// is qualified with the table name: PostgreSQL finds a bare column name
// ambiguous, SQLite accepts both).
const statsUpsert = `INSERT INTO request_stats (hour, key_id, key_name, public_model, provider, upstream_model, requests, success, fallback, input_tokens, output_tokens, cached_tokens, cost_usd, cost_cny, unpriced, latency_ms, ttfb_ms, ttfb_n, user_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(hour, key_id, key_name, public_model, provider, upstream_model) DO UPDATE SET
	requests = request_stats.requests + excluded.requests, success = request_stats.success + excluded.success, fallback = request_stats.fallback + excluded.fallback, input_tokens = request_stats.input_tokens + excluded.input_tokens, output_tokens = request_stats.output_tokens + excluded.output_tokens, cached_tokens = request_stats.cached_tokens + excluded.cached_tokens, cost_usd = request_stats.cost_usd + excluded.cost_usd, cost_cny = request_stats.cost_cny + excluded.cost_cny, unpriced = request_stats.unpriced + excluded.unpriced, latency_ms = request_stats.latency_ms + excluded.latency_ms, ttfb_ms = request_stats.ttfb_ms + excluded.ttfb_ms, ttfb_n = request_stats.ttfb_n + excluded.ttfb_n`

// statsBackfill rebuilds the rollup from request_logs.
const statsBackfill = `INSERT INTO request_stats (hour, key_id, key_name, public_model, provider, upstream_model, requests, success, fallback, input_tokens, output_tokens, cached_tokens, cost_usd, cost_cny, unpriced, latency_ms, ttfb_ms, ttfb_n, user_id)
SELECT (created_at / 3600000) * 3600000, key_id, key_name, public_model, provider, upstream_model,
	COUNT(*), SUM(success), SUM(fallback), SUM(input_tokens), SUM(output_tokens), SUM(cached_tokens),
	SUM(CASE WHEN currency = 'USD' THEN cost ELSE 0 END), SUM(CASE WHEN currency = 'CNY' THEN cost ELSE 0 END),
	SUM(CASE WHEN cost_source = '' AND input_tokens + output_tokens > 0 THEN 1 ELSE 0 END),
	SUM(latency_ms), SUM(CASE WHEN ttfb_ms > 0 THEN ttfb_ms ELSE 0 END), SUM(CASE WHEN ttfb_ms > 0 THEN 1 ELSE 0 END), MAX(user_id)
FROM request_logs GROUP BY 1, 2, 3, 4, 5, 6`

type statKey struct {
	hour, keyID                         int64
	keyName, model, provider, upstreamM string
	userID                              int64 // follows from keyID; not part of the primary key
}

// statGroup holds one row of counters, either from the rollup or from a
// raw log line (requests = 1).
type statGroup struct {
	requests, success, fallback, in, out, cached, unpriced, latency, ttfbSum, ttfbN int64
	usd, cny                                                                        float64
}

func (g *statGroup) add(o *statGroup) {
	g.requests += o.requests
	g.success += o.success
	g.fallback += o.fallback
	g.in += o.in
	g.out += o.out
	g.cached += o.cached
	g.unpriced += o.unpriced
	g.latency += o.latency
	g.ttfbSum += o.ttfbSum
	g.ttfbN += o.ttfbN
	g.usd += o.usd
	g.cny += o.cny
}

// groupOf turns a log line into its rollup key and counters.
func groupOf(l *RequestLog) (statKey, statGroup) {
	k := statKey{hour: l.CreatedAt - l.CreatedAt%hourMs, keyID: l.KeyID, keyName: l.KeyName,
		model: l.PublicModel, provider: l.Provider, upstreamM: l.UpstreamModel, userID: l.UserID}
	g := statGroup{requests: 1, in: l.InputTokens, out: l.OutputTokens, cached: l.CachedTokens, latency: l.LatencyMs}
	if l.Success {
		g.success = 1
	}
	if l.Fallback {
		g.fallback = 1
	}
	switch l.Currency {
	case CurrencyUSD:
		g.usd = l.Cost
	case CurrencyCNY:
		g.cny = l.Cost
	}
	if l.CostSource == "" && l.InputTokens+l.OutputTokens > 0 {
		g.unpriced = 1
	}
	if l.TTFBMs > 0 {
		g.ttfbSum, g.ttfbN = l.TTFBMs, 1
	}
	return k, g
}

// upsertStats folds a batch of (already clipped) log lines into the rollup
// inside the log insert's transaction.
func (s *Store) upsertStats(tx *txx, batch []RequestLog) error {
	groups := map[statKey]*statGroup{}
	var order []statKey
	for i := range batch {
		k, g := groupOf(&batch[i])
		if cur, ok := groups[k]; ok {
			cur.add(&g)
		} else {
			gg := g
			groups[k] = &gg
			order = append(order, k)
		}
	}
	stmt, err := tx.Prepare(statsUpsert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, k := range order {
		g := groups[k]
		if _, err := stmt.Exec(k.hour, k.keyID, k.keyName, k.model, k.provider, k.upstreamM,
			g.requests, g.success, g.fallback, g.in, g.out, g.cached, g.usd, g.cny, g.unpriced, g.latency, g.ttfbSum, g.ttfbN, k.userID); err != nil {
			return err
		}
	}
	return nil
}

// ensureStatsBackfilled builds the rollup from existing logs once (on the
// first start after the upgrade). It runs before the log writer starts, so
// nothing is counted twice.
func (s *Store) ensureStatsBackfilled() error {
	if _, ok := s.GetKV(statsBackfilledKey); ok {
		return nil
	}
	if err := s.RebuildStats(); err != nil {
		return err
	}
	return s.SetKV(statsBackfilledKey, "1")
}

// RebuildStats recomputes the whole rollup from request_logs (after a
// migration between databases).
func (s *Store) RebuildStats() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM request_stats`); err != nil {
		return err
	}
	if _, err := tx.Exec(statsBackfill); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- reading ----------

// statAcc accumulates one group of the stats.
type statAcc struct {
	row            StatRow
	latency        int64
	ttfbSum, ttfbN int64
}

func (a *statAcc) result(key string) StatRow {
	r := a.row
	r.Key = key
	if r.Requests > 0 {
		r.AvgLatencyMs = float64(a.latency) / float64(r.Requests)
	}
	if a.ttfbN > 0 {
		r.AvgTTFBMs = float64(a.ttfbSum) / float64(a.ttfbN)
	}
	return r
}

func (a *statAcc) add(g *statGroup, cost float64) {
	r := &a.row
	r.Requests += g.requests
	r.Success += g.success
	r.Failed += g.requests - g.success
	r.Fallback += g.fallback
	r.InputTokens += g.in
	r.OutputTokens += g.out
	r.CachedTokens += g.cached
	r.Cost += cost
	r.Unpriced += g.unpriced
	a.latency += g.latency
	a.ttfbSum += g.ttfbSum
	a.ttfbN += g.ttfbN
}

// sortedRows orders groups by request count (then key), like the console expects.
func sortedRows(m map[string]*statAcc) []StatRow {
	out := make([]StatRow, 0, len(m))
	for k, a := range m {
		out = append(out, a.result(k))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// GetStats aggregates requests since the given unix ms; the timeline uses
// fixed buckets of bucketMs (gaps filled with zero rows, keys in local
// time). Buckets of an hour or more are served from the hourly rollup
// (since is rounded down to the hour); shorter ones scan the raw logs.
// Costs are converted to the display currency.
func (s *Store) GetStats(since int64, bucketMs int64) (*Stats, error) {
	return s.GetUserStats(since, bucketMs, 0)
}

// GetUserStats is GetStats for one user's requests (userID 0 = everyone).
func (s *Store) GetUserStats(since int64, bucketMs int64, userID int64) (*Stats, error) {
	settings := s.GetSettings()
	usd, cny := settings.CurrencyFactors()
	st := &Stats{Since: since, BucketMs: bucketMs, Currency: settings.Currency}
	var total statAcc
	byModel, byProvider, byTarget, byKey := map[string]*statAcc{}, map[string]*statAcc{}, map[string]*statAcc{}, map[string]*statAcc{}
	byBucket := map[int64]*statAcc{}
	off := tzOffsetMs()
	group := func(m map[string]*statAcc, k string) *statAcc {
		a, ok := m[k]
		if !ok {
			a = &statAcc{}
			m[k] = a
		}
		return a
	}
	fold := func(at int64, k statKey, g *statGroup) {
		cost := g.usd*usd + g.cny*cny
		target, provider := "(none)", k.provider
		if provider != "" {
			target = provider + "/" + k.upstreamM
		} else {
			provider = "(none)"
		}
		bucket := (at + off) / bucketMs
		b, ok := byBucket[bucket]
		if !ok {
			b = &statAcc{}
			byBucket[bucket] = b
		}
		for _, a := range []*statAcc{&total, group(byModel, k.model), group(byProvider, provider), group(byTarget, target), group(byKey, k.keyName), b} {
			a.add(g, cost)
		}
	}
	var err error
	if bucketMs >= hourMs {
		err = s.scanStatsRollup(since-since%hourMs, userID, fold)
	} else {
		err = s.scanStatsRaw(since, userID, fold)
	}
	if err != nil {
		return nil, err
	}
	st.Total = total.result("total")
	st.ByModel, st.ByProvider, st.ByTarget, st.ByKey = sortedRows(byModel), sortedRows(byProvider), sortedRows(byTarget), sortedRows(byKey)
	first, last := (since+off)/bucketMs, (time.Now().UnixMilli()+off)/bucketMs
	layout := "01-02 15:04"
	if bucketMs >= 24*hourMs {
		layout = "2006-01-02"
	}
	for k := first; k <= last; k++ {
		var r StatRow
		if a, ok := byBucket[k]; ok {
			r = a.result("")
		}
		r.Key = time.UnixMilli(k*bucketMs - off).Format(layout)
		st.Timeline = append(st.Timeline, r)
	}
	return st, nil
}

// userFilter narrows a stats query to one user.
func userFilter(userID int64, args ...any) (string, []any) {
	if userID <= 0 {
		return "", args
	}
	return " AND user_id = ?", append(args, userID)
}

func (s *Store) scanStatsRollup(since, userID int64, fold func(int64, statKey, *statGroup)) error {
	cond, args := userFilter(userID, since)
	rows, err := s.db.Query(`SELECT hour, key_id, key_name, public_model, provider, upstream_model, requests, success, fallback,
		input_tokens, output_tokens, cached_tokens, cost_usd, cost_cny, unpriced, latency_ms, ttfb_ms, ttfb_n
		FROM request_stats WHERE hour >= ?`+cond, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k statKey
		var g statGroup
		if err := rows.Scan(&k.hour, &k.keyID, &k.keyName, &k.model, &k.provider, &k.upstreamM, &g.requests, &g.success, &g.fallback,
			&g.in, &g.out, &g.cached, &g.usd, &g.cny, &g.unpriced, &g.latency, &g.ttfbSum, &g.ttfbN); err != nil {
			return err
		}
		fold(k.hour, k, &g)
	}
	return rows.Err()
}

func (s *Store) scanStatsRaw(since, userID int64, fold func(int64, statKey, *statGroup)) error {
	cond, args := userFilter(userID, since)
	rows, err := s.db.Query(`SELECT created_at, key_id, key_name, public_model, provider, upstream_model, success, fallback,
		input_tokens, output_tokens, cached_tokens, latency_ms, ttfb_ms, cost, currency, cost_source
		FROM request_logs WHERE created_at >= ?`+cond, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l RequestLog
		var success, fallback int
		if err := rows.Scan(&l.CreatedAt, &l.KeyID, &l.KeyName, &l.PublicModel, &l.Provider, &l.UpstreamModel, &success, &fallback,
			&l.InputTokens, &l.OutputTokens, &l.CachedTokens, &l.LatencyMs, &l.TTFBMs, &l.Cost, &l.Currency, &l.CostSource); err != nil {
			return err
		}
		l.Success, l.Fallback = success == 1, fallback == 1
		k, g := groupOf(&l)
		fold(l.CreatedAt, k, &g)
	}
	return rows.Err()
}

// tzOffsetMs aligns day buckets to local midnight.
func tzOffsetMs() int64 {
	_, off := time.Now().Zone()
	return int64(off) * 1000
}

// KeySpend returns each key's cost since the given unix ms (rounded down
// to the hour), in the display currency, from the hourly rollup.
func (s *Store) KeySpend(since int64) (map[int64]float64, error) {
	usd, cny := s.GetSettings().CurrencyFactors()
	rows, err := s.db.Query(`SELECT key_id, SUM(cost_usd * CAST(? AS DOUBLE PRECISION) + cost_cny * CAST(? AS DOUBLE PRECISION))
		FROM request_stats WHERE hour >= ? AND (cost_usd > 0 OR cost_cny > 0) GROUP BY key_id`, usd, cny, since-since%hourMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// MonthStart is local midnight on the first day of t's month, in unix ms.
func MonthStart(t time.Time) int64 {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location()).UnixMilli()
}
