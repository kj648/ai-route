package store

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two instances on one PostgreSQL database (skipped with SQLite).
func clusterStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	loc := testLoc(t)
	if !IsPostgresURL(loc) {
		t.Skip("multi-instance tests need PostgreSQL (AI_ROUTE_TEST_DATABASE_URL)")
	}
	a, err := openLoc(loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := openLoc(loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	a.Cluster().Heartbeat()
	b.Cluster().Heartbeat()
	return a, b
}

// The check-and-count is atomic: concurrent requests on two instances never
// exceed the limit.
func TestClusterLimitsAreExact(t *testing.T) {
	a, b := clusterStores(t)
	var taken, slots atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		c := []*Cluster{a.Cluster(), b.Cluster()}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _, err := c.TakeWindow("rpm:1", 7, time.Now()); err != nil {
				t.Error(err)
			} else if ok {
				taken.Add(1)
			}
			if ok, err := c.Acquire("key:1", 5); err != nil {
				t.Error(err)
			} else if ok {
				slots.Add(1)
			}
		}()
	}
	wg.Wait()
	if taken.Load() != 7 || slots.Load() != 5 {
		t.Fatalf("admitted %d of rpm 7, %d of 5 slots", taken.Load(), slots.Load())
	}
	totals, _ := a.Cluster().SlotTotals("key:")
	if totals["1"] != 5 {
		t.Fatalf("slot totals: %v", totals)
	}
}

func TestClusterDeadInstanceSlotsExpire(t *testing.T) {
	a, b := clusterStores(t)
	if ok, _ := a.Cluster().Acquire("prov:gpu", 1); !ok {
		t.Fatal("first slot")
	}
	if ok, _ := b.Cluster().Acquire("prov:gpu", 1); ok {
		t.Fatal("slot taken twice")
	}
	// a stops sending heartbeats (crash): after InstanceTimeout its slot no longer counts
	b.Cluster().now = func() time.Time { return time.Now().Add(InstanceTimeout + time.Second) }
	if ok, _ := b.Cluster().Acquire("prov:gpu", 1); !ok {
		t.Fatal("dead instance still holds the slot")
	}
	list, _ := b.Cluster().Instances()
	alive := 0
	for _, in := range list {
		if in.Alive {
			alive++
		}
	}
	if len(list) != 2 || alive != 0 { // b's clock is ahead of both heartbeats
		t.Fatalf("instances: %+v", list)
	}
}

func TestClusterLeasesAndConfigVersion(t *testing.T) {
	a, b := clusterStores(t)
	if !a.Cluster().Claim("job", time.Minute) || b.Cluster().Claim("job", time.Minute) {
		t.Fatal("lease must go to exactly one instance")
	}
	a.Cluster().ReleaseLease("job")
	if !b.Cluster().Claim("job", time.Minute) {
		t.Fatal("released lease")
	}
	// a write on a reaches b on its next poll
	mustNil(t, a.CreateModel(&Model{Name: "m", Targets: []string{"x/y"}, Enabled: true}))
	if b.Snapshot().ResolveModel("m") != nil {
		t.Fatal("b saw the model before polling")
	}
	b.Cluster().Poll(Handlers{})
	if b.Snapshot().ResolveModel("m") == nil {
		t.Fatal("b did not reload")
	}
	// a generated secret: the first writer wins, everybody reads the same
	x, _ := a.InitKV("admin_token", "from-a")
	y, _ := b.InitKV("admin_token", "from-b")
	if x != "from-a" || y != "from-a" {
		t.Fatalf("tokens %q %q", x, y)
	}
}
