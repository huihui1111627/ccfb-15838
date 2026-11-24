package resolv

import (
	"testing"
	"time"
)

// fakeNet answers from a callback and records each query.
type fakeNet struct {
	clock  *time.Duration
	seq    []string
	answer func(name string, qtype uint16) Reply
}

func (f *fakeNet) env() Env {
	return Env{
		Now: func() time.Duration { return *f.clock },
		Send: func(server, name string, qtype uint16, timeout time.Duration, done func(Reply)) {
			f.seq = append(f.seq, name)
			*f.clock += 2 * time.Millisecond
			done(f.answer(name, qtype))
		},
	}
}

func baseCfg() Config {
	return Config{
		Nameservers: []string{"10.96.0.10:53"},
		Search:      []string{"prod.svc.cluster.local.", "svc.cluster.local.", "cluster.local.", "corp.internal."},
		Ndots:       5,
		Timeout:     5 * time.Second,
		Attempts:    2,
		Policy:      PolicyLibc,
	}
}

func lookup(t *testing.T, cfg Config, name, network string, answer func(string, uint16) Reply) (Result, *fakeNet) {
	t.Helper()
	var clock time.Duration
	f := &fakeNet{clock: &clock, answer: answer}
	var res Result
	Start(f.env(), cfg, name, network, nil, func(r Result) { res = r })
	return res, f
}

func TestExternalNameCostsBothFamilies(t *testing.T) {
	cfg := baseCfg()
	res, net := lookup(t, cfg, "api.stripe.com", "ip", func(name string, qt uint16) Reply {
		if name == "api.stripe.com." {
			if qt == TypeA {
				return Reply{Status: StatusAnswer, IPs: []string{"203.0.113.5"}}
			}
			return Reply{Status: StatusNoData}
		}
		return Reply{Status: StatusNXDomain}
	})
	if res.Status != StatusAnswer || len(res.IPs) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Four search candidates, each asked for A and AAAA. The A walk reaches
	// the as-is name (9 queries); the AAAA walk continues through the as-is
	// name too (10 queries total).
	if len(net.seq) != 10 {
		t.Fatalf("expected 10 queries, got %d: %v", len(net.seq), net.seq)
	}
	if res.Queries != len(net.seq) {
		t.Fatalf("result.Queries=%d but %d on the wire", res.Queries, len(net.seq))
	}
	// The as-is name appears twice (once per family), after eight suffixed
	// queries.
	n := 0
	for _, q := range net.seq {
		if q == "api.stripe.com." {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("as-is name queried %d times, want 2: %v", n, net.seq)
	}
}

func TestRootedExternalIsOneQuery(t *testing.T) {
	cfg := baseCfg()
	res, net := lookup(t, cfg, "api.stripe.com.", "ip4", func(name string, qt uint16) Reply {
		return Reply{Status: StatusAnswer, IPs: []string{"203.0.113.5"}}
	})
	if res.Status != StatusAnswer || len(net.seq) != 1 {
		t.Fatalf("rooted lookup sent %d queries: %v", len(net.seq), net.seq)
	}
}

func TestNdotsTwoShortCircuitsExternal(t *testing.T) {
	cfg := baseCfg()
	cfg.Ndots = 2
	res, net := lookup(t, cfg, "api.stripe.com", "ip4", func(name string, qt uint16) Reply {
		if name == "api.stripe.com." {
			return Reply{Status: StatusAnswer, IPs: []string{"203.0.113.5"}}
		}
		return Reply{Status: StatusNXDomain}
	})
	if res.Status != StatusAnswer || len(net.seq) != 1 || net.seq[0] != "api.stripe.com." {
		t.Fatalf("ndots:2 should try the name first, got %v", net.seq)
	}
}

func TestShortInClusterNameStopsAtFirstHit(t *testing.T) {
	cfg := baseCfg()
	res, net := lookup(t, cfg, "redis", "ip4", func(name string, qt uint16) Reply {
		if name == "redis.prod.svc.cluster.local." {
			return Reply{Status: StatusAnswer, IPs: []string{"10.96.12.7"}}
		}
		return Reply{Status: StatusNXDomain}
	})
	if res.Status != StatusAnswer || len(net.seq) != 1 {
		t.Fatalf("expected one query, got %v", net.seq)
	}
	if res.WinningName != "redis.prod.svc.cluster.local." {
		t.Fatalf("winning name %q", res.WinningName)
	}
}

func TestNXDomainWalksAllSuffixes(t *testing.T) {
	cfg := baseCfg()
	cfg.Ndots = 2
	res, net := lookup(t, cfg, "nosuchhost", "ip4", func(string, uint16) Reply {
		return Reply{Status: StatusNXDomain}
	})
	// Four suffixes then the bare name.
	if len(net.seq) != 5 {
		t.Fatalf("expected 5 queries, got %d: %v", len(net.seq), net.seq)
	}
	if res.Status != StatusNXDomain {
		t.Fatalf("status %v", res.Status)
	}
}

func TestLibcStopsOnTimeoutButGoContinues(t *testing.T) {
	cfg := baseCfg()
	cfg.Ndots = 2
	timeout := func(string, uint16) Reply { return Reply{Status: StatusTimeout} }

	libc, libcNet := lookup(t, cfg, "api.stripe.com", "ip4", timeout)
	if libc.Status != StatusTimeout {
		t.Fatalf("libc status %v", libc.Status)
	}
	// Candidate 1 is retried twice, then the walk ends: 2 queries.
	if len(libcNet.seq) != cfg.Attempts {
		t.Fatalf("libc sent %d queries, want %d: %v", len(libcNet.seq), cfg.Attempts, libcNet.seq)
	}

	goCfg := cfg
	goCfg.Policy = PolicyGo
	_, goNet := lookup(t, goCfg, "api.stripe.com", "ip4", timeout)
	if len(goNet.seq) <= len(libcNet.seq) {
		t.Fatalf("go policy should keep searching after a timeout: %v", goNet.seq)
	}
}

func TestServfailRetriesThenMovesOn(t *testing.T) {
	cfg := baseCfg()
	cfg.Ndots = 5 // five candidates; four suffixes fail before the as-is name
	res, net := lookup(t, cfg, "api.stripe.com", "ip4", func(name string, qt uint16) Reply {
		if name == "api.stripe.com." {
			return Reply{Status: StatusAnswer, IPs: []string{"203.0.113.5"}}
		}
		return Reply{Status: StatusServFail}
	})
	seq := net.seq
	if res.Status != StatusAnswer {
		t.Fatalf("status %v", res.Status)
	}
	// The first four candidates fail twice each, then the root query answers.
	if len(seq) != 4*cfg.Attempts+1 {
		t.Fatalf("expected %d queries, got %d: %v", 4*cfg.Attempts+1, len(seq), seq)
	}
}

func TestSerialFamiliesHalvesConcurrencyNotCount(t *testing.T) {
	cfg := baseCfg()
	cfg.SerialFamilies = true
	_, net := lookup(t, cfg, "api.stripe.com", "ip", func(name string, qt uint16) Reply {
		if name == "api.stripe.com." && qt == TypeA {
			return Reply{Status: StatusAnswer, IPs: []string{"203.0.113.5"}}
		}
		return Reply{Status: StatusNXDomain}
	})
	seq := net.seq
	// Still 8 search queries; the A record is found at the root query and the
	// AAAA query for that name is issued first in serial mode.
	if len(seq) < 8 {
		t.Fatalf("too few queries: %v", seq)
	}
	if seq[len(seq)-1] != "api.stripe.com." {
		t.Fatalf("unexpected tail: %v", seq)
	}
}

func TestIPv4OnlySendsOneFamily(t *testing.T) {
	cfg := baseCfg()
	cfg.Families = FamiliesIPv4
	_, net := lookup(t, cfg, "api.github.com", "ip", func(string, uint16) Reply {
		return Reply{Status: StatusNXDomain}
	})
	// 5 candidates x 1 family.
	if len(net.seq) != 5 {
		t.Fatalf("expected 5 queries, got %d: %v", len(net.seq), net.seq)
	}
}
