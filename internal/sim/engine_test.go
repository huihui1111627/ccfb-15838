package sim

import (
	"testing"
	"time"

	"dnsim/internal/engine"
)

// TestDeterminism verifies the same seed reproduces identical metrics, which
// is what makes the reported numbers reviewable.
func TestDeterminism(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DurationMinutes = 2
	cfg.Apps = 4
	cfg.Namespaces = 3
	cfg.ServicesPerNS = 4
	cfg.Nodes = 3
	cfg.ContainersPerNode = 2
	run := func() *Metrics {
		m, _, err := Run(cfg, Profiles(cfg)[1])
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := run(), run()
	if a.ClientQueries != b.ClientQueries || a.ServerQueries != b.ServerQueries ||
		a.CacheHits != b.CacheHits || a.WastedQueries != b.WastedQueries {
		t.Fatalf("non-deterministic run: %+v vs %+v", a, b)
	}
	if a.Latency.Percentile(0.99) != b.Latency.Percentile(0.99) {
		t.Fatal("latency percentile differs between identical runs")
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	bad := cfg
	bad.Nodes = 0
	if bad.Validate() == nil {
		t.Fatal("zero nodes accepted")
	}
	bad = cfg
	bad.ClientPolicy = "bogus"
	if bad.Validate() == nil {
		t.Fatal("unknown client policy accepted")
	}
	bad = cfg
	bad.InClusterShare = 2
	if bad.Validate() == nil {
		t.Fatal("out-of-range share accepted")
	}
}

func TestLoadCurveShape(t *testing.T) {
	cfg := DefaultConfig()
	rnd := engine.NewRand(1, 1)
	if q := cfg.qpsAt(0, rnd); q > cfg.BaseQPSPerApp*1.5 {
		t.Fatalf("start of run should be near baseline, got %.1f", q)
	}
	peakT := time.Duration((cfg.PeakWindow[0] + cfg.PeakWindow[1]) / 2 * float64(cfg.Total()))
	if !cfg.InPeak(peakT) {
		t.Fatalf("%v should be inside the peak", peakT)
	}
	if q := cfg.qpsAt(peakT, rnd); q < cfg.PeakQPSPerApp {
		t.Fatalf("peak qps %.1f below configured %.1f", q, cfg.PeakQPSPerApp)
	}
	if cfg.InPeak(cfg.Total()) {
		t.Fatal("end of run should be outside the peak")
	}
}
