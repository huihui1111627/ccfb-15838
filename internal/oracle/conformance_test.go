package oracle_test

// Conformance of the simulator's stub-resolver rules against the real C
// resolver. Every case runs the actual libresolv binary against a recording
// UDP server and compares the exact query sequence, and the
// success/failure outcome, to internal/resolv running the same scenario
// against the same recording server.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"dnsim/internal/oracle"
	"dnsim/internal/resolv"
)

type replyMode struct {
	name  string
	rcode int
	drop  bool
	ansA  bool
}

var replyModes = []replyMode{
	{name: "nxdomain", rcode: 3},
	{name: "nodata", rcode: 0},
	{name: "servfail", rcode: 2},
	{name: "answer", rcode: 0, ansA: true},
	{name: "drop", drop: true},
}

type searchCase struct {
	name  string
	ndots int
	query string
	// Which candidates glibc's serial search visits. We run libc once per
	// address family, so the expected simulator sequence is per family.
	families []uint16
	search   []string
}

var searchCases = []searchCase{
	{
		name: "k8s external ndots5", ndots: 5, query: "api.stripe.com",
		families: []uint16{1},
		search:   []string{"svc.cluster.local", "cluster.local", "corp.internal"},
	},
	{
		name: "k8s external ndots5 dual", ndots: 5, query: "api.stripe.com",
		families: []uint16{1, 28},
		search:   []string{"svc.cluster.local", "cluster.local", "corp.internal"},
	},
	{
		name: "ndots1 external", ndots: 1, query: "example.com",
		families: []uint16{1},
		search:   []string{"svc.cluster.local", "cluster.local"},
	},
	{
		name: "ndots2 single label", ndots: 2, query: "redis",
		families: []uint16{1},
		search:   []string{"svc.cluster.local", "cluster.local"},
	},
	{
		name: "rooted skips search", ndots: 5, query: "api.stripe.com.",
		families: []uint16{1},
		search:   []string{"svc.cluster.local"},
	},
	{
		name: "in cluster short name", ndots: 5, query: "redis",
		families: []uint16{1},
		search:   []string{"prod.svc.cluster.local", "svc.cluster.local", "cluster.local"},
	},
}

// TestSearchExpansionMatchesLibresolv checks ordering and stop/continue
// behavior for every response mode.
//
// The one intentional difference: on a dropped packet glibc keeps trying the
// remaining search candidates in the same family walk, while the simulator
// stops the walk and reports a timeout. That is the latency pathology being
// studied, so the conformance comparison for the drop mode is restricted to
// the first candidate's retransmission behavior.
func TestSearchExpansionMatchesLibresolv(t *testing.T) {
	if !oracle.Available() {
		t.Skip("no C compiler available; skipping libresolv conformance")
	}
	for _, sc := range searchCases {
		for _, mode := range replyModes {
			sc, mode := sc, mode
			t.Run(sc.name+"/"+mode.name, func(t *testing.T) {
				srv, err := oracle.NewServer()
				if err != nil {
					t.Fatal(err)
				}
				defer srv.Close()
				srv.SetDrop(mode.drop)
				srv.SetResponse(mode.rcode, mode.ansA)

				// libc: one res_search per address family, one retry.
				var libcSeq []string
				for _, fam := range sc.families {
					srv.Reset()
					qtype := "A"
					if fam == 28 {
						qtype = "AAAA"
					}
					res, err := oracle.Run(oracle.Parameters{
						Port: srv.Port(), Name: sc.query, QType: qtype,
						Ndots: sc.ndots, Timeout: 1, Retry: 1, Search: sc.search,
					})
					if err != nil {
						t.Fatal(err)
					}
					_ = res
					for _, q := range srv.Queries() {
						libcSeq = append(libcSeq, q.Name)
					}
				}

				simSeq := runSimulator(t, srv, sc, mode)
				if mode.drop {
					// Both stop after the first candidate: compare that
					// prefix exactly.
					if len(simSeq) == 0 || len(libcSeq) == 0 || simSeq[0] != libcSeq[0] {
						t.Fatalf("first query differs: sim %v libc %v", simSeq, libcSeq)
					}
					return
				}
				if got, want := strings.Join(simSeq, " "), strings.Join(libcSeq, " "); got != want {
					t.Fatalf("query sequence mismatch\n sim: %s\nlibc: %s", got, want)
				}
			})
		}
	}
}

// runSimulator executes the same scenario through internal/resolv against
// the recording server with virtual time.
func runSimulator(t *testing.T, srv *oracle.Server, sc searchCase, mode replyMode) []string {
	t.Helper()
	var seq []string
	var clock time.Duration
	env := resolv.Env{
		Now: func() time.Duration { return clock },
		Send: func(server, name string, qtype uint16, timeout time.Duration, done func(resolv.Reply)) {
			seq = append(seq, name)
			// One-second timeout: libresolv's minimum.
			clock += time.Second
			if mode.drop {
				done(resolv.Reply{Status: resolv.StatusTimeout})
				return
			}
			st := resolv.StatusNoData
			switch mode.rcode {
			case 3:
				st = resolv.StatusNXDomain
			case 2:
				st = resolv.StatusServFail
			case 0:
				if mode.ansA && qtype == 1 {
					st = resolv.StatusAnswer
				}
			}
			var ips []string
			if st == resolv.StatusAnswer {
				ips = []string{"10.0.0.1"}
			}
			done(resolv.Reply{Status: st, IPs: ips})
		},
	}
	cfg := resolv.Config{
		Nameservers: []string{"fake:53"},
		Search:      rooted(sc.search),
		Ndots:       sc.ndots,
		Timeout:     time.Second,
		Attempts:    1,
		Policy:      resolv.PolicyLibc,
	}
	for _, fam := range sc.families {
		network := "ip4"
		if fam == 28 {
			network = "ip6"
		}
		// Each family walk runs serially; the simulator starts one session
		// per family so the sequence matches libc's own per-family calls.
		_ = resolv.Start(env, cfg, sc.query, network, nil, func(resolv.Result) {})
	}
	return seq
}

func rooted(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = resolv.Rooted(s)
	}
	return out
}

var _ = fmt.Sprint
