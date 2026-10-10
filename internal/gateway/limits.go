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

// Limiter enforces per-key RPM / TPM / monthly budget / concurrency, and
// the RPM / TPM / budget of the user owning a key over all their keys.
// State is in memory, or shared through the database when several
// instances run (cluster); the monthly spend is reloaded from the logs.
type Limiter struct {
	store   *store.Store
	cluster *store.Cluster
	now     func() time.Time

	mu   sync.Mutex
	keys map[string]*keyUsage // by subject id
	// spendMu serializes reloads of the month-to-date spend: when the
	// cached value expires, one query refreshes every key instead of each
	// concurrent request scanning the logs
	spendMu sync.Mutex
}

func NewLimiter(s *store.Store) *Limiter {
	return &Limiter{store: s, cluster: s.Cluster(), now: time.Now, keys: map[string]*keyUsage{}}
}

// subject is what a set of limits applies to: an API key, or the user
// owning it.
type subject struct {
	id     string // "12" for key 12, "u3" for user 3: state and cluster window names
	name   string // for warnings
	whom   string // in rejection messages
	rpm    int
	tpm    int
	budget float64
}

func (s subject) limited() bool { return s.rpm > 0 || s.tpm > 0 || s.budget > 0 }

func subjects(k *store.APIKey, u *store.User) []subject {
	out := []subject{{id: strconv.FormatInt(k.ID, 10), name: k.Name, whom: "this API key", rpm: k.RPM, tpm: k.TPM, budget: k.MonthlyBudget}}
	if u != nil {
		out = append(out, subject{id: "u" + strconv.FormatInt(u.ID, 10), name: u.Username, whom: "your account", rpm: u.RPM, tpm: u.TPM, budget: u.MonthlyBudget})
	}
	return out
}

func (l *Limiter) usage(id string) *keyUsage {
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

// Admit checks the limits of the key and of its owner (u, nil for the
// administrator's keys) and, when admitted, counts the request towards
// their RPM.
func (l *Limiter) Admit(k *store.APIKey, u *store.User) *rejection {
	subs := subjects(k, u)
	limited := false
	for _, s := range subs {
		limited = limited || s.limited()
	}
	if !limited {
		return nil
	}
	now := l.now()
	for _, s := range subs {
		if s.budget > 0 {
			if spend := l.monthSpend(s.id, now); spend >= s.budget {
				cur := l.store.GetSettings().Currency
				return &rejection{status: http.StatusPaymentRequired,
					msg: fmt.Sprintf("monthly budget exhausted for %s (%.2f / %.2f %s); it resets on the 1st", s.whom, spend, s.budget, cur)}
			}
		}
	}
	if l.cluster != nil {
		for _, s := range subs {
			if rej := l.admitShared(s, now.Add(l.cluster.Offset())); rej != nil { // windows use the database clock
				return rej
			}
		}
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	for _, s := range subs {
		u := l.usage(s.id)
		for len(u.reqs) > 0 && !u.reqs[0].After(cutoff) {
			u.reqs = u.reqs[1:]
		}
		for len(u.tokens) > 0 && !u.tokens[0].at.After(cutoff) {
			u.tokens = u.tokens[1:]
		}
		if s.rpm > 0 && len(u.reqs) >= s.rpm {
			return rpmRejection(s, u.reqs[len(u.reqs)-s.rpm].Add(time.Minute).Sub(now))
		}
		if s.tpm > 0 {
			var used int64
			for _, e := range u.tokens {
				used += e.tokens
			}
			if used >= int64(s.tpm) {
				// wait until enough of the window expires to get back under the limit
				wait := time.Minute
				for _, e := range u.tokens {
					used -= e.tokens
					if used < int64(s.tpm) {
						wait = e.at.Add(time.Minute).Sub(now)
						break
					}
				}
				return tpmRejection(s, wait)
			}
		}
	}
	// admitted by every subject: count the request for each
	for _, s := range subs {
		if s.rpm > 0 {
			u := l.usage(s.id)
			u.reqs = append(u.reqs, now)
		}
	}
	return nil
}

func rpmRejection(s subject, retry time.Duration) *rejection {
	return &rejection{status: http.StatusTooManyRequests, retryAfter: retry,
		msg: fmt.Sprintf("rate limit exceeded for %s: %d requests per minute", s.whom, s.rpm)}
}

func tpmRejection(s subject, retry time.Duration) *rejection {
	return &rejection{status: http.StatusTooManyRequests, retryAfter: retry,
		msg: fmt.Sprintf("token rate limit exceeded for %s: %d tokens per minute", s.whom, s.tpm)}
}

// admitShared is Admit's RPM / TPM check against counters shared by every
// instance. When the database cannot be reached the request is admitted.
func (l *Limiter) admitShared(s subject, now time.Time) *rejection {
	if s.tpm > 0 {
		buckets, err := l.cluster.Window("tpm:"+s.id, now)
		if err != nil {
			slog.Warn("rate limit: token window check failed", "subject", s.name, "err", err)
		} else if store.WindowSum(buckets) >= int64(s.tpm) {
			return tpmRejection(s, store.WindowRetry(buckets, int64(s.tpm), now))
		}
	}
	if s.rpm > 0 {
		ok, buckets, err := l.cluster.TakeWindow("rpm:"+s.id, int64(s.rpm), now)
		if err != nil {
			slog.Warn("rate limit: request window check failed", "subject", s.name, "err", err)
		} else if !ok {
			return rpmRejection(s, store.WindowRetry(buckets, int64(s.rpm), now))
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
	u := l.usage(strconv.FormatInt(k.ID, 10))
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
	if u := l.usage(strconv.FormatInt(k.ID, 10)); u.inflight > 0 {
		u.inflight--
	}
}

// Record adds a finished request's tokens and cost to the key and its owner.
func (l *Limiter) Record(k *store.APIKey, owner *store.User, e *store.RequestLog) {
	if e == nil {
		return
	}
	now := l.now()
	cost := l.store.GetSettings().ToDisplayCurrency(e.Cost, e.Currency)
	t := e.InputTokens + e.OutputTokens
	for _, s := range subjects(k, owner) {
		if s.tpm <= 0 && s.budget <= 0 {
			continue
		}
		if l.cluster != nil && s.tpm > 0 && t > 0 {
			if err := l.cluster.AddWindow("tpm:"+s.id, t, now.Add(l.cluster.Offset())); err != nil {
				slog.Warn("rate limit: recording tokens failed", "subject", s.name, "err", err)
			}
		}
		l.mu.Lock()
		u := l.usage(s.id)
		if l.cluster == nil && s.tpm > 0 && t > 0 {
			u.tokens = append(u.tokens, tokenEvent{at: now, tokens: t})
		}
		if u.spendMonth == store.MonthStart(now) {
			u.spend += cost
		}
		l.mu.Unlock()
	}
}

// monthSpend returns the key's month-to-date spend, reloading it from the
// logs when stale.
func (l *Limiter) monthSpend(id string, now time.Time) float64 {
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
	spend := map[string]float64{}
	keys, err := l.store.KeySpend(month)
	if err == nil {
		var users map[int64]float64
		users, err = l.store.UserSpend(month)
		for id, v := range keys {
			spend[strconv.FormatInt(id, 10)] = v
		}
		for id, v := range users {
			spend["u"+strconv.FormatInt(id, 10)] = v
		}
	}
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
