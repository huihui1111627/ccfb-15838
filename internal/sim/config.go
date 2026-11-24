// Package sim models a Kubernetes-like cluster under DNS peak load and runs
// the three experiments required by the acceptance criteria: no cache, node
// cache, and node cache plus resolver-config changes.
//
// Everything runs on a virtual clock (internal/engine) in one goroutine, so
// thousands of services, thousands of containers and millions of DNS packets
// simulate in seconds and are reproducible from a seed.
package sim

import (
	"fmt"
	"math"
	"time"

	"dnsim/internal/engine"
	"dnsim/internal/resolv"
)

// Cluster parameters.

const (
	// ClusterDomain is the cluster DNS domain.
	ClusterDomain = "cluster.local"
	// RegionDomain is a second search domain seen in many real clusters
	// (split-horizon corporate zone).
	RegionDomain = "corp.internal"
)

// Config is the full simulation configuration.
type Config struct {
	Seed uint64 `json:"seed"`

	// Topology.
	Namespaces        int `json:"namespaces"`
	ServicesPerNS     int `json:"servicesPerNamespace"`
	Nodes             int `json:"nodes"`
	ContainersPerNode int `json:"containersPerNode"`

	// Workload.
	Apps            int        `json:"apps"`
	DurationMinutes float64    `json:"durationMinutes"`
	PeakWindow      [2]float64 `json:"peakWindowFraction"`
	BaseQPSPerApp   float64    `json:"baseQPSPerApp"`
	PeakQPSPerApp   float64    `json:"peakQPSPerApp"`
	InClusterShare  float64    `json:"inClusterShare"`
	ExternalDomains int        `json:"externalDomains"`

	// Resolver.
	DefaultNdots       int     `json:"defaultNdots"`
	AppDeadlineSeconds float64 `json:"appDeadlineSeconds"`
	ModifiedShare      float64 `json:"modifiedShare"`
	RootedShare        float64 `json:"rootedShare"`
	IPv4OnlyShare      float64 `json:"ipv4OnlyShare"`
	SerializerShare    float64 `json:"singleRequestShare"`
	TimeoutSeconds     float64 `json:"timeoutSeconds"`
	Attempts           int     `json:"attempts"`
	ClientPolicy       string  `json:"clientPolicy"`

	// DNS service.
	CoreDNSReplicas int     `json:"corednsReplicas"`
	CoreDNSCapacity float64 `json:"corednsCapacityQPS"` // per replica

	// Network.
	LossRate     float64 `json:"lossRate"`
	PeakLossRate float64 `json:"peakLossRate"`
	RTTMeanMs    float64 `json:"rttMeanMs"`
	RTTStdMs     float64 `json:"rttStdMs"`

	// Cache.
	CacheTTLSeconds               float64 `json:"cacheTTLSeconds"`
	NegativeTTLSeconds            float64 `json:"negativeTTLSeconds"`
	NegativeTTLObserve            float64 `json:"negativeTTLObserveSeconds"`
	UpstreamRetryTimeoutMs        float64 `json:"upstreamRetryTimeoutMs"`
	UpstreamRetryTimeoutObserveMs float64 `json:"upstreamRetryTimeoutObservedMs"`

	// Service changes.
	ServiceChangesPerMinute float64 `json:"serviceChangesPerMinute"`
	// NotifyDelayMs is the extra latency before a cache learns about a service
	// change (informers are fast, so this is small).
	NotifyDelayMs float64 `json:"notifyDelayMs"`

	// Measurement.
	WarmupMinutes float64 `json:"warmupMinutes"`
}

// DefaultConfig returns a realistic 3000-container peak scenario.
func DefaultConfig() Config {
	return Config{
		Seed:               1,
		Namespaces:         60,
		ServicesPerNS:      40,
		Nodes:              200,
		ContainersPerNode:  15,
		Apps:               120,
		DurationMinutes:    60,
		PeakWindow:         [2]float64{0.75, 0.95},
		BaseQPSPerApp:      8,
		PeakQPSPerApp:      40,
		InClusterShare:     0.55,
		ExternalDomains:    600,
		DefaultNdots:       5,
		AppDeadlineSeconds: 5,
		ModifiedShare:      0.25,
		RootedShare:        0.08,
		IPv4OnlyShare:      0.15,
		SerializerShare:    0.10,
		TimeoutSeconds:     5,
		Attempts:           2,
		ClientPolicy:       "glibc",
		CoreDNSReplicas:    4,
		CoreDNSCapacity:    12000,

		LossRate:                      0.0003,
		PeakLossRate:                  0.0015,
		RTTMeanMs:                     0.6,
		RTTStdMs:                      0.25,
		CacheTTLSeconds:               30,
		NegativeTTLSeconds:            5,
		UpstreamRetryTimeoutMs:        400,
		UpstreamRetryTimeoutObserveMs: 200,
		ServiceChangesPerMinute:       6,
		NotifyDelayMs:                 15,
		WarmupMinutes:                 5,
	}
}

// Validate checks configuration sanity.
func (c Config) Validate() error {
	switch {
	case c.Namespaces < 1, c.ServicesPerNS < 1, c.Nodes < 1, c.ContainersPerNode < 1:
		return fmt.Errorf("sim: topology values must be positive")
	case c.Apps < 1, c.DurationMinutes <= 0:
		return fmt.Errorf("sim: apps and duration must be positive")
	case c.ExternalDomains < 1:
		return fmt.Errorf("sim: externalDomains must be positive")
	case c.CoreDNSReplicas < 1, c.CoreDNSCapacity <= 0:
		return fmt.Errorf("sim: coredns replicas and capacity must be positive")
	case c.DefaultNdots < 0 || c.DefaultNdots > 15:
		return fmt.Errorf("sim: ndots must be within 0..15")
	case c.InClusterShare < 0 || c.InClusterShare > 1:
		return fmt.Errorf("sim: inClusterShare must be within 0..1")
	case c.TimeoutSeconds <= 0, c.Attempts < 1:
		return fmt.Errorf("sim: timeout and attempts must be positive")
	}
	if _, ok := resolv.ParsePolicy(c.ClientPolicy); !ok {
		return fmt.Errorf("sim: unknown client policy %q", c.ClientPolicy)
	}
	return nil
}

// Topology describes generated cluster objects.

// Service is one cluster service.
type Service struct {
	Name      string // short name, unique within namespace
	Namespace string
	// FQDN is name.namespace.svc.cluster.local.
	FQDN string
	IPv4 string
	IPv6 string
	// HoldsEndpoints reports whether the service currently has endpoints.
	HoldsEndpoints bool
	// EndpointV4 changes whenever the endpoint set changes; caches use it to
	// detect staleness without polling.
	EndpointV4 string
	// Version increments on every endpoint change.
	Version uint64
	// Changes counts changes for diagnostics.
	Changes int
}

// ExternalDomain is an outside name the applications call.
type ExternalDomain struct {
	Name string
	IPv4 string
	IPv6 string
	TTL  uint32
	// Weight is the sampling weight (popular external services weigh more).
	Weight float64
	// HasAAAA reports whether an AAAA record exists.
	HasAAAA bool
}

// App is one application (a group of containers sharing a workload).
type App struct {
	Name      string
	Namespace string
	Node      int
	// Config is the container resolver configuration.
	Config resolv.Config
	// Style labels the configuration for the report.
	Style string
	// QueryRate is the calls per second at peak.
	QueryRate float64
	// InClusterShare is how much of its traffic is in-cluster.
	InClusterShare float64
	// RootedShare is how many of its in-cluster names are written with a
	// trailing dot.
	RootedShare float64
	// RawNdots is the ndots in the container's resolv.conf.
	RawNdots int
	// Services is the subset of cluster services this app calls.
	Services []*Service
	// Externals is the subset of external names it calls.
	Externals []*ExternalDomain
	// WorkloadShare is used to sample requests.
	Weight float64
}

// Dump renders topology stats for the report.
func (c Config) Summary() string {
	return fmt.Sprintf("%d namespaces x %d services, %d nodes x %d containers, %d apps, peak %.0f qps/app",
		c.Namespaces, c.ServicesPerNS, c.Nodes, c.ContainersPerNode, c.Apps, c.PeakQPSPerApp)
}

// frac returns the position of t within the run, in [0,1].
func (c Config) frac(t time.Duration) float64 {
	total := c.Total()
	if total <= 0 {
		return 1
	}
	f := float64(t) / float64(total)
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// InPeak reports whether t is inside the peak window. The window is given as
// fractions of the simulated run, so short runs still exercise the peak.
func (c Config) InPeak(t time.Duration) bool {
	f := c.frac(t)
	return f >= c.PeakWindow[0] && f <= c.PeakWindow[1]
}

// qpsAt returns the per-app resolution rate at time t: a slow ramp out of a
// quiet baseline, a bursty saturation plateau inside the peak window, and a
// sharp ramp down afterwards. This is the shape of an observed daily peak on
// a short simulated timeline.
func (c Config) qpsAt(t time.Duration, r *engine.Rand) float64 {
	f := c.frac(t)
	lo, hi := c.PeakWindow[0], c.PeakWindow[1]
	const ramp = 0.06
	var level float64
	switch {
	case f < lo-ramp || f > hi+ramp:
		level = 0
	case f < lo:
		level = (f - (lo - ramp)) / ramp
	case f <= hi:
		level = 1
	default:
		level = 1 - (f-hi)/ramp
	}
	if level < 0 {
		level = 0
	}
	// Smooth the ramp with a cosine so the load curve looks organic.
	level = 0.5 - 0.5*math.Cos(level*math.Pi)
	v := c.BaseQPSPerApp + (c.PeakQPSPerApp-c.BaseQPSPerApp)*level
	if c.InPeak(t) {
		// Bursty saturation: 1.3x-1.7x the smooth level.
		v *= 1.3 + 0.4*r.Float64()
	}
	return v
}

// lossAt returns the packet-loss rate for time t. Loss climbs with the peak,
// modeling congested node networking and kube-proxy/conntrack pressure.
func (c Config) lossAt(t time.Duration, r *engine.Rand) float64 {
	if c.InPeak(t) {
		return c.PeakLossRate * (0.65 + 0.7*r.Float64())
	}
	return c.LossRate * (0.5 + r.Float64())
}

// Warmup returns the warmup duration.
func (c Config) Warmup() time.Duration {
	return time.Duration(c.WarmupMinutes * float64(time.Minute))
}

// Total returns the configured duration.
func (c Config) Total() time.Duration {
	return time.Duration(c.DurationMinutes * float64(time.Minute))
}

// timeSeconds converts a fractional second count to a Duration.
func timeSeconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
