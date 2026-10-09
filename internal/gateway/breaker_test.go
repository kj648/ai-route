package gateway

import (
	"testing"
	"time"

	"ai-route/internal/store"
)

func testBreaker() *Breaker {
	return NewBreaker(func() store.Settings {
		return store.Settings{FailureThreshold: 2, CooldownSeconds: 60, MaxCooldownSeconds: 1800}
	})
}

func TestBreakerStaleSuccessKeepsCooldown(t *testing.T) {
	b := testBreaker()
	started := time.Now().Add(-time.Second) // a request already in flight
	b.Failure("kimi", "kimi/k2", failProviderHard, 0, "HTTP 401")
	if b.OpenUntil("kimi", "kimi/k2").IsZero() {
		t.Fatal("expected the provider to be cooling")
	}
	b.Success("kimi", "kimi/k2", started)
	if b.OpenUntil("kimi", "kimi/k2").IsZero() {
		t.Fatal("a success that started before the 401 must not clear the cooldown")
	}
	b.Success("kimi", "kimi/k2", time.Now())
	if !b.OpenUntil("kimi", "kimi/k2").IsZero() {
		t.Fatal("a success after the cooldown began clears it")
	}
}

func TestBreakerApplyIgnoresStaleClear(t *testing.T) {
	b := testBreaker()
	var published []store.Cooldown
	b.publish = func(c store.Cooldown) { published = append(published, c) }
	b.Failure("glm", "glm/g5", failProviderHard, 0, "HTTP 402")
	if len(published) != 1 || published[0].OpenUntil.IsZero() {
		t.Fatalf("published: %+v", published)
	}
	// another instance reports a success whose request began before our cooldown
	b.Apply([]store.Cooldown{{Key: "p:glm", At: time.Now().Add(-time.Minute)}})
	if b.OpenUntil("glm", "glm/g5").IsZero() {
		t.Fatal("stale clear from a peer must not reopen the provider")
	}
	b.Apply([]store.Cooldown{{Key: "p:glm", At: time.Now()}})
	if !b.OpenUntil("glm", "glm/g5").IsZero() {
		t.Fatal("a fresh clear from a peer closes the cooldown")
	}
	// an explicit reset always wins
	b.Failure("glm", "glm/g5", failProviderHard, 0, "HTTP 402")
	b.Apply([]store.Cooldown{{Key: "*"}})
	if !b.OpenUntil("glm", "glm/g5").IsZero() {
		t.Fatal("reset-all must clear everything")
	}
}

func TestBreakerSuccessPublishesStartTime(t *testing.T) {
	b := testBreaker()
	var published []store.Cooldown
	b.publish = func(c store.Cooldown) { published = append(published, c) }
	b.Failure("x", "x/m", failTargetHard, 0, "HTTP 404")
	started := time.Now()
	b.Success("x", "x/m", started)
	if len(published) != 2 || !published[1].OpenUntil.IsZero() || !published[1].At.Equal(started) {
		t.Fatalf("published: %+v", published)
	}
}
