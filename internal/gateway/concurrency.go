package gateway

import (
	"context"
	"sync"
	"time"
)

// slots counts in-flight requests per provider for providers with a
// concurrency cap (self-hosted servers).
type slots struct {
	mu      sync.Mutex
	inUse   map[string]int
	changed chan struct{} // closed and replaced whenever a slot is released
}

func newSlots() *slots {
	return &slots{inUse: map[string]int{}, changed: make(chan struct{})}
}

// tryAcquire takes a slot unless limit (> 0) slots are already in use.
func (s *slots) tryAcquire(prefix string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > 0 && s.inUse[prefix] >= limit {
		return false
	}
	s.inUse[prefix]++
	return true
}

func (s *slots) release(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[prefix] > 0 {
		s.inUse[prefix]--
	}
	if s.inUse[prefix] == 0 {
		delete(s.inUse, prefix)
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// acquireAny waits up to timeout for a free slot on any of the candidates
// and returns the index of the one it got, or -1.
func (s *slots) acquireAny(ctx context.Context, cands []candidate, timeout time.Duration) int {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		for i, c := range cands {
			if s.tryAcquire(c.prefix, c.provider.MaxConcurrency) {
				return i
			}
		}
		select {
		case <-changed:
		case <-deadline.C:
			return -1
		case <-ctx.Done():
			return -1
		}
	}
}

// snapshot returns the in-flight count per provider.
func (s *slots) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.inUse))
	for k, v := range s.inUse {
		out[k] = v
	}
	return out
}
