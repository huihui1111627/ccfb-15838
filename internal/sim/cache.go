package sim

import (
	"fmt"
	"time"

	"dnsim/internal/engine"
	"dnsim/internal/resolv"
)

// Node is a cluster node. Each node runs the node-local DNS cache (the
// "node cache" experiment) and hosts containers.
type Node struct {
	Index      int
	Name       string
	Containers int
	IP         string
	// Cache is nil when the node cache is disabled.
	Cache *NodeCache
}

// CacheAddr is the node-local listener address.
func (n *Node) CacheAddr() string { return n.IP + ":5353" }

// CacheEntry is one cache record.
type CacheEntry struct {
	Name   string
	Type   uint16
	IPs    []string
	RCode  int // 0 for a positive record, 3 for NXDOMAIN
	NoData bool
	TTL    time.Duration
	Expiry time.Duration
	// ServiceVersion is the service version observed when the entry was
	// inserted, used for endpoint-change invalidation. IsService is false for
	// external names, which expire only by TTL.
	ServiceVersion uint64
	IsService      bool
	Inserted       time.Duration
	Hits           int
}

// NodeCache is the node-local cache proxy.
type NodeCache struct {
	node   *Node
	sim    *Sim
	engine *engine.Engine

	entries map[cacheKey]*CacheEntry
	flights map[cacheKey]*flight

	// Settings applied from the experiment profile.
	PositiveTTL      time.Duration
	NegativeTTL      time.Duration
	UpstreamRetry    time.Duration
	UpstreamAttempts int

	// Counters.
	lookups          int
	hits             int
	negativeHits     int
	missesCount      int
	upstreamQueries  int
	upstreamRetries  int
	upstreamTimeouts int
	upstreamFailures int
	invalidations    int
	staleServes      int
	inserts          int
}

type cacheKey struct {
	name  string
	qtype uint16
}

type flight struct {
	waiters []func(resolv.Reply)
	start   time.Duration
}

// NewNodeCache creates a cache for node.
func NewNodeCache(node *Node, s *Sim) *NodeCache {
	c := &NodeCache{
		node:    node,
		sim:     s,
		engine:  s.eng,
		entries: make(map[cacheKey]*CacheEntry),
		flights: make(map[cacheKey]*flight),
	}
	c.configure(s.profile)
	return c
}

// configure applies an experiment profile's cache settings.
func (c *NodeCache) configure(p Profile) {
	c.PositiveTTL = timeSeconds(p.CacheTTLSeconds)
	c.NegativeTTL = timeSeconds(p.NegativeTTLSeconds)
	c.UpstreamRetry = time.Duration(p.UpstreamRetryTimeoutMs * float64(time.Millisecond))
	if c.UpstreamRetry <= 0 {
		c.UpstreamRetry = 400 * time.Millisecond
	}
	c.UpstreamAttempts = p.UpstreamAttempts
	if c.UpstreamAttempts < 1 {
		c.UpstreamAttempts = 1
	}
}

// Lookup answers a query from the cache or forwards it upstream. done runs
// when the answer is ready. timeout is the application's per-query timeout.
// The cache's own upstream deadline is much shorter, so a lost packet is
// retried before the application would ever see a stall.
func (c *NodeCache) Lookup(app, name string, qtype uint16, timeout time.Duration, done func(resolv.Reply)) {
	now := c.engine.Now()
	c.lookups++
	c.sim.metrics.CacheLookups++
	key := cacheKey{name, qtype}
	if e := c.entries[key]; e != nil {
		if e.Expiry > now {
			c.hits++
			if e.RCode != 0 || e.NoData {
				c.negativeHits++
				c.sim.metrics.CacheNegHits++
			}
			c.sim.metrics.bucket(now).CacheHits++
			e.Hits++
			c.sim.metrics.CacheHits++
			if am := c.sim.metrics.app(app); am != nil {
				am.CacheHits++
			}
			done(cacheReply(e))
			return
		}
		delete(c.entries, key)
	}
	c.missesCount++
	c.sim.metrics.CacheMisses++
	// hits+misses must equal lookups for every app, so the hit ratio is
	// meaningful; a coalesced request that joins an in-flight exchange is a
	// miss for the app but not a new upstream query.
	if am := c.sim.metrics.app(app); am != nil {
		am.CacheMisses++
	}
	// Coalesce concurrent lookups of the same name/type.
	if f, ok := c.flights[key]; ok {
		f.waiters = append(f.waiters, done)
		return
	}
	f := &flight{start: now}
	f.waiters = append(f.waiters, done)
	c.flights[key] = f
	c.exchange(name, qtype, key, f, 0)
}

// exchange sends the upstream query, retrying lost packets with the cache's
// own short deadline.
func (c *NodeCache) exchange(name string, qtype uint16, key cacheKey, f *flight, attempt int) {
	if attempt > 0 {
		c.upstreamRetries++
		c.sim.metrics.UpstreamRetries++
	}
	c.upstreamQueries++
	c.sim.metrics.UpstreamCalls++
	c.sim.metrics.bucket(c.engine.Now()).Upstream++
	c.sim.sendUpstream(name, qtype, c.UpstreamRetry, func(reply CoreDNSReply, st resolv.Status) {
		switch st {
		case resolv.StatusTimeout, resolv.StatusServFail:
			c.upstreamTimeouts++
			c.sim.metrics.UpstreamTimeouts++
			if attempt+1 < c.UpstreamAttempts {
				c.exchange(name, qtype, key, f, attempt+1)
				return
			}
			c.upstreamFailures++
			c.sim.metrics.UpstreamFailures++
			delete(c.flights, key)
			for _, cb := range f.waiters {
				cb(resolv.Reply{Status: st})
			}
		default:
			c.insert(name, qtype, reply, st)
			delete(c.flights, key)
			r := cacheReplyFor(reply, st)
			for _, cb := range f.waiters {
				cb(r)
			}
		}
	})
}

// insert stores a response, honoring positive and negative TTLs.
func (c *NodeCache) insert(name string, qtype uint16, reply CoreDNSReply, st resolv.Status) {
	now := c.engine.Now()
	key := cacheKey{name, qtype}
	e := &CacheEntry{Name: name, Type: qtype, Inserted: now}
	switch st {
	case resolv.StatusAnswer:
		if len(reply.IPs) == 0 {
			return
		}
		e.IPs = append([]string(nil), reply.IPs...)
		e.TTL = c.PositiveTTL
		// Honor the record TTL: the cache may shorten it, never extend it.
		// External names live exactly as long as their record says; cluster
		// services also expire on endpoint changes via the watch path.
		if reply.TTL > 0 {
			if d := time.Duration(reply.TTL) * time.Second; d < e.TTL || e.TTL <= 0 {
				e.TTL = d
			}
		}
		if svc := c.sim.cluster.serviceByName[name]; svc != nil {
			e.IsService = true
			e.ServiceVersion = svc.Version
		}
	case resolv.StatusNXDomain:
		e.RCode = 3
		e.TTL = c.NegativeTTL
	case resolv.StatusNoData:
		e.NoData = true
		e.TTL = c.NegativeTTL
	default:
		return
	}
	if e.TTL <= 0 {
		return
	}
	e.Expiry = now + e.TTL
	c.entries[key] = e
	c.inserts++
}

// OnServiceChange invalidates cache entries for a service whose endpoints
// changed. The entry version is compared with the service version so an entry
// refreshed after the change is not dropped.
func (c *NodeCache) OnServiceChange(svc *Service) {
	for _, qt := range []uint16{resolv.TypeA, resolv.TypeAAAA} {
		key := cacheKey{svc.FQDN, qt}
		e := c.entries[key]
		if e == nil || !e.IsService {
			continue
		}
		if e.ServiceVersion < svc.Version {
			// The entry still maps the old endpoint set.
			c.staleServes++
			c.sim.metrics.StaleServes++
			delete(c.entries, key)
			c.invalidations++
			c.sim.metrics.Invalidations++
		}
	}
}

// staleRemaining returns, in milliseconds, how long the cache can still serve
// an entry that maps the pre-change address of svc.
func (c *NodeCache) staleRemaining(svc *Service, now time.Duration) float64 {
	key := cacheKey{svc.FQDN, resolv.TypeA}
	e := c.entries[key]
	if e == nil || !e.IsService || e.ServiceVersion >= svc.Version {
		return 0
	}
	if e.Expiry <= now {
		return 0
	}
	return float64(e.Expiry-now) / float64(time.Millisecond)
}

func cacheReply(e *CacheEntry) resolv.Reply {
	switch {
	case e.RCode == 3:
		return resolv.Reply{Status: resolv.StatusNXDomain}
	case e.NoData:
		return resolv.Reply{Status: resolv.StatusNoData}
	}
	return resolv.Reply{Status: resolv.StatusAnswer, IPs: append([]string(nil), e.IPs...)}
}

func cacheReplyFor(reply CoreDNSReply, st resolv.Status) resolv.Reply {
	switch st {
	case resolv.StatusAnswer:
		return resolv.Reply{Status: st, IPs: reply.IPs}
	}
	return resolv.Reply{Status: st}
}

// Size returns the number of live entries.
func (c *NodeCache) Size() int { return len(c.entries) }

func (c *NodeCache) String() string {
	return fmt.Sprintf("cache on %s: %d entries", c.node.Name, len(c.entries))
}
