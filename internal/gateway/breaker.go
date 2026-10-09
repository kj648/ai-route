package gateway

import (
	"sort"
	"strings"
	"sync"
	"time"

	"ai-route/internal/store"
)

// failKind classifies an upstream failure for the circuit breaker.
type failKind int

const (
	failIgnore       failKind = iota // request-specific (e.g. 400): fail over but do not penalize
	failSoft                         // counts towards FailureThreshold for the target
	failTargetHard                   // cool down the target immediately (e.g. model not found)
	failProviderHard                 // cool down the whole provider (quota / auth problems)
)

// classifyResponse refines classifyStatus with the response: a 429 or 404
// caused by the request itself (too large, context too long) must not let
// one client cool down a provider everybody uses, and a short 429 is a
// passing rate limit rather than an exhausted plan.
func classifyResponse(status int, retryAfter time.Duration, body []byte) failKind {
	if status == 429 || status == 404 || status == 413 {
		lower := strings.ToLower(string(body))
		for _, hint := range []string{"too large", "too long", "context_length", "context length", "maximum context", "max_tokens", "reduce the length"} {
			if strings.Contains(lower, hint) {
				return failIgnore
			}
		}
	}
	if status == 429 && retryAfter <= maxRetryAfter {
		return failSoft
	}
	return classifyStatus(status)
}

func classifyStatus(status int) failKind {
	switch {
	case status == 401 || status == 402 || status == 429:
		return failProviderHard
	case status == 403:
		// often client-specific (e.g. Kimi Code rejects non-whitelisted
		// User-Agents): fail over, but don't cool the plan down for everyone
		return failIgnore
	case status == 404:
		return failTargetHard
	case status == 408 || status >= 500:
		return failSoft
	default:
		return failIgnore
	}
}

type breakerEntry struct {
	Key           string    `json:"key"`
	Failures      int       `json:"failures"`   // consecutive soft failures
	Opens         int       `json:"opens"`      // consecutive cooldowns (for backoff)
	OpenUntil     time.Time `json:"open_until"` // zero = closed
	LastError     string    `json:"last_error"`
	LastErrorAt   time.Time `json:"last_error_at"`
	LastSuccessAt time.Time `json:"last_success_at"`
	TotalSuccess  int64     `json:"total_success"`
	TotalFailure  int64     `json:"total_failure"`
	// Down: the provider fails its active health check; it stays out of
	// rotation (tried last) until a check passes.
	Down bool `json:"down,omitempty"`
}

// downFor is how far in the future a down provider's OpenUntil is reported,
// which sorts it behind every cooling target.
const downFor = 24 * time.Hour

// Breaker tracks health per provider ("p:<prefix>") and per target
// ("t:<prefix>/<model>"). State is in memory; with several instances,
// cooldowns opened or cleared here are passed to publish and the other
// instances' changes arrive through Apply.
type Breaker struct {
	mu       sync.Mutex
	entries  map[string]*breakerEntry
	settings func() store.Settings
	publish  func(store.Cooldown) // nil with a single instance; must not block
}

func NewBreaker(settings func() store.Settings) *Breaker {
	return &Breaker{entries: map[string]*breakerEntry{}, settings: settings}
}

func providerKey(prefix string) string { return "p:" + prefix }
func targetKey(target string) string   { return "t:" + target }

func (b *Breaker) entry(key string) *breakerEntry {
	e, ok := b.entries[key]
	if !ok {
		e = &breakerEntry{Key: key}
		b.entries[key] = e
	}
	return e
}

// OpenUntil returns when the target becomes available again (zero if available now).
func (b *Breaker) OpenUntil(prefix, target string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var until time.Time
	for _, k := range []string{providerKey(prefix), targetKey(target)} {
		e, ok := b.entries[k]
		if !ok {
			continue
		}
		open := e.OpenUntil
		if e.Down {
			open = now.Add(downFor)
		}
		if open.After(now) && open.After(until) {
			until = open
		}
	}
	return until
}

// SetDown marks a provider as failing (or passing) its health check and
// reports whether the state changed.
func (b *Breaker) SetDown(prefix string, down bool, msg string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entry(providerKey(prefix))
	changed := e.Down != down
	e.Down = down
	if down {
		e.LastError, e.LastErrorAt = msg, time.Now()
	}
	return changed
}

func (b *Breaker) Success(prefix, target string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for _, k := range []string{providerKey(prefix), targetKey(target)} {
		e := b.entry(k)
		if b.publish != nil && (e.Opens > 0 || !e.OpenUntil.IsZero()) {
			b.publish(store.Cooldown{Key: k, At: now}) // recovered: others reset their backoff too
		}
		e.Failures, e.Opens = 0, 0
		e.OpenUntil = time.Time{}
		e.LastSuccessAt = now
		e.TotalSuccess++
	}
}

// Failure records a failed request on a target. When it cools something
// down, it returns the entry key ("p:<prefix>" or "t:<target>") and the
// cooldown length.
func (b *Breaker) Failure(prefix, target string, kind failKind, retryAfter time.Duration, msg string) (string, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.settings()
	now := time.Now()
	t := b.entry(targetKey(target))
	t.LastError, t.LastErrorAt = msg, now
	t.TotalFailure++
	var open *breakerEntry
	switch kind {
	case failIgnore:
		return "", 0
	case failSoft:
		t.Failures++
		if t.Failures >= st.FailureThreshold {
			open = t
		}
	case failTargetHard:
		open = t
	case failProviderHard:
		p := b.entry(providerKey(prefix))
		p.LastError, p.LastErrorAt = msg, now
		p.TotalFailure++
		open = p
	}
	if open == nil {
		return "", 0
	}
	open.Opens++
	open.Failures = 0
	cooldown := time.Duration(st.CooldownSeconds) * time.Second
	for i := 1; i < open.Opens && cooldown < time.Duration(st.MaxCooldownSeconds)*time.Second; i++ {
		cooldown *= 2
	}
	if max := time.Duration(st.MaxCooldownSeconds) * time.Second; cooldown > max {
		cooldown = max
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
		if cooldown > 6*time.Hour {
			cooldown = 6 * time.Hour
		}
	}
	open.OpenUntil = now.Add(cooldown)
	if b.publish != nil {
		b.publish(store.Cooldown{Key: open.Key, OpenUntil: open.OpenUntil, Opens: open.Opens, Error: msg, At: now})
	}
	return open.Key, cooldown
}

// Apply takes cooldowns opened or cleared by other instances.
func (b *Breaker) Apply(changes []store.Cooldown) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range changes {
		if c.Key == "*" {
			b.resetLocked("")
			continue
		}
		e := b.entry(c.Key)
		if c.OpenUntil.IsZero() {
			e.Failures, e.Opens, e.OpenUntil = 0, 0, time.Time{}
			continue
		}
		if c.OpenUntil.After(e.OpenUntil) {
			e.OpenUntil = c.OpenUntil
		}
		if c.Opens > e.Opens {
			e.Opens = c.Opens
		}
		e.Failures = 0
		if c.Error != "" {
			e.LastError, e.LastErrorAt = c.Error, c.At
		}
	}
}

// Reset clears one entry (or all entries when key is empty), on every
// instance.
func (b *Breaker) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetLocked(key)
	if b.publish != nil {
		k := key
		if k == "" {
			k = "*"
		}
		b.publish(store.Cooldown{Key: k, At: time.Now()})
	}
}

func (b *Breaker) resetLocked(key string) {
	if key == "" {
		b.entries = map[string]*breakerEntry{}
		return
	}
	delete(b.entries, key)
}

type BreakerStatus struct {
	breakerEntry
	Kind            string `json:"kind"` // provider | target
	Name            string `json:"name"`
	Open            bool   `json:"open"`
	RemainingSecond int64  `json:"remaining_seconds"`
}

func (b *Breaker) Status() []BreakerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	out := make([]BreakerStatus, 0, len(b.entries))
	for k, e := range b.entries {
		s := BreakerStatus{breakerEntry: *e}
		if strings.HasPrefix(k, "p:") {
			s.Kind, s.Name = "provider", k[2:]
		} else {
			s.Kind, s.Name = "target", k[2:]
		}
		if e.OpenUntil.After(now) {
			s.Open = true
			s.RemainingSecond = int64(e.OpenUntil.Sub(now).Seconds()) + 1
		}
		if e.Down {
			s.Open = true
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind > out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}
