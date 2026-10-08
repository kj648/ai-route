package store

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Attempt records one upstream try within a request.
type Attempt struct {
	Target     string `json:"target"`
	Retry      int    `json:"retry,omitempty"` // 0 = first try on this target
	Protocol   string `json:"protocol"`
	HTTPStatus int    `json:"http_status"`
	LatencyMs  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
	Cooling    bool   `json:"cooling,omitempty"` // tried as last resort while cooling down
}

type RequestLog struct {
	ID               int64     `json:"id"`
	CreatedAt        int64     `json:"created_at"`
	KeyID            int64     `json:"key_id"`
	KeyName          string    `json:"key_name"`
	RequestedModel   string    `json:"requested_model"`
	PublicModel      string    `json:"public_model"`
	Inbound          string    `json:"inbound"`
	Stream           bool      `json:"stream"`
	Provider         string    `json:"provider"`
	UpstreamModel    string    `json:"upstream_model"`
	UpstreamProtocol string    `json:"upstream_protocol"`
	Success          bool      `json:"success"`
	HTTPStatus       int       `json:"http_status"`
	LatencyMs        int64     `json:"latency_ms"`
	TTFBMs           int64     `json:"ttfb_ms"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	Fallback         bool      `json:"fallback"`
	Attempts         []Attempt `json:"attempts"`
	Error            string    `json:"error"`
	ClientIP         string    `json:"client_ip"`
	// Cost is in Currency; CostSource is "upstream" (reported by the
	// upstream, e.g. OpenRouter), "price" (from the provider's unit prices)
	// or "" (not costed).
	Cost       float64 `json:"cost"`
	Currency   string  `json:"currency"`
	CostSource string  `json:"cost_source"`

	flushed chan struct{} // FlushLogs marker, not a real entry
}

// AddLog enqueues a log entry; it never blocks the request path.
func (s *Store) AddLog(l *RequestLog) {
	s.logMu.RLock()
	defer s.logMu.RUnlock()
	if s.logClosed {
		return
	}
	select {
	case s.logCh <- l:
	default:
		log.Printf("request log queue full, dropping entry")
	}
}

// FlushLogs blocks until every log queued before the call is written.
func (s *Store) FlushLogs() {
	done := make(chan struct{})
	s.logMu.RLock()
	if s.logClosed {
		s.logMu.RUnlock()
		return
	}
	s.logCh <- &RequestLog{flushed: done}
	s.logMu.RUnlock()
	<-done
}

func (s *Store) logWriter() {
	defer close(s.logDone)
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	s.cleanupLogs()
	batch := make([]*RequestLog, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertLogs(batch); err != nil {
			log.Printf("write request logs: %v", err)
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case l, ok := <-s.logCh:
			if !ok {
				flush()
				return
			}
			if l.flushed != nil {
				flush()
				close(l.flushed)
				continue
			}
			batch = append(batch, l)
			if len(batch) >= 64 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-cleanup.C:
			s.cleanupLogs()
		}
	}
}

func (s *Store) insertLogs(batch []*RequestLog) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO request_logs (created_at, key_id, key_name, requested_model, public_model, inbound, stream, provider, upstream_model, upstream_protocol, success, http_status, latency_ms, ttfb_ms, input_tokens, output_tokens, cached_tokens, fallback, attempts, error, client_ip, cost, currency, cost_source) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, l := range batch {
		attempts := l.Attempts // never mutate: callers may still read the entry
		if attempts == nil {
			attempts = []Attempt{}
		}
		if _, err := stmt.Exec(l.CreatedAt, l.KeyID, l.KeyName, l.RequestedModel, l.PublicModel, l.Inbound, b2i(l.Stream), l.Provider, l.UpstreamModel, l.UpstreamProtocol, b2i(l.Success), l.HTTPStatus, l.LatencyMs, l.TTFBMs, l.InputTokens, l.OutputTokens, l.CachedTokens, b2i(l.Fallback), mustJSON(attempts), l.Error, l.ClientIP, l.Cost, l.Currency, l.CostSource); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) cleanupLogs() {
	days := s.GetSettings().LogRetentionDays
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM request_logs WHERE created_at < ?`, cutoff); err != nil {
		log.Printf("cleanup logs: %v", err)
	}
}

type LogQuery struct {
	Model    string
	Provider string
	KeyID    int64
	Status   string // "success" | "failed" | ""
	Fallback bool
	Limit    int
	Offset   int
}

func (q LogQuery) where() (string, []any) {
	var conds []string
	var args []any
	if q.Model != "" {
		conds = append(conds, "public_model = ?")
		args = append(args, q.Model)
	}
	if q.Provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, q.Provider)
	}
	if q.KeyID > 0 {
		conds = append(conds, "key_id = ?")
		args = append(args, q.KeyID)
	}
	switch q.Status {
	case "success":
		conds = append(conds, "success = 1")
	case "failed":
		conds = append(conds, "success = 0")
	}
	if q.Fallback {
		conds = append(conds, "fallback = 1")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *Store) QueryLogs(q LogQuery) ([]*RequestLog, int64, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	where, args := q.where()
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id, created_at, key_id, key_name, requested_model, public_model, inbound, stream, provider, upstream_model, upstream_protocol, success, http_status, latency_ms, ttfb_ms, input_tokens, output_tokens, cached_tokens, fallback, attempts, error, client_ip, cost, currency, cost_source FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*RequestLog{}
	for rows.Next() {
		l := &RequestLog{}
		var stream, success, fallback int
		var attempts string
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.KeyID, &l.KeyName, &l.RequestedModel, &l.PublicModel, &l.Inbound, &stream, &l.Provider, &l.UpstreamModel, &l.UpstreamProtocol, &success, &l.HTTPStatus, &l.LatencyMs, &l.TTFBMs, &l.InputTokens, &l.OutputTokens, &l.CachedTokens, &fallback, &attempts, &l.Error, &l.ClientIP, &l.Cost, &l.Currency, &l.CostSource); err != nil {
			return nil, 0, err
		}
		l.Stream, l.Success, l.Fallback = stream == 1, success == 1, fallback == 1
		_ = json.Unmarshal([]byte(attempts), &l.Attempts)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

type StatRow struct {
	Key          string  `json:"key"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	Fallback     int64   `json:"fallback"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	AvgTTFBMs    float64 `json:"avg_ttfb_ms"`
	// Cost is in Stats.Currency. Unpriced counts requests that used tokens
	// but have no cost (no unit price configured for that upstream model).
	Cost     float64 `json:"cost"`
	Unpriced int64   `json:"unpriced"`
}

type Stats struct {
	Since      int64     `json:"since"`
	BucketMs   int64     `json:"bucket_ms"`
	Currency   string    `json:"currency"`
	Total      StatRow   `json:"total"`
	ByModel    []StatRow `json:"by_model"`
	ByProvider []StatRow `json:"by_provider"`
	ByTarget   []StatRow `json:"by_target"`
	ByKey      []StatRow `json:"by_key"`
	Timeline   []StatRow `json:"timeline"`
}

const statCols = `COUNT(*), SUM(success), SUM(1-success), SUM(fallback), SUM(input_tokens), SUM(output_tokens), SUM(cached_tokens), AVG(latency_ms), AVG(CASE WHEN ttfb_ms > 0 THEN ttfb_ms END), ` +
	`SUM(cost * CASE currency WHEN 'USD' THEN ? WHEN 'CNY' THEN ? ELSE 0 END), SUM(cost_source = '' AND input_tokens + output_tokens > 0)`

// statGroup aggregates logs grouped by expr; costs are converted to the
// settings' display currency.
func (s *Store) statGroup(expr string, since int64, order string) ([]StatRow, error) {
	st := s.GetSettings()
	usd, cny := 1.0, 1/st.USDToCNY // to USD
	if st.Currency == CurrencyCNY {
		usd, cny = st.USDToCNY, 1
	}
	rows, err := s.db.Query(`SELECT `+expr+` AS k, `+statCols+` FROM request_logs WHERE created_at >= ? GROUP BY k ORDER BY `+order, usd, cny, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatRow{}
	for rows.Next() {
		var r StatRow
		var avgTTFB *float64
		var avgLat *float64
		var succ, fail, fb, in, outT, cached, unpriced *int64
		var cost *float64
		if err := rows.Scan(&r.Key, &r.Requests, &succ, &fail, &fb, &in, &outT, &cached, &avgLat, &avgTTFB, &cost, &unpriced); err != nil {
			return nil, err
		}
		r.Unpriced = deref(unpriced)
		if cost != nil {
			r.Cost = *cost
		}
		r.Success, r.Failed, r.Fallback = deref(succ), deref(fail), deref(fb)
		r.InputTokens, r.OutputTokens, r.CachedTokens = deref(in), deref(outT), deref(cached)
		if avgLat != nil {
			r.AvgLatencyMs = *avgLat
		}
		if avgTTFB != nil {
			r.AvgTTFBMs = *avgTTFB
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// GetStats aggregates logs since the given unix ms; the timeline uses
// fixed buckets of bucketMs (gaps filled with zero rows, keys in local time).
func (s *Store) GetStats(since int64, bucketMs int64) (*Stats, error) {
	st := &Stats{Since: since, BucketMs: bucketMs, Currency: s.GetSettings().Currency}
	tot, err := s.statGroup(`'total'`, since, "k")
	if err != nil {
		return nil, err
	}
	if len(tot) > 0 {
		st.Total = tot[0]
	}
	st.Total.Key = "total"
	if st.ByModel, err = s.statGroup(`public_model`, since, "COUNT(*) DESC"); err != nil {
		return nil, err
	}
	if st.ByProvider, err = s.statGroup(`CASE WHEN provider = '' THEN '(none)' ELSE provider END`, since, "COUNT(*) DESC"); err != nil {
		return nil, err
	}
	if st.ByTarget, err = s.statGroup(`CASE WHEN provider = '' THEN '(none)' ELSE provider || '/' || upstream_model END`, since, "COUNT(*) DESC"); err != nil {
		return nil, err
	}
	if st.ByKey, err = s.statGroup(`key_name`, since, "COUNT(*) DESC"); err != nil {
		return nil, err
	}
	rows, err := s.statGroup(fmt.Sprintf(`CAST((created_at + %d) / %d AS TEXT)`, tzOffsetMs(), bucketMs), since, "k")
	if err != nil {
		return nil, err
	}
	byBucket := map[int64]StatRow{}
	for _, r := range rows {
		n, _ := strconv.ParseInt(r.Key, 10, 64)
		byBucket[n] = r
	}
	off := tzOffsetMs()
	first, last := (since+off)/bucketMs, (time.Now().UnixMilli()+off)/bucketMs
	layout := "01-02 15:04"
	if bucketMs >= 24*3600*1000 {
		layout = "2006-01-02"
	}
	for b := first; b <= last; b++ {
		r := byBucket[b]
		r.Key = time.UnixMilli(b*bucketMs - off).Format(layout)
		st.Timeline = append(st.Timeline, r)
	}
	return st, nil
}

// tzOffsetMs aligns day buckets to local midnight.
func tzOffsetMs() int64 {
	_, off := time.Now().Zone()
	return int64(off) * 1000
}
