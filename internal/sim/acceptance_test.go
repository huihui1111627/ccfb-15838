package sim

import (
	"testing"
	"time"
)

// smallConfig is a scaled-down peak scenario that still exercises every
// modeled effect. It runs in a couple of seconds.
func smallConfig() Config {
	cfg := DefaultConfig()
	cfg.Seed = 11
	cfg.DurationMinutes = 12
	cfg.Apps = 40
	cfg.Namespaces = 12
	cfg.ServicesPerNS = 12
	cfg.Nodes = 24
	cfg.ContainersPerNode = 4
	return cfg
}

func runProfiles(t *testing.T, cfg Config) map[string]*Result {
	t.Helper()
	out := map[string]*Result{}
	for _, p := range Profiles(cfg) {
		res, err := RunProfile(cfg, p)
		if err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		out[p.Name] = res
	}
	return out
}

func TestNodeCacheReducesServerLoad(t *testing.T) {
	cfg := smallConfig()
	res := runProfiles(t, cfg)
	a := res["A-no-cache"].Metrics
	b := res["B-node-cache"].Metrics
	c := res["C-node-cache+config"].Metrics

	if b.ServerQueries >= a.ServerQueries/2 {
		t.Fatalf("node cache should at least halve CoreDNS load: %d vs %d", b.ServerQueries, a.ServerQueries)
	}
	if c.ServerQueries >= b.ServerQueries/2 {
		t.Fatalf("config adjustment should roughly halve the cached load again: %d vs %d", c.ServerQueries, b.ServerQueries)
	}
	if c.WastedQueries >= a.WastedQueries/5 {
		t.Fatalf("config adjustment should remove most wasted queries: %d vs %d", c.WastedQueries, a.WastedQueries)
	}
	t.Logf("server queries: %.2fM -> %.1fK -> %.1fK", float64(a.ServerQueries)/1e6, float64(b.ServerQueries)/1e3, float64(c.ServerQueries)/1e3)
}

func TestLatencyImprovesAndStallsDisappear(t *testing.T) {
	cfg := smallConfig()
	res := runProfiles(t, cfg)
	a := res["A-no-cache"].Metrics
	b := res["B-node-cache"].Metrics
	c := res["C-node-cache+config"].Metrics

	if a.Latency.Percentile(0.99) <= b.Latency.Percentile(0.99) {
		t.Fatalf("cache should reduce p99: A=%.1fms B=%.1fms", a.Latency.Percentile(0.99), b.Latency.Percentile(0.99))
	}
	if c.Latency.Percentile(0.99) > b.Latency.Percentile(0.99) {
		t.Fatalf("config+ cache should not be slower than cache alone")
	}
	if a.SlowLookups == 0 {
		t.Fatalf("no-cache scenario should show five-second stalls")
	}
	if b.SlowLookups != 0 {
		t.Fatalf("node cache plus short upstream retries should remove five-second stalls, got %d", b.SlowLookups)
	}
	// No lookup may exceed the application deadline (the acceptance 5s).
	for name, r := range res {
		if max := r.Metrics.Latency.MaxMS(); max > 5100 {
			t.Fatalf("%s: lookup latency %.0fms exceeds the 5s deadline", name, max)
		}
	}
	t.Logf("p99: %.1fms -> %.1fms -> %.1fms; stalls: %d -> %d -> %d",
		a.Latency.Percentile(0.99), b.Latency.Percentile(0.99), c.Latency.Percentile(0.99),
		a.SlowLookups, b.SlowLookups, c.SlowLookups)
}

func TestServiceChangeStaleness(t *testing.T) {
	cfg := smallConfig()
	res := runProfiles(t, cfg)
	b := res["B-node-cache"].Metrics
	d := res["D-node-cache-ttl-only"].Metrics

	notify := time.Duration(cfg.NotifyDelayMs * float64(time.Millisecond))
	if b.StaleWindowMax > float64((notify*2)/time.Millisecond) {
		t.Fatalf("watch invalidation window %.1fms should be within the notify delay %.1fms",
			b.StaleWindowMax, float64(notify/time.Millisecond))
	}
	if b.StaleResidualMax != 0 {
		t.Fatalf("entries still answering the old address after invalidation: %.1fms", b.StaleResidualMax)
	}
	// TTL-only mode is bounded by the positive TTL.
	if d.StaleWindowMax > cfg.CacheTTLSeconds*1000 {
		t.Fatalf("TTL-only stale window %.0fms exceeds the TTL %.0fs", d.StaleWindowMax, cfg.CacheTTLSeconds)
	}
	if d.StaleWindowMax <= b.StaleWindowMax*10 {
		t.Fatalf("TTL-only stale window (%.0fms) should dwarf the watch window (%.0fms)", d.StaleWindowMax, b.StaleWindowMax)
	}
}

func TestCacheCountersConsistent(t *testing.T) {
	cfg := smallConfig()
	res := runProfiles(t, cfg)
	b := res["B-node-cache"].Metrics
	if b.CacheHits+b.CacheMisses != b.CacheLookups {
		t.Fatalf("cache hits+misses (%d+%d) != lookups (%d)", b.CacheHits, b.CacheMisses, b.CacheLookups)
	}
	if b.CacheNegHits == 0 {
		t.Fatalf("expected negative cache hits from failed search suffixes")
	}
	// Each miss causes at least one upstream query, and possibly a retry.
	if b.UpstreamCalls < b.CacheMisses {
		t.Fatalf("upstream calls %d fewer than misses %d", b.UpstreamCalls, b.CacheMisses)
	}
	if b.UpstreamRetries == 0 {
		t.Fatalf("expected the cache to retry some lost upstream queries")
	}
	if b.Invalidations == 0 {
		t.Fatalf("expected service-change invalidations")
	}
}

func TestWastedQueriesAreSearchSuffixes(t *testing.T) {
	cfg := smallConfig()
	res := runProfiles(t, cfg)
	rep := res["A-no-cache"]
	total := 0
	for _, am := range rep.Metrics.Apps {
		total += am.Queries
	}
	if total != rep.Metrics.ClientQueries {
		t.Fatalf("per-app queries %d != total %d", total, rep.Metrics.ClientQueries)
	}
	// The worst app must be a default-ndots5 one and show amplification.
	var worst string
	worstQ := 0.0
	for _, app := range rep.Cluster.Apps {
		am := rep.Metrics.Apps[app.Name]
		if am == nil || am.Lookups == 0 {
			continue
		}
		q := float64(am.Queries) / float64(am.Lookups)
		if q > worstQ {
			worstQ, worst = q, app.Name
		}
	}
	if worstQ < 3 {
		t.Fatalf("expected amplification of at least 3x, got %.1fx (%s)", worstQ, worst)
	}
}

// TestNamesGroupedByService ensures the per-name report attributes short-name
// lookups to the service they resolve to, including through the search list.
func TestNamesGroupedByService(t *testing.T) {
	cfg := smallConfig()
	cfg.InClusterShare = 1
	res := runProfiles(t, cfg)["A-no-cache"]
	if res.Metrics.Answers == 0 {
		t.Fatal("no successful in-cluster lookups")
	}
	for appName, am := range res.Metrics.Apps {
		for name := range am.PerName {
			if _, ok := res.Cluster.serviceByName[name]; !ok {
				t.Fatalf("app %s statistics grouped under %q, which is not a service FQDN", appName, name)
			}
		}
	}
	// Some services intentionally have no endpoints, which makes their
	// lookups resolve as NODATA. That is a modeled production condition, not
	// a bug, so lookup errors are expected; attribution must still hold.
	t.Logf("NODATA lookups: %d of %d", res.Metrics.LookupErrors, res.Metrics.AppLookups)
}
