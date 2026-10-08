package gateway

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"ai-route/internal/store"
)

// spendTTL is how long a key's month-to-date spend loaded from the logs is
// trusted; in between, completed requests are added in memory.
const spendTTL = 5 * time.Minute

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
}

// Limiter enforces per-key RPM / TPM / monthly budget. State is in memory;
// the monthly spend is reloaded from the request logs.
type Limiter struct {
	store *store.Store
	now   func() time.Time

	mu   sync.Mutex
	keys map[int64]*keyUsage
}

func NewLimiter(s *store.Store) *Limiter {
	return &Limiter{store: s, now: time.Now, keys: map[int64]*keyUsage{}}
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

// Record adds a finished request's tokens and cost.
func (l *Limiter) Record(k *store.APIKey, e *store.RequestLog) {
	if e == nil || k.TPM <= 0 && k.MonthlyBudget <= 0 {
		return
	}
	now := l.now()
	cost := l.store.GetSettings().ToDisplayCurrency(e.Cost, e.Currency)
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(k.ID)
	if t := e.InputTokens + e.OutputTokens; k.TPM > 0 && t > 0 {
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
	if u.spendMonth == month && now.Sub(u.spendLoaded) < spendTTL {
		v := u.spend
		l.mu.Unlock()
		return v
	}
	l.mu.Unlock()

	l.store.FlushLogs() // so the query sees every finished request
	spend, err := l.store.KeySpend(month)
	if err != nil {
		log.Printf("load key spend: %v", err)
		l.mu.Lock()
		defer l.mu.Unlock()
		return u.spend
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u.spend, u.spendMonth, u.spendLoaded = spend[id], month, now
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
