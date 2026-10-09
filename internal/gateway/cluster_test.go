package gateway

import (
	"strings"
	"sync"
	"testing"
	"time"

	"ai-route/internal/store"
)

// Several instances on one PostgreSQL database share limits, slots, breaker
// cooldowns, configuration and alerts. These tests run only when
// AI_ROUTE_TEST_DATABASE_URL is set.

func clusterPair(t *testing.T) (*harness, *harness) {
	t.Helper()
	a := newHarness(t)
	if a.st.Cluster() == nil {
		t.Skip("multi-instance tests need PostgreSQL (AI_ROUTE_TEST_DATABASE_URL)")
	}
	return a, a.peer()
}

// eventually polls until ok: changes reach other instances within a poll
// interval (store.PollEvery).
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * store.PollEvery)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", 3*store.PollEvery, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestClusterConfigPropagates(t *testing.T) {
	a, b := clusterPair(t)
	a.model("coder", "oa/ok")
	if resp, _ := b.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 404 {
		t.Fatalf("b knew the model before polling: %d", resp.StatusCode)
	}
	b.gw.ClusterPoll()
	if resp, body := b.post("/v1/chat/completions", oaReq("coder", false)); resp.StatusCode != 200 {
		t.Fatalf("b after poll: %d %s", resp.StatusCode, body)
	}
}

func TestClusterSharedRPM(t *testing.T) {
	a, b := clusterPair(t)
	a.model("coder", "oa/ok")
	key := a.limitedKey(store.APIKey{RPM: 3})
	b.gw.ClusterPoll()
	for i, h := range []*harness{a, b, a} {
		if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	for _, h := range []*harness{b, a} {
		if code, retry, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 429 || retry == "" || !strings.Contains(body, "3 requests per minute") {
			t.Fatalf("4th request across instances: %d %q %s", code, retry, body)
		}
	}
}

func TestClusterSharedTPM(t *testing.T) {
	a, b := clusterPair(t)
	a.model("coder", "oa/ok") // 15 tokens per request
	key := a.limitedKey(store.APIKey{TPM: 20})
	b.gw.ClusterPoll()
	for i, h := range []*harness{a, b} {
		if code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	if code, _, body := a.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 429 || !strings.Contains(body, "tokens per minute") {
		t.Fatalf("30 tokens used across instances: %d %s", code, body)
	}
}

func TestClusterKeyConcurrency(t *testing.T) {
	a, b := clusterPair(t)
	a.model("coder", "oa/slow")
	key := a.limitedKey(store.APIKey{MaxConcurrency: 1})
	b.gw.ClusterPoll()
	done := make(chan int)
	go func() {
		code, _, _ := a.postAs(key, "/v1/chat/completions", oaReq("coder", false))
		done <- code
	}()
	time.Sleep(100 * time.Millisecond)
	if code, _, body := b.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 429 || !strings.Contains(body, "concurrent") {
		t.Fatalf("second concurrent request on the other instance: %d %s", code, body)
	}
	if code := <-done; code != 200 {
		t.Fatalf("first request: %d", code)
	}
	if code, _, body := b.postAs(key, "/v1/chat/completions", oaReq("coder", false)); code != 200 {
		t.Fatalf("after the first finished: %d %s", code, body)
	}
}

func TestClusterProviderSlotsOverflow(t *testing.T) {
	a, b := clusterPair(t)
	a.setProvider("oa", func(p *store.Provider) { p.MaxConcurrency = 1 })
	a.model("coder", "oa/slow", "an/ok")
	b.gw.ClusterPoll()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if resp, _ := a.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/slow" {
			t.Errorf("first request should hold the slot: %s", resp.Header.Get("X-Route-Target"))
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if n := b.gw.InFlight()["oa"]; n != 1 {
		t.Errorf("in-flight seen from the other instance: %d", n)
	}
	resp, body := b.post("/v1/chat/completions", oaReq("coder", false))
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatalf("should overflow to the fallback: %d %s %s", resp.StatusCode, resp.Header.Get("X-Route-Target"), body)
	}
	wg.Wait()
	if resp, _ := b.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "oa/slow" {
		t.Fatalf("slot not released: %s", resp.Header.Get("X-Route-Target"))
	}
}

func TestClusterQueueWaitsForOtherInstance(t *testing.T) {
	a, b := clusterPair(t)
	a.setProvider("oa", func(p *store.Provider) { p.MaxConcurrency = 1 })
	st := a.st.GetSettings()
	st.QueueTimeoutSeconds = 5
	mustNil(t, a.st.UpdateSettings(st))
	a.model("gpu", "oa/slow")
	b.gw.ClusterPoll()
	go a.post("/v1/chat/completions", oaReq("gpu", false))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	resp, body := b.post("/v1/chat/completions", oaReq("gpu", false))
	if resp.StatusCode != 200 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("queued request: %d after %s: %s", resp.StatusCode, time.Since(start), body)
	}
}

func TestClusterBreakerShared(t *testing.T) {
	a, b := clusterPair(t)
	a.model("coder", "oa/fail402", "an/ok")
	b.gw.ClusterPoll()
	if resp, _ := a.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "an/ok" {
		t.Fatal("a should fail over")
	}
	eventually(t, "b learns the cooldown", func() bool {
		a.gw.ClusterPoll()
		b.gw.ClusterPoll()
		return time.Until(b.gw.Breaker.OpenUntil("oa", "oa/fail402")) > time.Second
	})
	hits := a.mock.Hits("fail402")
	if resp, _ := b.post("/v1/chat/completions", oaReq("coder", false)); resp.Header.Get("X-Route-Target") != "an/ok" || a.mock.Hits("fail402") != hits {
		t.Fatal("b should skip the cooling provider")
	}
	// a reset on one instance clears the others
	b.gw.Breaker.Reset("")
	eventually(t, "the reset reaches a", func() bool {
		b.gw.ClusterPoll()
		a.gw.ClusterPoll()
		return a.gw.Breaker.OpenUntil("oa", "oa/fail402").IsZero()
	})
}

func TestClusterAlertSentOnce(t *testing.T) {
	a, b := clusterPair(t)
	sink := a.alertSink(nil)
	a.model("broken", "oa/fail400")
	b.gw.ClusterPoll()
	a.post("/v1/chat/completions", oaReq("broken", false))
	b.post("/v1/chat/completions", oaReq("broken", false))
	b.gw.Alerts.Wait()
	if got := sink.get(a); len(got) != 1 {
		t.Fatalf("the same alert from two instances: %d sent", len(got))
	}
}
