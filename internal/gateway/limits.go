package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ai-route/internal/store"
)

// spendTTL is how long a key's month-to-date spend loaded from the logs is
// trusted; in between, completed requests are added in memory. With several
// instances the others' spending only shows up on reload, so it is shorter.
const (
	spendTTL        = 5 * time.Minute
	clusterSpendTTL = 30 * time.Second
)

type tokenEvent struct {
	at     time.Time
	tokens int64
}

type keyUsage struct {
	reqs   []time.Time  // request starts within the last minute
	tokens []tokenEvent // tokens used by requests finished within the last minute

	spend       float64 // month-to-date cost in the display currency
	spendMonth  int64   // MonthStart the spend belongs to
	spendLoaded time.Time

	inflight int // requests in progress (counted only for keys with MaxConcurrency)
}

// Limiter enforces per-key RPM / TPM / monthly budget / concurrency. State
// is in memory, or shared through the database when several instances run
// (cluster); the monthly spend is reloaded from the request logs.
type Limiter struct {
	store   *store.Store
	cluster *store.Cluster
	now     func() time.Time

	mu   sync.Mutex
	keys map[int64]*keyUsage
	// spendMu serializes reloads of the month-to-date spend: when the
	// cached value expires, one query refreshes every key instead of each
	// concurrent request scanning the logs
	spendMu sync.Mutex
}

func NewLimiter(s *store.Store) *Limiter {
	return &Limiter{store: s, cluster: s.Cluster(), now: time.Now, keys: map[int64]*keyUsage{}}
}

func (l *Limiter) usage(id int64) *keyUsage {
	u, ok := l.keys[id]
	if !ok {
		u = &keyUsage{}
		l.keys[id] = u
	}
	return u
}

// rejection describes why a request was refused.
type rejection struct {
	status     int
	msg        string
	retryAfter time.Duration
}

// Admit checks the key's limits and, when admitted, counts the request
// towards its RPM.
func (l *Limiter) Admit(k *store.APIKey) *rejection {
	if k.RPM <= 0 && k.TPM <= 0 && k.MonthlyBudget <= 0 {
		return nil
	}
	now := l.now()
	if k.MonthlyBudget > 0 {
		spend := l.monthSpend(k.ID, now)
		if spend >= k.MonthlyBudget {
			cur := l.store.GetSettings().Currency
			return &rejection{status: http.StatusPaymentRequired,
				msg: fmt.Sprintf("monthly budget exhausted for this API key (%.2f / %.2f %s); it resets on the 1st", spend, k.MonthlyBudget, cur)}
		}
	}
	if l.cluster != nil {
		return l.admitShared(k, now.Add(l.cluster.Offset())) // windows use the database clock
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(k.ID)
	cutoff := now.Add(-time.Minute)
	for len(u.reqs) > 0 && !u.reqs[0].After(cutoff) {
		u.reqs = u.reqs[1:]
	}
	for len(u.tokens) > 0 && !u.tokens[0].at.After(cutoff) {
		u.tokens = u.tokens[1:]
	}
	if k.RPM > 0 && len(u.reqs) >= k.RPM {
		return &rejection{status: http.StatusTooManyRequests, retryAfter: u.reqs[len(u.reqs)-k.RPM].Add(time.Minute).Sub(now),
			msg: fmt.Sprintf("rate limit exceeded for this API key: %d requests per minute", k.RPM)}
	}
	if k.TPM > 0 {
		var used int64
		for _, e := range u.tokens {
			used += e.tokens
		}
		if used >= int64(k.TPM) {
			// wait until enough of the window expires to get back under the limit
			wait := time.Minute
			for _, e := range u.tokens {
				used -= e.tokens
				if used < int64(k.TPM) {
					wait = e.at.Add(time.Minute).Sub(now)
					break
				}
			}
			return &rejection{status: http.StatusTooManyRequests, retryAfter: wait,
				msg: fmt.Sprintf("token rate limit exceeded for this API key: %d tokens per minute", k.TPM)}
		}
	}
	if k.RPM > 0 {
		u.reqs = append(u.reqs, now)
	}
	return nil
}

func rpmRejection(k *store.APIKey, retry time.Duration) *rejection {
	return &rejection{status: http.StatusTooManyRequests, retryAfter: retry,
		msg: fmt.Sprintf("rate limit exceeded for this API key: %d requests per minute", k.RPM)}
}

func tpmRejection(k *store.APIKey, retry time.Duration) *rejection {
	return &rejection{status: http.StatusTooManyRequests, retryAfter: retry,
		msg: fmt.Sprintf("token rate limit exceeded for this API key: %d tokens per minute", k.TPM)}
}

// admitShared is Admit's RPM / TPM check against counters shared by every
// instance. When the database cannot be reached the request is admitted.
func (l *Limiter) admitShared(k *store.APIKey, now time.Time) *rejection {
	id := strconv.FormatInt(k.ID, 10)
	if k.TPM > 0 {
		buckets, err := l.cluster.Window("tpm:"+id, now)
		if err != nil {
			slog.Warn("rate limit: token window check failed", "key", k.Name, "err", err)
		} else if store.WindowSum(buckets) >= int64(k.TPM) {
			return tpmRejection(k, store.WindowRetry(buckets, int64(k.TPM), now))
		}
	}
	if k.RPM > 0 {
		ok, buckets, err := l.cluster.TakeWindow("rpm:"+id, int64(k.RPM), now)
		if err != nil {
			slog.Warn("rate limit: request window check failed", "key", k.Name, "err", err)
		} else if !ok {
			return rpmRejection(k, store.WindowRetry(buckets, int64(k.RPM), now))
		}
	}
	return nil
}

func keySlot(id int64) string { return "key:" + strconv.FormatInt(id, 10) }

func concurrencyRejection(k *store.APIKey) *rejection {
	return &rejection{status: http.StatusTooManyRequests, retryAfter: time.Second,
		msg: fmt.Sprintf("too many concurrent requests for this API key: limit %d", k.MaxConcurrency)}
}

// Enter takes an in-flight slot for keys with MaxConcurrency; every nil
// return must be paired with Leave.
func (l *Limiter) Enter(k *store.APIKey) *rejection {
	if k.MaxConcurrency <= 0 {
		return nil
	}
	if l.cluster != nil {
		if ok, err := l.cluster.Acquire(keySlot(k.ID), k.MaxConcurrency); err != nil {
			slog.Warn("rate limit: key concurrency check failed", "key", k.Name, "err", err)
		} else if !ok {
			return concurrencyRejection(k)
		}
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(k.ID)
	if u.inflight >= k.MaxConcurrency {
		return concurrencyRejection(k)
	}
	u.inflight++
	return nil
}

// Leave releases a slot taken by Enter.
func (l *Limiter) Leave(k *store.APIKey) {
	if k.MaxConcurrency <= 0 {
		return
	}
	if l.cluster != nil {
		l.cluster.Release(keySlot(k.ID))
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if u := l.usage(k.ID); u.inflight > 0 {
		u.inflight--
	}
}

// Record adds a finished request's tokens and cost.
func (l *Limiter) Record(k *store.APIKey, e *store.RequestLog) {
	if e == nil || k.TPM <= 0 && k.MonthlyBudget <= 0 {
		return
	}
	now := l.now()
	cost := l.store.GetSettings().ToDisplayCurrency(e.Cost, e.Currency)
	t := e.InputTokens + e.OutputTokens
	if l.cluster != nil && k.TPM > 0 && t > 0 {
		if err := l.cluster.AddWindow("tpm:"+strconv.FormatInt(k.ID, 10), t, now.Add(l.cluster.Offset())); err != nil {
			slog.Warn("rate limit: recording tokens failed", "key", k.Name, "err", err)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(k.ID)
	if l.cluster == nil && k.TPM > 0 && t > 0 {
		u.tokens = append(u.tokens, tokenEvent{at: now, tokens: t})
	}
	if u.spendMonth == store.MonthStart(now) {
		u.spend += cost
	}
}

// monthSpend returns the key's month-to-date spend, reloading it from the
// logs when stale.
func (l *Limiter) monthSpend(id int64, now time.Time) float64 {
	month := store.MonthStart(now)
	l.mu.Lock()
	u := l.usage(id)
	ttl := spendTTL
	if l.cluster != nil {
		ttl = clusterSpendTTL
	}
	if u.spendMonth == month && now.Sub(u.spendLoaded) < ttl {
		v := u.spend
		l.mu.Unlock()
		return v
	}
	l.mu.Unlock()

	l.spendMu.Lock()
	defer l.spendMu.Unlock()
	l.mu.Lock()
	if u.spendMonth == month && now.Sub(u.spendLoaded) < ttl {
		v := u.spend // another request reloaded it while we waited
		l.mu.Unlock()
		return v
	}
	l.mu.Unlock()

	l.store.FlushLogsTimeout(2 * time.Second) // so the query sees every finished request
	spend, err := l.store.KeySpend(month)
	if err != nil {
		slog.Warn("rate limit: loading month-to-date spend failed", "err", err)
		l.mu.Lock()
		defer l.mu.Unlock()
		return u.spend
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// the query covers every key: refresh them all
	for kid, ku := range l.keys {
		ku.spend, ku.spendMonth, ku.spendLoaded = spend[kid], month, now
	}
	for kid, v := range spend {
		if _, ok := l.keys[kid]; !ok {
			l.keys[kid] = &keyUsage{spend: v, spendMonth: month, spendLoaded: now}
		}
	}
	return u.spend
}

// Forget drops cached state, e.g. after settings (currency) change.
func (l *Limiter) Forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, u := range l.keys {
		u.spendLoaded = time.Time{}
	}
}
