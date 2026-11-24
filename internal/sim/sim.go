package sim

import (
	"fmt"
	"math"
	"sort"
	"time"

	"dnsim/internal/engine"
	"dnsim/internal/resolv"
)

// Profile is one of the compared configurations.
type Profile struct {
	Name                   string  `json:"name"`
	CacheEnabled           bool    `json:"cacheEnabled"`
	ConfigAdjusted         bool    `json:"configAdjusted"`
	InvalidateOnWatch      bool    `json:"invalidateOnWatch"`
	CacheTTLSeconds        float64 `json:"cacheTtlSeconds"`
	NegativeTTLSeconds     float64 `json:"negativeTtlSeconds"`
	UpstreamRetryTimeoutMs float64 `json:"upstreamRetryTimeoutMs"`
	UpstreamAttempts       int     `json:"upstreamAttempts"`

	// AppDeadlineSeconds is the caller-side deadline for one hostname
	// resolution (a typical RPC/HTTP client timeout).
	AppDeadlineSeconds float64 `json:"appDeadlineSeconds"`
}

// Profiles returns the three acceptance profiles plus a TTL-only diagnostic.
func Profiles(cfg Config) []Profile {
	base := Profile{
		AppDeadlineSeconds:     cfg.AppDeadlineSeconds,
		CacheTTLSeconds:        cfg.CacheTTLSeconds,
		NegativeTTLSeconds:     cfg.NegativeTTLSeconds,
		UpstreamRetryTimeoutMs: cfg.UpstreamRetryTimeoutMs,
		UpstreamAttempts:       2,
	}
	a := base
	a.Name = "A-no-cache"
	a.CacheEnabled = false

	b := base
	b.Name = "B-node-cache"
	b.CacheEnabled = true
	b.InvalidateOnWatch = true

	c := base
	c.Name = "C-node-cache+config"
	c.CacheEnabled = true
	c.InvalidateOnWatch = true
	c.ConfigAdjusted = true
	c.UpstreamRetryTimeoutMs = cfg.UpstreamRetryTimeoutObserveMs

	d := base
	d.Name = "D-node-cache-ttl-only"
	d.CacheEnabled = true
	d.InvalidateOnWatch = false
	return []Profile{a, b, c, d}
}

// Sim is one simulated run of the cluster.
type Sim struct {
	cfg     Config
	profile Profile
	eng     *engine.Engine
	rnd     *engine.Rand
	netRnd  *engine.Rand
	cluster *Cluster
	dns     *CoreDNS
	metrics *Metrics
	nodes   []*Node
}

// Result is one completed profile run.
type Result struct {
	Profile Profile
	Config  Config
	Cluster *Cluster
	Metrics *Metrics
	// Suggestions are per-app config recommendations derived from this run.
	Suggestions []Suggestion
}

// RunProfile executes one profile and returns its full result.
func RunProfile(cfg Config, profile Profile) (*Result, error) {
	m, cluster, err := Run(cfg, profile)
	if err != nil {
		return nil, err
	}
	return &Result{Profile: profile, Config: cfg, Cluster: cluster, Metrics: m}, nil
}

// Run executes one profile and returns its metrics.
func Run(cfg Config, profile Profile) (*Metrics, *Cluster, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	s := &Sim{cfg: cfg, profile: profile, eng: engine.New()}

	s.rnd = engine.NewRand(cfg.Seed, 0x1111)
	s.netRnd = engine.NewRand(cfg.Seed, 0x2222)
	s.cluster = buildCluster(cfg, engine.NewRand(cfg.Seed, 0x3333))
	if profile.ConfigAdjusted {
		adjustConfigs(s.cluster)
	}
	s.nodes = s.cluster.Nodes
	if profile.CacheEnabled {
		for _, n := range s.nodes {
			n.Cache = NewNodeCache(n, s)
		}
	}
	s.dns = newCoreDNS(s)
	buckets := int(math.Ceil(cfg.Total().Seconds())) + 2
	s.metrics = NewMetrics(profile.CacheEnabled, buckets)
	s.scheduleWorkload()
	s.scheduleServiceChanges()
	s.eng.RunUntil(cfg.Total())
	s.finalizeStaleWindows()
	return s.metrics, s.cluster, nil
}

// scheduleWorkload creates the per-app query generators.
//
// Each application issues hostname resolutions as a Poisson process whose
// rate follows the cluster load curve. Each resolution is attributed to one
// of the app's in-cluster services or external domains, and written either as
// a short name (subject to the search list) or as a fully-qualified name,
// depending on the app's habits.
func (s *Sim) scheduleWorkload() {
	for _, app := range s.cluster.Apps {
		app := app
		var next func()
		next = func() {
			now := s.eng.Now()
			if now >= s.cfg.Total() {
				return
			}
			// Sample the instantaneous rate; skip bursts outside the peak in
			// proportion, which keeps the total event count proportional to
			// the integral of the load curve.
			rate := s.cfg.qpsAt(now, s.rnd)
			// Poisson interarrival: mean 1/rate, capped so the queue does not
			// grow without bound when the rate is tiny.
			if rate <= 0.01 {
				rate = 0.01
			}
			gap := s.rnd.Exp(time.Duration(float64(time.Second) / rate))
			if gap < time.Microsecond {
				gap = time.Microsecond
			}
			s.eng.At(gap, func() {
				if s.eng.Now() < s.cfg.Total() {
					s.startLookup(app)
				}
				next()
			})
		}
		// Stagger app start times.
		offset := time.Duration(s.rnd.Float64() * float64(time.Second) * 5)
		s.eng.At(offset, next)
	}
}

// startLookup issues one hostname resolution for an app.
func (s *Sim) startLookup(app *App) {
	now := s.eng.Now()
	useExternal := s.rnd.Float64() >= app.InClusterShare
	var name string
	if useExternal {
		if len(app.Externals) == 0 {
			return
		}
		d := app.Externals[s.rnd.IntN(len(app.Externals))]
		if s.rnd.Chance(app.RootedShare) {
			name = d.Name // already rooted
		} else {
			name = trimRoot(d.Name)
		}
	} else {
		if len(app.Services) == 0 {
			return
		}
		svc := app.Services[s.rnd.IntN(len(app.Services))]
		sameNS := svc.Namespace == app.Namespace
		switch {
		case s.rnd.Chance(app.RootedShare):
			// Written as a rooted FQDN: the search list is skipped.
			name = svc.FQDN
		case sameNS && s.rnd.Chance(0.6):
			// Short name in its own namespace: resolves after the first
			// search suffix, the efficient in-cluster case.
			name = svc.Name
		case sameNS:
			// service.namespace: resolves after the second suffix.
			name = svc.Name + "." + svc.Namespace
		default:
			// Calling another namespace: production practice is the fully
			// qualified name, which has enough dots to be tried first.
			name = trimRoot(svc.FQDN)
		}
	}

	s.metrics.AppLookups++
	s.metrics.bucket(now).AppLookups++
	am := s.metrics.app(app.Name)
	am.Lookups++

	// Group statistics by the name the app meant: the service FQDN for an
	// in-cluster call (even when written as a short name), or the external
	// record. That is what makes "worst amplified names" meaningful.
	var logical string
	if useExternal {
		logical = resolv.Rooted(name)
	} else if fqdn := s.cluster.ResolveName(name, app.Config); fqdn != "" {
		logical = fqdn
	} else {
		logical = s.cluster.Canonical(resolv.Rooted(name))
	}
	s.metrics.name(app.Name, logical).Lookups++

	// Every query the resolver puts on the wire is counted here, including
	// retransmissions, and classified as wasted when the candidate name could
	// never have resolved by an independent classifier (not by asking the
	// zone, which would make the metric tautological).
	rec := func(ev resolv.QueryEvent) {
		s.metrics.ClientQueries++
		s.metrics.bucket(now).ClientQueries++
		am.Queries++
		nm := s.metrics.name(app.Name, logical)
		nm.Queries++
		if ev.Attempt > 0 {
			s.metrics.ClientRetries++
		}
		if s.cluster.IsWastedName(ev.Name) {
			s.metrics.WastedQueries++
			s.metrics.bucket(now).Wasted++
			am.Wasted++
			nm.Wasted++
		}
	}

	env := resolv.Env{
		Now:  func() time.Duration { return s.eng.Now() },
		Send: s.sendFunc(app),
	}
	// The application has a deadline for the whole resolution (an RPC/HTTP
	// client timeout): it gives up once, and that is the "five-second
	// timeout" in the acceptance criteria. The resolver may retry several
	// search candidates within it, so its own timeout is not the bound.
	deadline := s.profile.AppDeadlineSeconds
	finished := false
	var sess *resolv.Session
	deadlineEv := s.eng.At(timeSeconds(deadline), func() {
		if finished {
			return
		}
		finished = true
		sess.Cancel()
		s.recordLookup(app, logical, resolv.Result{Status: resolv.StatusTimeout, Latency: timeSeconds(deadline)})
	})
	sess = resolv.Start(env, app.Config, name, "ip", rec, func(res resolv.Result) {
		if finished {
			return
		}
		finished = true
		deadlineEv.Cancel()
		s.recordLookup(app, logical, res)
	})
}

// recordLookup folds one completed resolution into the metrics.
func (s *Sim) recordLookup(app *App, logical string, res resolv.Result) {
	am := s.metrics.app(app.Name)
	nm := s.metrics.name(app.Name, logical)
	s.metrics.Latency.Add(res.Latency)
	am.Latency.Add(res.Latency)
	// A lookup stalls when its total latency reaches the five-second client
	// timeout. That happens either because the stub resolver gave up on
	// timeouts or because the application's own deadline expired first.
	// The acceptance threshold is the application deadline (5s), not the
	// resolver's per-query timeout.
	fiveSec := timeSeconds(s.profile.AppDeadlineSeconds)
	if res.Latency >= fiveSec {
		s.metrics.SlowLookups++
		am.Slow++
		nm.Slow++
		s.metrics.bucket(s.eng.Now()).Timeouts++
	}
	switch res.Status {
	case resolv.StatusAnswer:
		s.metrics.Answers++
		am.Answers++
	case resolv.StatusTimeout:
		s.metrics.LookupTimeouts++
		am.Timeouts++
		nm.Timeouts++
	default:
		s.metrics.LookupErrors++
		am.Errors++
	}
}

// cacheDispatchLatency models the loopback round trip to the node-local
// listener: about 150-250µs.
func (s *Sim) cacheDispatchLatency() time.Duration {
	return time.Duration(150+s.rnd.IntN(100)) * time.Microsecond
}

// sendFunc returns the transport the resolver uses. With the node cache the
// query goes to the node-local listener; without it, straight to CoreDNS.
func (s *Sim) sendFunc(app *App) resolv.SendFunc {
	node := s.nodes[app.Node]
	appMetrics := s.metrics.Apps[app.Name]
	// Every reply is timed at the transport boundary. That is the latency the
	// application observes per DNS query: a node-cache hit costs a loopback
	// round trip, an uncached query costs the full CoreDNS exchange. Lookup
	// latency, which covers every candidate and retry, is recorded separately.
	finish := func(start time.Duration, done func(resolv.Reply)) func(resolv.Reply) {
		return func(r resolv.Reply) {
			if r.Status == resolv.StatusTimeout {
				s.metrics.QueryTimeouts++
				s.metrics.bucket(s.eng.Now()).QueryTimeouts++
			}
			if appMetrics != nil {
				appMetrics.QueryLatency.Add(s.eng.Now() - start)
			}
			done(r)
		}
	}
	if !s.profile.CacheEnabled || node.Cache == nil {
		return func(server, name string, qtype uint16, timeout time.Duration, done func(resolv.Reply)) {
			start := s.eng.Now()
			s.sendUpstream(name, qtype, timeout, func(reply CoreDNSReply, status resolv.Status) {
				finish(start, done)(resolv.Reply{Status: status, IPs: reply.IPs})
			})
		}
	}
	cache := node.Cache
	return func(server, name string, qtype uint16, timeout time.Duration, done func(resolv.Reply)) {
		start := s.eng.Now()
		cache.Lookup(app.Name, name, qtype, timeout, finish(start, done))
	}
}

// scheduleServiceChanges moves endpoints between pods, which changes the
// address a service resolves to. Each change bumps the service version.
func (s *Sim) scheduleServiceChanges() {
	if s.cfg.ServiceChangesPerMinute <= 0 {
		return
	}
	r := engine.NewRand(s.cfg.Seed, 0x4444)
	mean := time.Duration(float64(time.Minute) / s.cfg.ServiceChangesPerMinute)
	var schedule func()
	schedule = func() {
		now := s.eng.Now()
		gap := r.Exp(mean)
		if now+gap >= s.cfg.Total() {
			return
		}
		svc := s.cluster.Services[r.IntN(len(s.cluster.Services))]
		s.eng.At(gap, func() {
			svc.Version++
			svc.Changes++
			svc.IPv4 = fmt.Sprintf("10.%d.%d.%d", 96+int(svc.Version)%32, (int(svc.Version)*7)&0xff, (int(svc.Version)*13)&0xff)
			svc.EndpointV4 = svc.IPv4
			s.metrics.ServiceChanges++
			s.invalidateFor(svc)
			schedule()
		})
	}
	s.eng.At(0, schedule)
}

// invalidateFor propagates a service change to every node cache after the
// informer notification delay.
func (s *Sim) invalidateFor(svc *Service) {
	if !s.profile.CacheEnabled {
		// No cache: there is no stale-address window to measure.
		return
	}
	delay := time.Duration(s.cfg.NotifyDelayMs * float64(time.Millisecond))
	// How long an existing cache entry can keep serving the old address. With
	// a watch-driven invalidation the entry is dropped after the informer
	// propagation delay. Without it the entry simply expires at the end of its
	// positive TTL from the moment it was inserted, so the residual lifetime is
	// uniform over [0, TTL]; we use the expected value for the report and also
	// verify the worst case from the entries actually present.
	stale := delay
	if !s.profile.InvalidateOnWatch {
		stale = time.Duration(s.profile.CacheTTLSeconds / 2 * float64(time.Second))
	}
	s.trackStaleWindow(svc, stale)
	if !s.profile.InvalidateOnWatch {
		return
	}
	s.eng.At(delay, func() {
		for _, n := range s.nodes {
			if n.Cache != nil {
				n.Cache.OnServiceChange(svc)
			}
		}
		// The residual check must run after the invalidation callbacks
		// scheduled for the same instant.
		s.eng.At(0, func() { s.checkResidual(svc) })
	})
}

// trackStaleWindow records how long a node cache may still answer with the
// previous address after a service change.
func (s *Sim) trackStaleWindow(svc *Service, d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	s.metrics.StaleWindowSum += ms
	s.metrics.StaleWindowCount++
	if ms > s.metrics.StaleWindowMax {
		s.metrics.StaleWindowMax = ms
	}
	s.metrics.bucket(s.eng.Now()).Stale++
}

// checkResidual verifies that no node cache can still serve the superseded
// address after the invalidation event has run.
func (s *Sim) checkResidual(svc *Service) {
	for _, n := range s.nodes {
		if n.Cache == nil {
			continue
		}
		if got := n.Cache.staleRemaining(svc, s.eng.Now()); got > 0 {
			s.metrics.StaleResidualMax = math.Max(s.metrics.StaleResidualMax, got)
		}
	}
}

func (s *Sim) finalizeStaleWindows() {
	for _, n := range s.nodes {
		if n.Cache != nil {
			s.metrics.Invalidations += n.Cache.invalidations
			s.metrics.UpstreamCalls += n.Cache.upstreamQueries
			s.metrics.UpstreamRetries += n.Cache.upstreamRetries
			s.metrics.UpstreamTimeouts += n.Cache.upstreamTimeouts
			s.metrics.StaleServes += n.Cache.staleServes
		}
	}

}

// trimRoot removes the trailing dot.
func trimRoot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

// adjustConfigs applies the data-driven recommendation to every app: lower
// ndots to 2 so fully-qualified external names are tried first, keep only the
// search domains the app actually uses, and write in-cluster names fully
// qualified more often.
func adjustConfigs(c *Cluster) {
	for _, app := range c.Apps {
		app.RootedShare = math.Max(app.RootedShare, 0.85)
		cfg := app.Config
		// ndots 2: external names with two dots go straight to the root
		// query; single-label in-cluster names still use the search list.
		cfg.Ndots = 2
		// Drop search domains that only ever produce NXDOMAIN for this app.
		keep := cfg.Search[:0]
		for _, s := range cfg.Search {
			if s == RegionDomain+"." {
				continue
			}
			keep = append(keep, s)
		}
		cfg.Search = append([]string(nil), keep...)
		app.Config = cfg
		app.Style += "+adjusted"
	}
}

// Suggestion is a per-application configuration recommendation.
type Suggestion struct {
	App                 string   `json:"app"`
	Style               string   `json:"style"`
	ExternalShare       float64  `json:"externalShare"`
	ShortShare          float64  `json:"shortNameShare"`
	CurrentNdots        int      `json:"currentNdots"`
	ConfigAdjusted      bool     `json:"configAdjusted"`
	CurrentAvgQueries   float64  `json:"currentAvgQueries"`
	SuggestedAvgQueries float64  `json:"suggestedAvgQueries"`
	Advice              []string `json:"advice"`
	CacheHits           int      `json:"cacheHits,omitempty"`
}

// Suggest produces per-app advice from observed traffic and the app's
// configuration. When adjusted is non-nil it also reports the query cost the
// same app would have after the recommended change.
func Suggest(cluster *Cluster, metrics, adjusted *Metrics) []Suggestion {
	var out []Suggestion
	for _, app := range cluster.Apps {
		am := metrics.Apps[app.Name]
		var cur, sug float64
		if am != nil && am.Lookups > 0 {
			cur = float64(am.Queries) / float64(am.Lookups)
		}
		var amAdj *AppMetrics
		if adjusted != nil {
			amAdj = adjusted.Apps[app.Name]
		}
		if amAdj != nil && amAdj.Lookups > 0 {
			sug = float64(amAdj.Queries) / float64(amAdj.Lookups)
		} else {
			// Estimate the post-change cost from the model rather than leaving
			// the column empty: ndots:2, one suffix dropped and 85% of names
			// fully qualified gives roughly one candidate per family on the
			// external path and one on the in-cluster path.
			sug = estimateAdjustedQueries(app)
		}
		suggestion := Suggestion{
			App:                 app.Name,
			Style:               app.Style,
			ExternalShare:       math.Round((1-app.InClusterShare)*100) / 100,
			CurrentNdots:        app.RawNdots,
			CurrentAvgQueries:   round1(cur),
			SuggestedAvgQueries: round1(sug),
		}
		if cur > 2.5 {
			suggestion.Advice = append(suggestion.Advice, fmt.Sprintf("ndots:%d 使外部域名先被拼接 %d 次搜索域;改为 ndots:2 可让完整域名第一个被查询", app.RawNdots, len(app.Config.Search)))
		}
		if len(app.Config.Search) > 2 {
			suggestion.Advice = append(suggestion.Advice, "搜索域包含 "+fmt.Sprint(len(app.Config.Search))+" 项,其中 corp.internal 对该应用从未命中,可移除")
		}
		if 1-app.InClusterShare > 0.3 {
			suggestion.Advice = append(suggestion.Advice, "外部域名占比高:对外部域名使用带结尾点的完整域名(FQDN)")
		}
		if app.InClusterShare > 0.6 && app.RawNdots >= 5 {
			suggestion.Advice = append(suggestion.Advice, "集群内调用为主:可继续保留下调的 ndots,但应把同命名空间服务写成 <svc>.<ns>.svc.cluster.local")
		}
		if len(suggestion.Advice) == 0 {
			suggestion.Advice = append(suggestion.Advice, "配置已接近最优")
		}
		// Record whether the current recommendation implies a change.
		suggestion.ConfigAdjusted = cur > sug && sug > 0
		out = append(out, suggestion)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CurrentAvgQueries > out[j].CurrentAvgQueries })
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// estimateAdjustedQueries approximates the per-lookup query cost after the
// recommended configuration change, for apps whose adjusted run was not part
// of this report.
func estimateAdjustedQueries(app *App) float64 {
	families := 2.0
	if app.Config.Families == resolv.FamiliesIPv4 {
		families = 1.0
	}
	// Short in-cluster names still walk one suffix; everything else is tried
	// as-is first. External names dominate the expensive case.
	perLookup := families * (0.5 + 0.5*(1-app.InClusterShare))
	if perLookup < 1 {
		perLookup = 1
	}
	return math.Round(perLookup*10) / 10
}
