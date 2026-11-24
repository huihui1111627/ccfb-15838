package sim

import (
	"testing"
	"time"

	"dnsim/internal/engine"
	"dnsim/internal/resolv"
)

// newTestCache builds a cache wired to a bare simulator with virtual time.
func newTestCache(t *testing.T, cfg Config, profile Profile) (*NodeCache, *Sim) {
	t.Helper()
	s := &Sim{cfg: cfg, profile: profile, eng: engine.New()}
	s.rnd = engine.NewRand(cfg.Seed, 0x1111)
	s.netRnd = engine.NewRand(cfg.Seed, 0x2222)
	s.cluster = buildCluster(cfg, engine.NewRand(cfg.Seed, 0x3333))
	s.nodes = s.cluster.Nodes
	s.dns = newCoreDNS(s)
	s.metrics = NewMetrics(true, 100)
	for _, n := range s.nodes {
		n.Cache = NewNodeCache(n, s)
	}
	return s.nodes[0].Cache, s
}

func TestNegativeCachingAndTTL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NegativeTTLSeconds = 5
	cache, s := newTestCache(t, cfg, Profile{CacheEnabled: true, CacheTTLSeconds: 30, NegativeTTLSeconds: 5, UpstreamAttempts: 1, UpstreamRetryTimeoutMs: 300})
	missing := "nosuchsvc.does-not-exist.svc.cluster.local."

	// First query misses and goes upstream, which answers NXDOMAIN.
	var got []resolv.Reply
	cache.Lookup("app", missing, resolv.TypeA, 5*time.Second, func(r resolv.Reply) { got = append(got, r) })
	s.eng.RunUntilIdle()
	if len(got) != 1 || got[0].Status != resolv.StatusNXDomain {
		t.Fatalf("first reply %+v", got)
	}
	upstreamAfterFirst := s.metrics.UpstreamCalls

	// Within the negative TTL the answer is served from the cache.
	cache.Lookup("app", missing, resolv.TypeA, 5*time.Second, func(r resolv.Reply) { got = append(got, r) })
	s.eng.RunUntilIdle()
	if s.metrics.UpstreamCalls != upstreamAfterFirst {
		t.Fatalf("negative entry was not cached")
	}
	if s.metrics.CacheNegHits != 1 {
		t.Fatalf("negative hits %d", s.metrics.CacheNegHits)
	}

	// Past the negative TTL it is a miss again.
	s.eng.RunUntil(6 * time.Second)
	cache.Lookup("app", missing, resolv.TypeA, 5*time.Second, func(resolv.Reply) {})
	s.eng.RunUntilIdle()
	if s.metrics.UpstreamCalls <= upstreamAfterFirst {
		t.Fatalf("expired negative entry still served from cache")
	}
}

func TestPositiveCacheAndCoalescing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheTTLSeconds = 30
	cache, s := newTestCache(t, cfg, Profile{CacheEnabled: true, CacheTTLSeconds: 30, NegativeTTLSeconds: 5, UpstreamAttempts: 1, UpstreamRetryTimeoutMs: 300})
	svc := s.cluster.Services[0]

	// Two concurrent lookups of the same name must coalesce into one upstream
	// exchange.
	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(resolv.Reply) {})
	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(resolv.Reply) {})
	s.eng.RunUntilIdle()
	if s.metrics.UpstreamCalls != 1 {
		t.Fatalf("coalescing failed: %d upstream calls", s.metrics.UpstreamCalls)
	}

	// A later lookup is a hit.
	before := s.metrics.UpstreamCalls
	var reply resolv.Reply
	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(r resolv.Reply) { reply = r })
	if s.metrics.UpstreamCalls != before {
		t.Fatalf("positive entry not cached")
	}
	if reply.Status != resolv.StatusAnswer || len(reply.IPs) != 1 {
		t.Fatalf("cached reply %+v", reply)
	}
}

func TestShortUpstreamRetryBeatsClientTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PeakWindow = [2]float64{0, 1}
	cfg.PeakLossRate = 1.0
	cfg.LossRate = 1.0
	p := Profile{CacheEnabled: true, CacheTTLSeconds: 30, NegativeTTLSeconds: 5,
		UpstreamAttempts: 3, UpstreamRetryTimeoutMs: 200}
	cache, s := newTestCache(t, cfg, p)
	svc := s.cluster.Services[1]

	var when time.Duration
	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(r resolv.Reply) {
		when = s.eng.Now()
	})
	s.eng.RunUntilIdle()
	// Three 200ms attempts finish in 600ms, far below the application's 5s.
	if when > time.Second {
		t.Fatalf("cache took %v to exhaust retries, expected ~600ms", when)
	}
	if s.metrics.UpstreamRetries == 0 {
		t.Fatalf("cache did not retry lost upstream queries")
	}
}

func TestWatchInvalidation(t *testing.T) {
	cfg := DefaultConfig()
	cache, s := newTestCache(t, cfg, Profile{CacheEnabled: true, CacheTTLSeconds: 30, NegativeTTLSeconds: 5, UpstreamAttempts: 1, UpstreamRetryTimeoutMs: 300})
	var svc *Service
	for _, cand := range s.cluster.Services {
		if cand.HoldsEndpoints {
			svc = cand
			break
		}
	}
	oldIP := svc.IPv4

	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(resolv.Reply) {})
	s.eng.RunUntilIdle()
	if cache.Size() == 0 {
		t.Fatal("nothing cached")
	}

	// Simulate an endpoint change and the informer update.
	svc.Version++
	svc.IPv4 = "10.255.0.1"
	cache.OnServiceChange(svc)
	if cache.Size() != 0 {
		t.Fatalf("entry not invalidated on service change")
	}

	var reply resolv.Reply
	cache.Lookup("app", svc.FQDN, resolv.TypeA, 5*time.Second, func(r resolv.Reply) { reply = r })
	s.eng.RunUntilIdle()
	if len(reply.IPs) != 1 || reply.IPs[0] == oldIP || reply.IPs[0] != svc.IPv4 {
		t.Fatalf("cache served stale/missing address: %+v (want %s)", reply, svc.IPv4)
	}
}
