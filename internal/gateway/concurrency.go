package gateway

import (
	"context"
	"log"
	"sync"
	"time"

	"ai-route/internal/store"
)

// slots counts in-flight requests per provider for providers with a
// concurrency cap (self-hosted servers). With several instances the counts
// are shared through the database (cluster).
type slots struct {
	cluster *store.Cluster

	mu      sync.Mutex
	inUse   map[string]int
	changed chan struct{} // closed and replaced whenever a slot is released
}

func newSlots(c *store.Cluster) *slots {
	return &slots{cluster: c, inUse: map[string]int{}, changed: make(chan struct{})}
}

func providerSlot(prefix string) string { return "prov:" + prefix }

// tryAcquire takes a slot unless limit (> 0) slots are already in use.
func (s *slots) tryAcquire(prefix string, limit int) bool {
	if s.cluster != nil && limit > 0 {
		ok, err := s.cluster.Acquire(providerSlot(prefix), limit)
		if err != nil {
			log.Printf("provider concurrency: %v", err)
		}
		if !ok {
			return false
		}
	} else {
		s.mu.Lock()
		defer s.mu.Unlock()
		if limit > 0 && s.inUse[prefix] >= limit {
			return false
		}
	}
	if s.cluster != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	s.inUse[prefix]++
	return true
}

func (s *slots) release(prefix string) {
	if s.cluster != nil {
		s.cluster.Release(providerSlot(prefix))
	}
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
// and returns the index of the one it got, or -1. Slots freed by other
// instances are noticed by polling.
func (s *slots) acquireAny(ctx context.Context, cands []candidate, timeout time.Duration) int {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var poll <-chan time.Time
	if s.cluster != nil {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		poll = t.C
	}
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
		case <-poll:
		case <-deadline.C:
			return -1
		case <-ctx.Done():
			return -1
		}
	}
}

// snapshot returns the in-flight count per provider (over all instances).
func (s *slots) snapshot() map[string]int {
	if s.cluster != nil {
		if m, err := s.cluster.SlotTotals("prov:"); err == nil {
			return m
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.inUse))
	for k, v := range s.inUse {
		out[k] = v
	}
	return out
}
