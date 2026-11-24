package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"dnsim/internal/sim"
)

// ProfileResult is one experiment's metrics plus derived views.
type ProfileResult struct {
	Profile sim.Profile  `json:"profile"`
	Metrics *sim.Metrics `json:"metrics"`
	// Apps is the ranked per-application diagnosis.
	Apps []AppReport `json:"apps"`
	// Names is the worst-affected logical names cluster-wide.
	Names []NameReport `json:"names"`
}

// AppReport is the per-application diagnosis row.
type AppReport struct {
	Name           string  `json:"name"`
	Style          string  `json:"style"`
	Ndots          int     `json:"ndots"`
	SearchDomains  int     `json:"searchDomains"`
	Families       string  `json:"families"`
	Serial         bool    `json:"serialFamilies"`
	Lookups        int     `json:"lookups"`
	Queries        int     `json:"queries"`
	AvgQueries     float64 `json:"avgQueriesPerLookup"`
	Wasted         int     `json:"wastedQueries"`
	WastedRatio    float64 `json:"wastedRatio"`
	TimeoutLookups int     `json:"timeoutLookups"`
	SlowLookups    int     `json:"slowLookups"`
	CacheHits      int     `json:"cacheHits"`
	CacheMisses    int     `json:"cacheMisses"`
	CacheHitRatio  float64 `json:"cacheHitRatio"`
	LookupP50MS    float64 `json:"lookupP50Ms"`
	LookupP99MS    float64 `json:"lookupP99Ms"`
	QueryP50MS     float64 `json:"queryP50Ms"`
	QueryP95MS     float64 `json:"queryP95Ms"`
	QueryP99MS     float64 `json:"queryP99Ms"`
	MaxMS          float64 `json:"maxMs"`
}

// NameReport is the worst-name diagnosis row.
type NameReport struct {
	Name      string  `json:"name"`
	App       string  `json:"app"`
	Lookups   int     `json:"lookups"`
	Queries   int     `json:"queries"`
	Wasted    int     `json:"wasted"`
	Amplifier float64 `json:"avgQueriesPerLookup"`
	Timeouts  int     `json:"timeouts"`
}

// Summary is the acceptance-criteria comparison table.
type Summary struct {
	Name             string  `json:"name"`
	CacheEnabled     bool    `json:"cacheEnabled"`
	ConfigAdjusted   bool    `json:"configAdjusted"`
	ClientQueries    int     `json:"clientQueries"`
	ServerQueries    int     `json:"serverQueries"`
	WastedQueries    int     `json:"wastedQueries"`
	WastedRatio      float64 `json:"wastedRatio"`
	LatencyP50MS     float64 `json:"latencyP50Ms"`
	LatencyP95MS     float64 `json:"latencyP95Ms"`
	LatencyP99MS     float64 `json:"latencyP99Ms"`
	LatencyMaxMS     float64 `json:"latencyMaxMs"`
	FiveSecondSlow   int     `json:"fiveSecondStalls"`
	TimeoutLookups   int     `json:"timeoutLookups"`
	QueryTimeouts    int     `json:"queryTimeouts"`
	CacheHitRatio    float64 `json:"cacheHitRatio"`
	CacheNegHits     int     `json:"negativeCacheHits"`
	StaleInvalidated int     `json:"staleEntriesInvalidated"`
	UpstreamQueries  int     `json:"upstreamQueries"`
	StaleMaxMS       float64 `json:"staleWindowMaxMs"`
	StaleResidualMS  float64 `json:"staleResidualMaxMs"`
	StaleAvgMS       float64 `json:"staleWindowAvgMs"`
	ServerDrops      int     `json:"serverDrops"`
	ServerQueueP99MS float64 `json:"serverQueueP99Ms"`
	NetworkLost      int     `json:"networkLost"`
}

// Build derives report views from a run.
func Build(res *sim.Result) ProfileResult {
	out := ProfileResult{
		Profile: res.Profile,
		Metrics: res.Metrics,
	}
	for _, app := range res.Cluster.Apps {
		am := res.Metrics.Apps[app.Name]
		if am == nil {
			continue
		}
		row := AppReport{
			Name:           app.Name,
			Style:          app.Style,
			Ndots:          app.Config.Ndots,
			SearchDomains:  len(app.Config.Search),
			Families:       app.Config.Families.String(),
			Serial:         app.Config.SerialFamilies,
			Lookups:        am.Lookups,
			Queries:        am.Queries,
			Wasted:         am.Wasted,
			TimeoutLookups: am.Timeouts,
			SlowLookups:    am.Slow,
			CacheHits:      am.CacheHits,
			CacheMisses:    am.CacheMisses,
			LookupP50MS:    am.Latency.Percentile(0.5),
			LookupP99MS:    am.Latency.Percentile(0.99),
			QueryP50MS:     am.QueryLatency.Percentile(0.5),
			QueryP95MS:     am.QueryLatency.Percentile(0.95),
			QueryP99MS:     am.QueryLatency.Percentile(0.99),
			MaxMS:          am.Latency.MaxMS(),
		}
		if am.Lookups > 0 {
			row.AvgQueries = round2(float64(am.Queries) / float64(am.Lookups))
		}
		if am.Queries > 0 {
			row.WastedRatio = round3(float64(am.Wasted) / float64(am.Queries))
		}
		if am.CacheHits+am.CacheMisses > 0 {
			row.CacheHitRatio = round3(float64(am.CacheHits) / float64(am.CacheHits+am.CacheMisses))
		}
		out.Apps = append(out.Apps, row)
	}
	sort.Slice(out.Apps, func(i, j int) bool {
		if out.Apps[i].Wasted != out.Apps[j].Wasted {
			return out.Apps[i].Wasted > out.Apps[j].Wasted
		}
		return out.Apps[i].AvgQueries > out.Apps[j].AvgQueries
	})
	type nameKey struct{ app, name string }
	agg := map[nameKey]*NameReport{}
	for appName, am := range res.Metrics.Apps {
		for name, nm := range am.PerName {
			k := nameKey{appName, name}
			agg[k] = &NameReport{
				Name: name, App: appName, Lookups: nm.Lookups, Queries: nm.Queries,
				Wasted: nm.Wasted, Timeouts: nm.Timeouts,
			}
		}
	}
	for _, r := range agg {
		if r.Lookups > 0 {
			r.Amplifier = round2(float64(r.Queries) / float64(r.Lookups))
		}
		out.Names = append(out.Names, *r)
	}
	sort.Slice(out.Names, func(i, j int) bool {
		if out.Names[i].Wasted != out.Names[j].Wasted {
			return out.Names[i].Wasted > out.Names[j].Wasted
		}
		return out.Names[i].Queries > out.Names[j].Queries
	})
	if len(out.Names) > 100 {
		out.Names = out.Names[:100]
	}
	return out
}

// Summarize builds the comparison row for one run.
func Summarize(res *sim.Result) Summary {
	m := res.Metrics
	s := Summary{
		Name:             res.Profile.Name,
		CacheEnabled:     res.Profile.CacheEnabled,
		ConfigAdjusted:   res.Profile.ConfigAdjusted,
		ClientQueries:    m.ClientQueries,
		ServerQueries:    m.ServerQueries,
		WastedQueries:    m.WastedQueries,
		LatencyP50MS:     round2(m.Latency.Percentile(0.5)),
		LatencyP95MS:     round2(m.Latency.Percentile(0.95)),
		LatencyP99MS:     round2(m.Latency.Percentile(0.99)),
		LatencyMaxMS:     round2(m.Latency.MaxMS()),
		FiveSecondSlow:   m.SlowLookups,
		TimeoutLookups:   m.LookupTimeouts,
		QueryTimeouts:    m.QueryTimeouts,
		CacheNegHits:     m.CacheNegHits,
		StaleInvalidated: m.StaleServes,
		UpstreamQueries:  m.UpstreamCalls,
		ServerDrops:      m.ServerDrops,
		ServerQueueP99MS: round2(m.ServerQueueWait.Percentile(0.99)),
		NetworkLost:      m.NetworkLost,
		StaleMaxMS:       round2(m.StaleWindowMax),
		StaleResidualMS:  round2(m.StaleResidualMax),
	}
	if m.ClientQueries > 0 {
		s.WastedRatio = round3(float64(m.WastedQueries) / float64(m.ClientQueries))
	}
	if m.CacheLookups > 0 {
		s.CacheHitRatio = round3(float64(m.CacheHits) / float64(m.CacheLookups))
	}
	if m.StaleWindowCount > 0 {
		s.StaleAvgMS = round2(m.StaleWindowSum / float64(m.StaleWindowCount))
	}
	return s
}

// Report is the complete JSON document written by the simulator.
type Report struct {
	Config      sim.Config       `json:"config"`
	Cluster     ClusterSummary   `json:"cluster"`
	Profiles    []ProfileResult  `json:"profiles"`
	Comparison  []Summary        `json:"comparison"`
	Suggestions []sim.Suggestion `json:"suggestions"`
}

// ClusterSummary is a small description of the generated cluster.
type ClusterSummary struct {
	Services   int `json:"services"`
	Externals  int `json:"externals"`
	Apps       int `json:"apps"`
	Nodes      int `json:"nodes"`
	Containers int `json:"containers"`
}

// WriteJSON encodes the report.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }

// FormatNumber renders large counts compactly for the text summary.
func FormatNumber(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// BuildAll turns a set of runs into the final report.
func BuildAll(cfg sim.Config, results []*sim.Result) *Report {
	rep := &Report{Config: cfg}
	if len(results) > 0 {
		c := results[0].Cluster
		rep.Cluster = ClusterSummary{
			Services: len(c.Services), Externals: len(c.Externals),
			Apps: len(c.Apps), Nodes: len(c.Nodes),
			Containers: len(c.Nodes) * cfg.ContainersPerNode,
		}
	}
	var adjusted *sim.Metrics
	for _, r := range results {
		if r.Profile.ConfigAdjusted {
			adjusted = r.Metrics
		}
	}
	for _, r := range results {
		rep.Profiles = append(rep.Profiles, Build(r))
		rep.Comparison = append(rep.Comparison, Summarize(r))
	}
	if len(results) > 0 {
		rep.Suggestions = sim.Suggest(results[0].Cluster, results[0].Metrics, adjusted)
		if len(rep.Suggestions) > 30 {
			rep.Suggestions = rep.Suggestions[:30]
		}
	}
	return rep
}

// WriteJSONFile writes the report as indented JSON.
func WriteJSONFile(path string, rep *Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return rep.WriteJSON(f)
}

// PrintSummary prints the acceptance comparison as a text table.
func PrintSummary(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "\nPeak-load DNS comparison (%s)\n", rep.Config.Summary())
	fmt.Fprintf(w, "%-22s %14s %14s %8s %9s %9s %9s %10s %8s %11s\n",
		"profile", "clientQueries", "serverQueries", "wasted", "p50", "p95", "p99", ">5sStalls", "hitRate", "staleWindow")
	for _, s := range rep.Comparison {
		stale := "-"
		if s.CacheEnabled {
			stale = fmt.Sprintf("%.0fms", s.StaleAvgMS)
		}
		fmt.Fprintf(w, "%-22s %14s %14s %7.1f%% %8.2fms %8.1fms %8.1fms %10s %7.1f%% %11s\n",
			s.Name, FormatNumber(s.ClientQueries), FormatNumber(s.ServerQueries),
			s.WastedRatio*100, s.LatencyP50MS, s.LatencyP95MS, s.LatencyP99MS,
			FormatNumber(s.FiveSecondSlow), s.CacheHitRatio*100, stale)
	}
	if len(rep.Comparison) >= 3 {
		a, b, c := rep.Comparison[0], rep.Comparison[1], rep.Comparison[2]
		fmt.Fprintf(w, "\nServer query reduction: cache %.1f%%, cache+config %.1f%%\n",
			100*(1-float64(b.ServerQueries)/float64(a.ServerQueries)),
			100*(1-float64(c.ServerQueries)/float64(a.ServerQueries)))
		fmt.Fprintf(w, "p99 latency: %.1fms -> %.1fms -> %.1fms; >5s stalls: %d -> %d -> %d\n",
			a.LatencyP99MS, b.LatencyP99MS, c.LatencyP99MS,
			a.FiveSecondSlow, b.FiveSecondSlow, c.FiveSecondSlow)
		if len(rep.Comparison) >= 4 {
			d := rep.Comparison[3]
			fmt.Fprintf(w, "stale-address window after a service change: watch-driven %.0fms avg, TTL-only %.0fms avg (%.0fms worst case)\n",
				b.StaleAvgMS, d.StaleAvgMS, d.StaleMaxMS)
		}
	}
	fmt.Fprintf(w, "\n%d apps; worst amplification (queries per lookup):\n", int64(len(rep.Profiles[0].Apps)))
	for i, app := range rep.Profiles[0].Apps {
		if i >= 8 {
			break
		}
		fmt.Fprintf(w, "  %-10s %-22s %.1f q/lookup, %.1f%% wasted, p99 %.0fms\n",
			app.Name, app.Style, app.AvgQueries, app.WastedRatio*100, app.LookupP99MS)
	}
}

func jsonBytes(rep *Report) ([]byte, error) {
	return json.Marshal(rep)
}
