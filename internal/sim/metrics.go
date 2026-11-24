package sim

import (
	"math"
	"time"
)

// histogram is a latency histogram in microseconds. Below 100 ms it uses 100µs
// buckets, so the median and the percentiles that matter for cached answers
// are accurate; above that it uses logarithmic buckets up to 60 s, which is
// enough to resolve the 5-second timeout plateau.
type histogram struct {
	fine   []uint64 // 0..fineMaxUS in fineStepUS buckets
	coarse []uint64 // log buckets above fineMaxUS
	count  uint64
	sum    float64 // microseconds
	max    float64
}

const (
	fineStepUS = 100.0
	fineBins   = 1000 // up to 100ms
	coarseBins = 30
)

const fineMaxUS = fineStepUS * fineBins

func newHistogram() *histogram {
	return &histogram{
		fine:   make([]uint64, fineBins),
		coarse: make([]uint64, coarseBins),
	}
}

func (h *histogram) addIndex(us float64) int {
	if us < fineMaxUS {
		i := int(us / fineStepUS)
		if i < 0 {
			i = 0
		}
		if i >= fineBins {
			i = fineBins - 1
		}
		h.fine[i]++
		return i
	}
	return -1
}

func coarseIndex(us float64) int {
	// Each coarse bucket covers a factor of (60s/100ms)^(1/coarseBins).
	ratio := math.Pow(60e6/fineMaxUS, 1/float64(coarseBins))
	i := int(math.Log(us/fineMaxUS) / math.Log(ratio))
	if i < 0 {
		i = 0
	}
	if i >= coarseBins {
		i = coarseBins - 1
	}
	return i
}

func coarseUpper(i int) float64 {
	ratio := math.Pow(60e6/fineMaxUS, 1/float64(coarseBins))
	return fineMaxUS * math.Pow(ratio, float64(i+1))
}

func (h *histogram) Add(d time.Duration) {
	us := float64(d) / float64(time.Microsecond)
	if us < 0 {
		us = 0
	}
	if h.addIndex(us) < 0 {
		h.coarse[coarseIndex(us)]++
	}
	h.count++
	h.sum += us
	if us > h.max {
		h.max = us
	}
}

// Percentile returns an approximate percentile in milliseconds. Fine buckets
// report their upper edge; coarse buckets report a geometric interpolation.
func (h *histogram) Percentile(p float64) float64 {
	if h.count == 0 {
		return 0
	}
	target := uint64(math.Ceil(p * float64(h.count)))
	var cum uint64
	for i, c := range h.fine {
		cum += c
		if cum >= target {
			return (float64(i) + 1) * fineStepUS / 1000
		}
	}
	for i, c := range h.coarse {
		cum += c
		if cum >= target {
			return coarseUpper(i) / 1000
		}
	}
	return h.max / 1000
}

func (h *histogram) MeanMS() float64 {
	if h.count == 0 {
		return 0
	}
	return h.sum / float64(h.count) / 1000
}

func (h *histogram) MaxMS() float64 { return h.max / 1000 }

func (h *histogram) Count() uint64 { return h.count }

// TimeBucket is one point of the load curve.
type TimeBucket struct {
	Start         time.Duration `json:"t"`
	AppLookups    int           `json:"appLookups"`
	ClientQueries int           `json:"clientQueries"`
	ServerQueries int           `json:"serverQueries"`
	CacheHits     int           `json:"cacheHits"`
	CacheMisses   int           `json:"cacheMisses"`
	Upstream      int           `json:"upstreamQueries"`
	Timeouts      int           `json:"timeouts"`
	QueryTimeouts int           `json:"queryTimeouts"`
	Wasted        int           `json:"wasted"`
	Lost          int           `json:"lost"`
	Stale         int           `json:"staleServes"`
}

// Metrics aggregates one run.
type Metrics struct {
	Buckets []TimeBucket `json:"buckets"`

	// Client-side.
	AppLookups     int `json:"appLookups"`
	ClientQueries  int `json:"clientQueries"`
	WastedQueries  int `json:"wastedQueries"`
	LookupTimeouts int `json:"lookupTimeouts"`
	SlowLookups    int `json:"slowLookups"`
	QueryTimeouts  int `json:"queryTimeouts"`
	LookupErrors   int `json:"lookupErrors"`
	Answers        int `json:"answers"`

	Latency *histogram `json:"-"`

	// Server-side.
	ServerQueries   int
	ServerDrops     int // queries silently dropped by overloaded replicas
	NetworkLost     int // packets lost in transit
	ServerLatency   *histogram
	ServerQueueWait *histogram

	// Cache.
	CacheEnabled     bool
	CacheLookups     int
	CacheHits        int
	CacheNegHits     int
	CacheMisses      int
	UpstreamCalls    int
	UpstreamRetries  int
	UpstreamTimeouts int
	UpstreamFailures int
	ClientRetries    int
	Invalidations    int
	StaleServes      int
	StaleWindowSum   float64 // ms
	StaleWindowMax   float64
	StaleWindowCount int
	StaleResidualMax float64

	// Service changes.
	ServiceChanges int

	// Per-app and per-name attribution.
	Apps map[string]*AppMetrics
}

// AppMetrics is per-application instrumentation.
type AppMetrics struct {
	Lookups  int
	Queries  int
	Wasted   int
	Timeouts int
	Slow     int
	Errors   int
	Answers  int
	Latency  *histogram
	// QueryLatency is the per-DNS-query latency observed by the app.
	QueryLatency *histogram
	// PerName counts queries per intended logical name.
	PerName map[string]*NameMetrics
	// CacheHits/Misses when the node cache is used.
	CacheHits   int
	CacheMisses int
}

// MarshalJSON is not used; app metrics are converted to AppReport.

// Histogram exposes percentile queries for serialization.
type LatencySummary struct {
	P50MS  float64 `json:"p50Ms"`
	P95MS  float64 `json:"p95Ms"`
	P99MS  float64 `json:"p99Ms"`
	MaxMS  float64 `json:"maxMs"`
	MeanMS float64 `json:"meanMs"`
}

// Summary returns the latency summary of the histogram.
func (h *histogram) Summary() LatencySummary {
	return LatencySummary{
		P50MS: h.Percentile(0.50), P95MS: h.Percentile(0.95),
		P99MS: h.Percentile(0.99), MaxMS: h.MaxMS(), MeanMS: h.MeanMS(),
	}
}

// NameMetrics is per-name instrumentation.
type NameMetrics struct {
	Queries  int
	Wasted   int
	Lookups  int
	Timeouts int
	Slow     int
}

// NewMetrics creates an empty metrics set.
func NewMetrics(cacheEnabled bool, buckets int) *Metrics {
	b := make([]TimeBucket, buckets)
	for i := range b {
		b[i].Start = time.Duration(i) * time.Second
	}
	return &Metrics{
		Buckets:         b,
		Latency:         newHistogram(),
		ServerLatency:   newHistogram(),
		ServerQueueWait: newHistogram(),
		CacheEnabled:    cacheEnabled,
		Apps:            make(map[string]*AppMetrics),
	}
}

func (m *Metrics) app(name string) *AppMetrics {
	a := m.Apps[name]
	if a == nil {
		a = &AppMetrics{Latency: newHistogram(), QueryLatency: newHistogram(), PerName: make(map[string]*NameMetrics)}
		m.Apps[name] = a
	}
	return a
}

func (m *Metrics) name(appName, name string) *NameMetrics {
	a := m.app(appName)
	n := a.PerName[name]
	if n == nil {
		n = &NameMetrics{}
		a.PerName[name] = n
	}
	return n
}

func (m *Metrics) bucket(t time.Duration) *TimeBucket {
	i := int(t / time.Second)
	if i < 0 {
		i = 0
	}
	if i >= len(m.Buckets) {
		i = len(m.Buckets) - 1
	}
	return &m.Buckets[i]
}
