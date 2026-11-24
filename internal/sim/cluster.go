package sim

import (
	"fmt"
	"math"
	"strings"

	"dnsim/internal/engine"
	"dnsim/internal/resolv"
)

// Cluster holds the generated topology.
type Cluster struct {
	Config    Config
	Services  []*Service
	Externals []*ExternalDomain
	Apps      []*App
	Nodes     []*Node

	// serviceByName indexes services by FQDN (rooted).
	serviceByName map[string]*Service
	// internalZone lists every name that exists inside the cluster, used both
	// by the authoritative zone and by the "wasted query" classifier.
	internalZone map[string]bool
	// externalByName indexes external domains by rooted name.
	externalByName map[string]*ExternalDomain
}

// BuildCluster generates a deterministic topology. Use it when a test or the
// live-zone bridge needs a cluster without running the full simulation.
func BuildCluster(cfg Config, seed uint64) *Cluster {
	return buildCluster(cfg, engine.NewRand(seed, 0x3333))
}

// buildCluster generates a deterministic topology.
func buildCluster(cfg Config, r *engine.Rand) *Cluster {
	c := &Cluster{
		Config:         cfg,
		serviceByName:  make(map[string]*Service),
		internalZone:   make(map[string]bool),
		externalByName: make(map[string]*ExternalDomain),
	}
	// Service names are drawn from a realistic catalogue; the same short
	// name is reused across namespaces, which is exactly why short-name
	// lookups depend on the search list.
	namePool := []string{
		"api", "web", "frontend", "backend", "auth", "users", "orders", "cart",
		"payment", "billing", "inventory", "catalog", "search", "recommend",
		"notify", "email", "sms", "chat", "feed", "timeline", "media", "upload",
		"image", "video", "transcode", "stream", "session", "token", "profile",
		"analytics", "metrics", "trace", "logging", "audit", "config", "flag",
		"cron", "scheduler", "worker", "queue", "events", "kafka", "redis",
		"postgres", "mysql", "mongo", "elastic", "minio", "vault", "gateway",
	}
	nsBase := []string{
		"prod", "prod-payments", "prod-catalog", "prod-identity", "prod-search",
		"staging", "canary", "infra", "platform", "observability", "data",
		"ml", "batch", "edge", "internal", "shared",
	}
	nsPool := make([]string, cfg.Namespaces)
	if cfg.Namespaces > 16 {
		// Real namespaces are mostly team/service names with a short random
		// suffix, not repeated bases: make every one unique.
		for i := range nsPool {
			nsPool[i] = fmt.Sprintf("%s-%s", nsBase[i%len(nsBase)], nsSuffix(i))
		}
	}
	for i := range nsPool {
		if nsPool[i] != "" {
			continue
		}
		if i < len(nsBase) {
			nsPool[i] = nsBase[i]
		} else {
			nsPool[i] = fmt.Sprintf("%s-%d", nsBase[i%len(nsBase)], i/len(nsBase))
		}
	}
	nTotal := cfg.Namespaces * cfg.ServicesPerNS
	c.Services = make([]*Service, 0, nTotal)
	ipCounter := 0
	for ns := 0; ns < cfg.Namespaces; ns++ {
		nsName := nsPool[ns]
		for s := 0; s < cfg.ServicesPerNS; s++ {
			short := namePool[(ns*7+s*3)%len(namePool)]
			if s >= len(namePool) {
				short = fmt.Sprintf("%s-%d", short, s/len(namePool))
			}
			ipCounter++
			v4 := fmt.Sprintf("10.%d.%d.%d", 96+(ipCounter>>16)%32, (ipCounter>>8)&0xff, ipCounter&0xff)
			v6 := fmt.Sprintf("fd00:10:96::%x", 0x1000+ipCounter)
			svc := &Service{
				Name:           short,
				Namespace:      nsName,
				FQDN:           fmt.Sprintf("%s.%s.svc.%s.", short, nsName, ClusterDomain),
				IPv4:           v4,
				IPv6:           v6,
				HoldsEndpoints: r.Chance(0.94),
				EndpointV4:     v4,
				Version:        1,
			}
			c.Services = append(c.Services, svc)
			c.serviceByName[svc.FQDN] = svc
			c.internalZone[svc.FQDN] = true
		}
		// Each namespace also gets a namespace-wide headless entry and the
		// service.namespace.svc.cluster.local shape.
		c.internalZone[fmt.Sprintf("%s.svc.%s.", nsName, ClusterDomain)] = true
	}
	// Kubernetes also resolves service.namespace.svc.cluster.local to the same
	// record set, so register the two-label form as an existing name too.
	for _, svc := range c.Services {
		twoLabel := fmt.Sprintf("%s.%s.svc.%s.", svc.Name, svc.Namespace, ClusterDomain)
		c.internalZone[twoLabel] = true
	}

	// Nodes and container counts. Containers are pods, not apps; several apps
	// may share a node.
	c.Nodes = make([]*Node, cfg.Nodes)
	for i := range c.Nodes {
		c.Nodes[i] = &Node{
			Index:      i,
			Name:       fmt.Sprintf("node-%03d", i),
			Containers: cfg.ContainersPerNode,
			IP:         fmt.Sprintf("10.200.%d.%d", i>>8, i&0xff),
		}
	}

	// External catalogue: popular SaaS/API names weigh more.
	extNames := []string{
		"api.stripe.com", "api.github.com", "github.com", "objects.githubusercontent.com",
		"api.sendgrid.com", "s3.amazonaws.com", "sqs.us-east-1.amazonaws.com",
		"dynamodb.us-east-1.amazonaws.com", "api.twilio.com", "api.openai.com",
		"api.anthropic.com", "login.microsoftonline.com", "graph.microsoft.com",
		"api.slack.com", "hooks.slack.com", "api.zoom.us", "www.googleapis.com",
		"oauth2.googleapis.com", "storage.googleapis.com", "pubsub.googleapis.com",
		"api.pagerduty.com", "api.datadoghq.com", "api.segment.io", "cdn.jsdelivr.net",
		"fonts.googleapis.com", "www.google-analytics.com", "api.mixpanel.com",
		"api.intercom.io", "api.hubspot.com", "api.salesforce.com", "api.quickbooks.com",
		"api.adyen.com", "api.braintreegateway.com", "api.paypal.com",
		"api.aws.amazon.com", "ec2.us-east-1.amazonaws.com", "api.cloudflare.com",
		"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org",
		"sum.golang.org", "dl.google.com", "update.googleapis.com", "api.heroku.com",
		"api.digitalocean.com", "api.vercel.com", "api.netlify.com", "api.fly.io",
		"api.sentry.io", "api.newrelic.com", "api.honeycomb.io", "api.grafana.com",
		"api.statuspage.io", "api.opsgenie.com", "api.victorops.com", "api.rollbar.com",
		"cdn.sanity.io", "images.ctfassets.net", "assets.contentful.com",
		"api.contentful.com", "api.algolia.com", "search.algolia.com",
		"api.mapbox.com", "api.tiles.mapbox.com", "api.here.com", "api.tomtom.com",
		"api.weather.com", "api.forecast.io", "api.currencylayer.com",
		"api.exchangerate-api.com", "api.coingecko.com", "api.binance.com",
	}
	c.Externals = make([]*ExternalDomain, 0, cfg.ExternalDomains)
	for i := 0; i < cfg.ExternalDomains; i++ {
		var name string
		if i < len(extNames) {
			name = extNames[i]
		} else {
			name = fmt.Sprintf("svc%03d.partner-external.example", i)
		}
		w := 1.0
		// Power-law popularity: the first few names dominate.
		w = math.Pow(float64(i+1), -0.9)
		if w < 0.05 {
			w = 0.05
		}
		d := &ExternalDomain{
			Name:    resolv.Rooted(name),
			IPv4:    fmt.Sprintf("203.0.%d.%d", 100+i/250, 1+i%250),
			IPv6:    fmt.Sprintf("2001:db8:%x::%x", i/0x1000, 1+i%0x1000),
			TTL:     uint32(r.IntRange(30, 300)),
			Weight:  w,
			HasAAAA: r.Chance(0.7),
		}
		c.Externals = append(c.Externals, d)
		c.externalByName[d.Name] = d
	}

	// Apps. Each app pins to one node (so it uses that node's cache) and gets
	// a resolver configuration drawn from the cluster's configuration
	// distribution.
	appNames := make([]string, cfg.Apps)
	for i := range appNames {
		appNames[i] = fmt.Sprintf("app-%03d", i)
	}
	c.Apps = make([]*App, cfg.Apps)
	for i := range c.Apps {
		node := r.IntN(cfg.Nodes)
		app := &App{
			Name:           appNames[i],
			Namespace:      nsPool[i%len(nsPool)],
			Node:           node,
			QueryRate:      cfg.BaseQPSPerApp,
			InClusterShare: cfg.InClusterShare,
			Weight:         1,
		}
		// Configuration variants. A share of pods has a hand-edited
		// resolv.conf (lower ndots, trailing dots, IPv4-only).
		switch {
		case r.Chance(cfg.ModifiedShare):
			app.RawNdots = 2
			app.Style = "modified-ndots2"
		case r.Chance(0.05):
			app.RawNdots = 1
			app.Style = "legacy-ndots1"
		default:
			app.RawNdots = cfg.DefaultNdots
			app.Style = fmt.Sprintf("default-ndots%d", cfg.DefaultNdots)
		}
		app.RootedShare = cfg.RootedShare
		app.InClusterShare = cfg.InClusterShare
		ipv4Only := r.Chance(cfg.IPv4OnlyShare)
		if ipv4Only {
			app.Style += "+ipv4"
		}
		serial := r.Chance(cfg.SerializerShare)
		policy, _ := resolv.ParsePolicy(cfg.ClientPolicy)
		fams := resolv.FamiliesBoth
		if ipv4Only {
			fams = resolv.FamiliesIPv4
		}
		// Build the search list the way kubelet does for this namespace.
		ns := app.Namespace
		app.Config = resolv.Config{
			Nameservers: []string{"10.96.0.10:53"},
			Search: []string{
				fmt.Sprintf("%s.svc.%s.", ns, ClusterDomain),
				fmt.Sprintf("svc.%s.", ClusterDomain),
				fmt.Sprintf("%s.", ClusterDomain),
				fmt.Sprintf("%s.", RegionDomain),
			},
			Ndots:          app.RawNdots,
			Timeout:        timeSeconds(cfg.TimeoutSeconds),
			Attempts:       cfg.Attempts,
			Policy:         policy,
			Families:       fams,
			SerialFamilies: serial,
		}
		// Each app calls a subset of services: its own namespace preferentially
		// plus popular shared services in other namespaces.
		nServices := r.IntRange(4, 14)
		seen := map[*Service]bool{}
		for len(app.Services) < nServices {
			var svc *Service
			if r.Chance(0.6) {
				// A service in its own namespace: short name.
				nsServices := c.servicesInNamespace(app.Namespace)
				if len(nsServices) > 0 {
					svc = nsServices[r.IntN(len(nsServices))]
				}
			}
			if svc == nil {
				svc = c.Services[r.IntN(len(c.Services))]
			}
			if !seen[svc] {
				seen[svc] = true
				app.Services = append(app.Services, svc)
			}
		}
		nExternal := r.IntRange(8, 40)
		for len(app.Externals) < nExternal {
			d := c.Externals[r.IntN(len(c.Externals))]
			dup := false
			for _, e := range app.Externals {
				if e == d {
					dup = true
					break
				}
			}
			if !dup {
				app.Externals = append(app.Externals, d)
			}
		}
		c.Apps[i] = app
	}
	return c
}

// nsSuffix returns a short deterministic suffix for namespace i.
func nsSuffix(i int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	a := alphabet[(i/36)%len(alphabet)]
	b := alphabet[i%len(alphabet)]
	return string([]byte{a, b})
}

func (c *Cluster) servicesInNamespace(ns string) []*Service {
	var out []*Service
	for _, s := range c.Services {
		if s.Namespace == ns {
			out = append(out, s)
		}
	}
	return out
}

// LookupName answers a query from the authoritative zone: it returns the
// record set for a rooted name plus whether the name exists and whether it
// has records of the requested type.
// ServiceTTL is the record TTL CoreDNS serves for cluster services. It is
// short because endpoints change; node caches additionally invalidate on
// watch events instead of waiting for it.
const ServiceTTL uint32 = 30

// LookupName answers a query from the authoritative zones. It returns the
// address set, the record TTL in seconds, whether the name exists and whether
// it exists without records of the requested type (NODATA).
func (c *Cluster) LookupName(name string, qtype uint16) (ips []string, ttl uint32, exists bool, noData bool) {
	if svc, ok := c.serviceByName[name]; ok {
		if !svc.HoldsEndpoints {
			// Service with no endpoints: the name exists but has no address
			// records (NODATA), which is a common production case.
			return nil, ServiceTTL, true, true
		}
		if qtype == resolv.TypeA {
			return []string{svc.IPv4}, ServiceTTL, true, false
		}
		return []string{svc.IPv6}, ServiceTTL, true, false
	}
	if d, ok := c.externalByName[name]; ok {
		if qtype == resolv.TypeA {
			return []string{d.IPv4}, d.TTL, true, false
		}
		if d.HasAAAA {
			return []string{d.IPv6}, d.TTL, true, false
		}
		return nil, d.TTL, true, true
	}
	// Names that exist as zone entries (e.g. svc.cluster.local.) answer
	// NODATA for A/AAAA.
	if c.internalZone[name] {
		return nil, ServiceTTL, true, true
	}
	return nil, 0, false, false
}

// CanResolve reports whether a rooted candidate name exists in the cluster or
// in the external catalogue. A candidate that can never resolve is a wasted
// search-list query.
func (c *Cluster) CanResolve(name string) bool {
	if _, ok := c.serviceByName[name]; ok {
		return true
	}
	if _, ok := c.externalByName[name]; ok {
		return true
	}
	return false
}

// IsWastedName reports whether a rooted candidate can never resolve: it is
// neither a cluster service, nor a zone entry, nor a known external name.
// Those are exactly the search-list queries that were doomed from the start
// (the amplified "invalid" queries the diagnosis has to count).
func (c *Cluster) IsWastedName(name string) bool {
	if c.internalZone[name] {
		return false
	}
	if _, ok := c.serviceByName[name]; ok {
		return false
	}
	if _, ok := c.externalByName[name]; ok {
		return false
	}
	return true
}

// ResolveName maps a name as an application wrote it (possibly a short name
// that only works through the search list) to the cluster service it denotes,
// using the application's own search list. It returns "" when no candidate
// names a service. This is the attribution used by the report, so a lookup of
// "redis" is grouped under redis.<ns>.svc.cluster.local.
func (c *Cluster) ResolveName(name string, cfg resolv.Config) string {
	for _, cand := range cfg.NameList(name) {
		if svc, ok := c.serviceByName[cand]; ok {
			return svc.FQDN
		}
	}
	return ""
}

// Canonical returns the name the application meant, for a candidate that was
// found. It strips search suffixes and re-roots, which is used in the report
// to group statistics by logical name.
func (c *Cluster) Canonical(name string) string {
	if _, ok := c.serviceByName[name]; ok {
		return name
	}
	if _, ok := c.externalByName[name]; ok {
		return name
	}
	// service.namespace: look up the fully qualified form directly.
	if idx := strings.IndexByte(name, '.'); idx > 0 {
		base := name[:idx]
		ns := strings.TrimSuffix(name[idx+1:], ".")
		if fqdn := fmt.Sprintf("%s.%s.svc.%s.", base, ns, ClusterDomain); c.serviceByName[fqdn] != nil {
			return fqdn
		}
	}
	// Try stripping each known search suffix.
	for _, suffix := range []string{
		".svc." + ClusterDomain + ".", "." + ClusterDomain + ".", "." + RegionDomain + ".",
	} {
		if strings.HasSuffix(name, suffix) {
			base := strings.TrimSuffix(name, suffix)
			if !strings.HasSuffix(base, ".") {
				base += "."
			}
			if _, ok := c.serviceByName[base]; ok {
				return base
			}
			if _, ok := c.externalByName[base]; ok {
				return base
			}
		}
	}
	return name
}
