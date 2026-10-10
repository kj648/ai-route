package store

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
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
	// OverQuota: the provider had used up one of its plan quotas, so the
	// target was tried after the others
	OverQuota bool `json:"over_quota,omitempty"`
	// Headers are the resolved values of dynamic provider headers.
	Headers map[string]string `json:"headers,omitempty"`
}

type RequestLog struct {
	ID               int64     `json:"id"`
	CreatedAt        int64     `json:"created_at"`
	KeyID            int64     `json:"key_id"`
	KeyName          string    `json:"key_name"`
	UserID           int64     `json:"user_id"` // owner of the key, 0 = administrator
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
	RequestID        string    `json:"request_id"`
	// Cost is in Currency; CostSource is "upstream" (reported by the
	// upstream, e.g. OpenRouter), "price" (from the provider's unit prices)
	// or "" (not costed).
	Cost       float64 `json:"cost"`
	Currency   string  `json:"currency"`
	CostSource string  `json:"cost_source"`

	flushed chan struct{} // FlushLogs marker, not a real entry
}

// logEnqueueWait bounds how long a request waits for room in the log queue
// before its entry is dropped; the response has already been sent by then,
// so a short wait costs the client nothing and keeps the cost rows the
// monthly budgets depend on.
const logEnqueueWait = time.Second

// AddLog enqueues a log entry, waiting briefly when the queue is full.
func (s *Store) AddLog(l *RequestLog) {
	s.logMu.RLock()
	defer s.logMu.RUnlock()
	if s.logClosed {
		return
	}
	select {
	case s.logCh <- l:
		return
	default:
	}
	t := time.NewTimer(logEnqueueWait)
	defer t.Stop()
	select {
	case s.logCh <- l:
	case <-t.C:
		s.reportDropped()
	}
}

// DroppedLogs is the number of entries lost to a full queue since start.
func (s *Store) DroppedLogs() int64 { return s.dropped.Load() }

// reportDropped logs dropped entries at most once every 10 seconds.
func (s *Store) reportDropped() {
	n := s.dropped.Add(1)
	now := time.Now().UnixMilli()
	last := s.droppedLog.Load()
	if now-last >= 10_000 && s.droppedLog.CompareAndSwap(last, now) {
		slog.Error("request log queue full", "dropped_total", n)
	}
}

// FlushLogsTimeout is FlushLogs that gives up after d (used on the request
// path, which must not hang on a stalled writer).
func (s *Store) FlushLogsTimeout(d time.Duration) bool {
	done := make(chan struct{})
	timer := time.NewTimer(d)
	defer timer.Stop()
	s.logMu.RLock()
	if s.logClosed {
		s.logMu.RUnlock()
		return false
	}
	select {
	case s.logCh <- &RequestLog{flushed: done}:
	case <-timer.C:
		s.logMu.RUnlock()
		return false
	}
	s.logMu.RUnlock()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
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

// cleanupLoop enforces log retention on its own goroutine: a large
// backlog of deletes or a vacuum must not stall the log writer.
func (s *Store) cleanupLoop() {
	defer close(s.cleanupDone)
	s.cleanupCaptures()
	s.cleanupLogs()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.cleanupStop:
			return
		case <-t.C:
			s.cleanupCaptures()
			s.cleanupSessions()
			s.cleanupLogs()
		}
	}
}

func (s *Store) logWriter() {
	defer close(s.logDone)
	batch := make([]*RequestLog, 0, 1024)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertLogs(batch); err != nil {
			slog.Error("write request logs failed", "err", err)
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
			// bigger batches while a backlog builds up: fewer transactions
			if len(batch) >= 1024 || (len(batch) >= 64 && len(s.logCh) == 0) {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

const logCols = 26

func (s *Store) insertLogs(batch []*RequestLog) error {
	// one multi-row INSERT per batch: a round trip per row would cap the
	// log rate on a networked database
	const perStmt = 256 // * logCols stays under both databases' parameter limits
	row := "(" + placeholders(logCols) + ")"
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	clipped := make([]RequestLog, 0, len(batch))
	for len(batch) > 0 {
		part := batch[:min(perStmt, len(batch))]
		batch = batch[len(part):]
		args := make([]any, 0, len(part)*logCols)
		for _, l := range part {
			// client-supplied strings are capped so a request cannot bloat the log
			l := *l
			l.RequestedModel, l.PublicModel = clip(l.RequestedModel, 256), clip(l.PublicModel, 256)
			l.KeyName, l.ClientIP, l.RequestID = clip(l.KeyName, 128), clip(l.ClientIP, 64), clip(l.RequestID, 64)
			l.Provider, l.UpstreamModel, l.Error = clip(l.Provider, 128), clip(l.UpstreamModel, 256), clip(l.Error, 2000)
			attempts := l.Attempts // never mutate: callers may still read the entry
			if attempts == nil {
				attempts = []Attempt{}
			}
			args = append(args, l.CreatedAt, l.KeyID, l.KeyName, l.RequestedModel, l.PublicModel, l.Inbound, b2i(l.Stream), l.Provider, l.UpstreamModel, l.UpstreamProtocol, b2i(l.Success), l.HTTPStatus, l.LatencyMs, l.TTFBMs, l.InputTokens, l.OutputTokens, l.CachedTokens, b2i(l.Fallback), mustJSON(attempts), l.Error, l.ClientIP, l.Cost, l.Currency, l.CostSource, l.RequestID, l.UserID)
			clipped = append(clipped, l)
		}
		if _, err := tx.Exec(`INSERT INTO request_logs (created_at, key_id, key_name, requested_model, public_model, inbound, stream, provider, upstream_model, upstream_protocol, success, http_status, latency_ms, ttfb_ms, input_tokens, output_tokens, cached_tokens, fallback, attempts, error, client_ip, cost, currency, cost_source, request_id, user_id) VALUES `+
			strings.TrimSuffix(strings.Repeat(row+",", len(part)), ","), args...); err != nil {
			return err
		}
	}
	if err := s.upsertStats(tx, clipped); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) cleanupLogs() {
	days := s.GetSettings().LogRetentionDays
	if days <= 0 {
		return
	}
	if s.cluster != nil && !s.cluster.Claim("log-cleanup", 50*time.Minute) {
		return // another instance does it
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	// small batches: one huge DELETE would hold the write lock for seconds
	var n int64
	for {
		select {
		case <-s.cleanupStop:
			return
		default:
		}
		res, err := s.db.Exec(`DELETE FROM request_logs WHERE id IN (SELECT id FROM request_logs WHERE created_at < ? LIMIT 5000)`, cutoff)
		if err != nil {
			slog.Error("cleanup logs failed", "err", err)
			return
		}
		k, _ := res.RowsAffected()
		n += k
		if k < 5000 {
			break
		}
	}
	statsCutoff := time.Now().Add(-statsRetentionDays * 24 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM request_stats WHERE hour < ?`, statsCutoff); err != nil {
		slog.Error("cleanup stats failed", "err", err)
	}
	if n > 0 && !s.db.pg {
		// give freed pages back to the file system, a bounded slice per
		// run: an unbounded vacuum on a big file holds the write lock for
		// seconds and stalls request logging
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum(8192)`)
		_, _ = s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	}
}

// clip shortens s to at most n bytes without splitting a UTF-8 character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

type LogQuery struct {
	Model    string
	Provider string
	KeyID    int64
	UserID   int64  // > 0: only this user's requests
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
	if q.UserID > 0 {
		conds = append(conds, "user_id = ?")
		args = append(args, q.UserID)
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
	rows, err := s.db.Query(`SELECT id, created_at, key_id, key_name, requested_model, public_model, inbound, stream, provider, upstream_model, upstream_protocol, success, http_status, latency_ms, ttfb_ms, input_tokens, output_tokens, cached_tokens, fallback, attempts, error, client_ip, cost, currency, cost_source, request_id, user_id FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
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
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.KeyID, &l.KeyName, &l.RequestedModel, &l.PublicModel, &l.Inbound, &stream, &l.Provider, &l.UpstreamModel, &l.UpstreamProtocol, &success, &l.HTTPStatus, &l.LatencyMs, &l.TTFBMs, &l.InputTokens, &l.OutputTokens, &l.CachedTokens, &fallback, &attempts, &l.Error, &l.ClientIP, &l.Cost, &l.Currency, &l.CostSource, &l.RequestID, &l.UserID); err != nil {
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

// CurrencyFactors returns what one USD and one CNY are worth in the
// display currency.
func (st Settings) CurrencyFactors() (usd, cny float64) {
	if st.Currency == CurrencyCNY {
		return st.USDToCNY, 1
	}
	return 1, 1 / st.USDToCNY
}

// ToDisplayCurrency converts an amount recorded in currency.
func (st Settings) ToDisplayCurrency(amount float64, currency string) float64 {
	usd, cny := st.CurrencyFactors()
	switch currency {
	case CurrencyUSD:
		return amount * usd
	case CurrencyCNY:
		return amount * cny
	}
	return 0
}
