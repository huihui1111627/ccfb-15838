package report

import (
	"encoding/json"
	"strings"
	"testing"

	"dnsim/internal/sim"
)

func smallRun(t *testing.T) *Report {
	t.Helper()
	cfg := sim.DefaultConfig()
	cfg.DurationMinutes = 4
	cfg.Apps = 6
	cfg.Namespaces = 3
	cfg.ServicesPerNS = 5
	cfg.Nodes = 4
	cfg.ContainersPerNode = 2
	var results []*sim.Result
	for _, p := range sim.Profiles(cfg) {
		res, err := sim.RunProfile(cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, res)
	}
	return BuildAll(cfg, results)
}

func TestReportJSONShape(t *testing.T) {
	rep := smallRun(t)
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"config", "cluster", "profiles", "comparison", "suggestions"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("missing %q in report", k)
		}
	}
	cmp := doc["comparison"].([]any)
	if len(cmp) != 4 {
		t.Fatalf("expected 4 comparison rows, got %d", len(cmp))
	}
	first := cmp[0].(map[string]any)
	for _, k := range []string{"serverQueries", "latencyP99Ms", "fiveSecondStalls", "staleWindowMaxMs", "cacheHitRatio"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("comparison row missing %q: %v", k, first)
		}
	}
	if a := first["serverQueries"].(float64); a <= 0 {
		t.Fatalf("serverQueries should be positive")
	}
}

func TestHTMLSelfContained(t *testing.T) {
	rep := smallRun(t)
	doc, err := RenderHTML(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "{{DATA}}") || strings.Contains(doc, "{{TITLE}}") {
		t.Fatal("template placeholders not substituted")
	}
	if !strings.Contains(doc, "const DATA = {") {
		t.Fatal("data payload missing")
	}
	if strings.Contains(doc, "<script src=") || strings.Contains(doc, "<link rel=\"stylesheet\"") {
		t.Fatal("dashboard must be self-contained")
	}
}

func TestSuggestionsRanked(t *testing.T) {
	rep := smallRun(t)
	if len(rep.Suggestions) == 0 {
		t.Fatal("no suggestions")
	}
	for i := 1; i < len(rep.Suggestions); i++ {
		if rep.Suggestions[i].CurrentAvgQueries > rep.Suggestions[i-1].CurrentAvgQueries {
			t.Fatal("suggestions not sorted by current cost")
		}
	}
	if len(rep.Suggestions[0].Advice) == 0 {
		t.Fatal("top suggestion has no advice")
	}
}
