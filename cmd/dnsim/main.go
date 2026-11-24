// Command dnsim simulates cluster DNS peak load for a Kubernetes-like cluster
// and compares no cache, node-local cache, and node cache plus resolver-config
// changes.
//
// Usage:
//
//	dnsim run -out web/report.json -html web/index.html
//	dnsim run -duration 30 -apps 180 -seed 7
//	dnsim explain
//
// The run writes a JSON report and a self-contained HTML dashboard.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"dnsim/internal/report"
	"dnsim/internal/sim"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "dnsim:", err)
			os.Exit(1)
		}
	case "explain":
		explain()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dnsim - cluster DNS peak-load simulator

commands:
  run      run the three experiments and write report.json and the dashboard
  explain  print the modeled resolver rules and the scenario description

run flags:
`)
	flag.CommandLine.SetOutput(os.Stderr)
	fs := runFlags(flag.NewFlagSet("run", flag.ContinueOnError))
	_ = fs
	fs.PrintDefaults()
}

func runFlags(fs *flag.FlagSet) *flag.FlagSet {
	fs.String("out", "web/report.json", "JSON report path")
	fs.String("html", "web/index.html", "HTML dashboard path (empty to skip)")
	fs.Int("seed", 1, "random seed")
	fs.Float64("duration", 60, "simulated minutes")
	fs.Int("apps", 120, "number of applications")
	fs.Int("namespaces", 60, "number of namespaces")
	fs.Int("services", 40, "services per namespace")
	fs.Int("nodes", 200, "nodes")
	fs.Int("containers", 15, "containers per node")
	fs.Int("coredns-replicas", 4, "CoreDNS replica count")
	fs.Float64("coredns-capacity", 12000, "queries per second per replica")
	fs.Float64("peak-qps", 40, "peak resolutions per second per app")
	fs.Float64("loss", 0.0015, "peak packet-loss rate")
	fs.Float64("cache-ttl", 30, "positive cache TTL seconds")
	fs.Float64("neg-ttl", 5, "negative cache TTL seconds")
	fs.Int("ndots", 5, "default ndots in pod resolv.conf")
	fs.String("client", "glibc", "client resolver policy: glibc or go")
	return fs
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	runFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	get := func(name string) string {
		f := fs.Lookup(name)
		return f.Value.String()
	}
	cfg := sim.DefaultConfig()
	cfg.Seed = uint64(atoi(get("seed"), 1))
	cfg.DurationMinutes = atof(get("duration"), 60)
	cfg.Apps = atoi(get("apps"), 120)
	cfg.Namespaces = atoi(get("namespaces"), 60)
	cfg.ServicesPerNS = atoi(get("services"), 40)
	cfg.Nodes = atoi(get("nodes"), 200)
	cfg.ContainersPerNode = atoi(get("containers"), 15)
	cfg.CoreDNSReplicas = atoi(get("coredns-replicas"), 4)
	cfg.CoreDNSCapacity = atof(get("coredns-capacity"), 12000)
	cfg.PeakQPSPerApp = atof(get("peak-qps"), 40)
	cfg.PeakLossRate = atof(get("loss"), 0.0015)
	cfg.CacheTTLSeconds = atof(get("cache-ttl"), 30)
	cfg.NegativeTTLSeconds = atof(get("neg-ttl"), 5)
	cfg.DefaultNdots = atoi(get("ndots"), 5)
	cfg.ClientPolicy = get("client")

	start := time.Now()
	profiles := sim.Profiles(cfg)
	results := make([]*sim.Result, 0, len(profiles))
	for _, p := range profiles {
		t0 := time.Now()
		res, err := sim.RunProfile(cfg, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  %-24s simulated %v in %v: %d lookups, %d client queries, %d server queries, p99 %.1fms\n",
			p.Name, cfg.Total(), time.Since(t0).Round(time.Millisecond),
			res.Metrics.AppLookups, res.Metrics.ClientQueries, res.Metrics.ServerQueries,
			res.Metrics.Latency.Percentile(0.99))
		results = append(results, res)
	}

	rep := report.BuildAll(cfg, results)
	out := get("out")
	if out != "" {
		if err := report.WriteJSONFile(out, rep); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", out)
	}
	htmlPath := get("html")
	if htmlPath != "" {
		if err := report.WriteHTMLFile(htmlPath, rep); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", htmlPath)
	}
	report.PrintSummary(os.Stdout, rep)
	fmt.Fprintf(os.Stderr, "total wall time %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func atoi(s string, def int) int {
	var v int
	if _, err := fmt.Sscan(s, &v); err != nil {
		return def
	}
	return v
}

func atof(s string, def float64) float64 {
	var v float64
	if _, err := fmt.Sscan(s, &v); err != nil {
		return def
	}
	return v
}

func explain() {
	fmt.Print(`Simulated resolver rules (verified against system libresolv):

search-list order
  name with trailing dot        -> queried as-is only
  name with >= ndots dots       -> as-is first, then each search suffix
  name with <  ndots dots       -> each search suffix first, as-is last
  suffixes longer than 254 B    -> skipped; at most 6 search domains

per candidate
  NXDOMAIN / NODATA / SERVFAIL  -> move to the next candidate
  no response within timeout    -> retry per attempts, then give up (h_errno TRY_AGAIN)
  A and AAAA                    -> both queried (unless single-request or IPv4-only)

The leak this models: a pod with ndots:5 and search
  <ns>.svc.cluster.local svc.cluster.local cluster.local corp.internal
turns one external lookup such as api.stripe.com into five candidate names,
each queried for A and AAAA: ten packets, four of which can never resolve.
Under packet loss each lost packet costs a full five-second timeout, and the
burst saturates the limited CoreDNS replicas.

experiments
  A  no cache
  B  node-local cache (positive and negative caching, short upstream retries,
     watch-driven invalidation for in-cluster services)
  C  B plus per-app resolver-config changes (ndots:2, trimmed search list,
     fully-qualified external names)
  D  B with TTL-only expiry (diagnostic: shows the stale-address window)
`)
}
