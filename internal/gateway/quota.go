package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ai-route/internal/store"
)

// QuotaUsage is one quota of a provider and how much of it is used.
type QuotaUsage struct {
	store.Quota
	Since        int64 `json:"since"` // window start, unix ms
	UsedRequests int64 `json:"used_requests"`
	UsedTokens   int64 `json:"used_tokens"`
	Over         bool  `json:"over"`
}

func (u *QuotaUsage) check() {
	u.Over = u.Requests > 0 && u.UsedRequests >= u.Requests || u.Tokens > 0 && u.UsedTokens >= u.Tokens
}

// quotaRefresh is how often usage is recounted from the database; in
// between, requests finished on this instance are added in memory.
const quotaRefresh = 30 * time.Second

// Quotas tracks providers' plan allowances (store.Quota). Routing only
// reads the cached state; a background loop recounts it.
type Quotas struct {
	store *store.Store
	now   func() time.Time

	refreshMu sync.Mutex // one recount at a time
	mu        sync.Mutex
	usage     map[string][]QuotaUsage // by provider prefix
	loaded    time.Time
}

func newQuotas(s *store.Store) *Quotas {
	return &Quotas{store: s, now: time.Now, usage: map[string][]QuotaUsage{}}
}

// Over reports whether a provider has used up one of its quotas.
func (q *Quotas) Over(prefix string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, u := range q.usage[prefix] {
		if u.Over {
			return true
		}
	}
	return false
}

// Record counts a finished request towards its provider's quotas until
// the next recount.
func (q *Quotas) Record(e *store.RequestLog) {
	if e == nil || e.Provider == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	us := q.usage[e.Provider]
	for i := range us {
		us[i].UsedRequests++
		us[i].UsedTokens += e.InputTokens + e.OutputTokens
		us[i].check()
	}
}

// Refresh recounts every quota from the request logs.
func (q *Quotas) Refresh() {
	q.refreshMu.Lock()
	defer q.refreshMu.Unlock()
	now := q.now()
	next := map[string][]QuotaUsage{}
	flushed := false
	for prefix, p := range q.store.Snapshot().Providers {
		if len(p.Quotas) == 0 {
			continue
		}
		if !flushed {
			q.store.FlushLogsTimeout(2 * time.Second) // count what has just finished
			flushed = true
		}
		var us []QuotaUsage
		for _, quota := range p.Quotas {
			u := QuotaUsage{Quota: quota, Since: store.QuotaStart(quota.Period, now)}
			var err error
			if u.UsedRequests, u.UsedTokens, err = q.store.ProviderUsage(prefix, u.Since); err != nil {
				slog.Warn("quota: counting usage failed", "provider", prefix, "err", err)
				q.mu.Lock()
				us = q.usage[prefix] // keep the last known state
				q.mu.Unlock()
				break
			}
			u.check()
			us = append(us, u)
		}
		next[prefix] = us
	}
	q.mu.Lock()
	q.usage, q.loaded = next, now
	q.mu.Unlock()
}

// Status returns the quota usage of every provider that has quotas,
// recounting first when the cached state is a few seconds old.
func (q *Quotas) Status() map[string][]QuotaUsage {
	q.mu.Lock()
	stale := q.now().Sub(q.loaded) > 5*time.Second
	q.mu.Unlock()
	if stale {
		q.Refresh()
	}
	return q.snapshot()
}

func (q *Quotas) snapshot() map[string][]QuotaUsage {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string][]QuotaUsage, len(q.usage))
	for k, v := range q.usage {
		out[k] = append([]QuotaUsage(nil), v...)
	}
	return out
}

// Run recounts the quotas periodically until ctx is done.
func (q *Quotas) Run(ctx context.Context) {
	t := time.NewTicker(quotaRefresh)
	defer t.Stop()
	q.Refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.Refresh()
		}
	}
}
