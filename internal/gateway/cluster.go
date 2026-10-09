package gateway

import (
	"context"
	"log"
	"sync"

	"ai-route/internal/store"
)

// clusterGlue connects the in-memory breaker, limiter and alerts to other
// instances sharing the database (PostgreSQL only).
type clusterGlue struct {
	c       *store.Cluster
	pending chan store.Cooldown // breaker changes, published in order

	dropMu  sync.Mutex
	dropped int
}

func (g *Gateway) setupCluster(c *store.Cluster) {
	if c == nil {
		return
	}
	cg := &clusterGlue{c: c, pending: make(chan store.Cooldown, 1024)}
	g.cluster = cg
	g.Breaker.publish = cg.queue
	// one instance sends each alert; the others see its lease
	g.Alerts.Share(c.Claim, c.ReleaseLease)
}

// queue hands a breaker change to the publisher without blocking the
// request path (the breaker lock is held).
func (cg *clusterGlue) queue(cd store.Cooldown) {
	select {
	case cg.pending <- cd:
	default:
		cg.dropMu.Lock()
		cg.dropped++
		n := cg.dropped
		cg.dropMu.Unlock()
		if n == 1 || n%1000 == 0 {
			log.Printf("cluster: breaker change queue full, %d changes not shared", n)
		}
	}
}

// RunCluster shares state with the other instances until ctx is done (it
// returns at once with a single instance).
func (g *Gateway) RunCluster(ctx context.Context) {
	cg := g.cluster
	if cg == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case cd := <-cg.pending:
				if err := cg.c.PublishCooldown(cd); err != nil {
					log.Printf("cluster: share breaker change %s: %v", cd.Key, err)
				}
			}
		}
	}()
	cg.c.Run(ctx, store.Handlers{Breaker: g.Breaker.Apply})
	wg.Wait()
}

// ClusterPoll applies other instances' changes now (tests).
func (g *Gateway) ClusterPoll() {
	if g.cluster == nil {
		return
	}
	// publish what is queued first, so a poll right after sees it
	for {
		select {
		case cd := <-g.cluster.pending:
			if err := g.cluster.c.PublishCooldown(cd); err != nil {
				log.Printf("cluster: share breaker change %s: %v", cd.Key, err)
			}
			continue
		default:
		}
		break
	}
	g.cluster.c.Poll(store.Handlers{Breaker: g.Breaker.Apply})
}
