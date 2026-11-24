package livezone_test

// End-to-end conformance: the simulator's cluster zone is served over real
// UDP, and the system C resolver (libresolv) queries it through the same
// resolv.conf search list the simulator models. This checks the zone answers
// and the search-list expansion against an independent implementation.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"dnsim/internal/livezone"
	"dnsim/internal/oracle"
	"dnsim/internal/resolv"
	"dnsim/internal/sim"
)

func TestLiveZoneWithLibresolv(t *testing.T) {
	if !oracle.Available() {
		t.Skip("no C compiler available")
	}
	cfg := sim.DefaultConfig()
	cfg.Namespaces = 4
	cfg.ServicesPerNS = 6
	cfg.Apps = 4
	cfg.Nodes = 4
	cfg.ContainersPerNode = 2
	cluster := sim.BuildCluster(cfg, 7)

	zone, err := livezone.Start(cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer zone.Close()

	// Pick a service that has endpoints, written as a short name in its own
	// namespace and as an external name that does not exist.
	var svc *sim.Service
	for _, s := range cluster.Services {
		if s.HoldsEndpoints {
			svc = s
			break
		}
	}
	if svc == nil {
		t.Fatal("no service with endpoints")
	}
	search := []string{
		svc.Namespace + ".svc." + sim.ClusterDomain,
		"svc." + sim.ClusterDomain,
		sim.ClusterDomain,
	}

	// libresolv resolves the short name through the search list.
	zone.Reset()
	res, err := oracle.Run(oracle.Parameters{
		Port: zone.Port(), Name: svc.Name, QType: "A", Ndots: 5,
		Timeout: 1, Retry: 1, Search: search,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RC <= 0 {
		t.Fatalf("libresolv failed to resolve %s: %+v (queries %v)", svc.Name, res, zone.Queries())
	}
	qs := zone.Queries()
	if len(qs) == 0 || qs[0] != svc.FQDN+"/1" {
		t.Fatalf("first query should be the service FQDN for A, got %v", qs)
	}

	// An external name with ndots:5 walks the whole search list. The zone must
	// see every suffix and answer NXDOMAIN, and libresolv must report failure.
	zone.Reset()
	ext := "api.stripe.com"
	res, err = oracle.Run(oracle.Parameters{
		Port: zone.Port(), Name: ext, QType: "A", Ndots: 5,
		Timeout: 1, Retry: 1, Search: search,
	})
	if err != nil {
		t.Fatal(err)
	}
	// rc>0 also happens for a NOERROR/NODATA answer (62 bytes), which is what
	// a negative-caching zone returns; success requires an address, so check
	// that the resolver reports NXDOMAIN/no-data instead.
	if res.RC > 0 && res.HErrno == 0 {
		t.Fatalf("external name unexpectedly resolved: %+v", res)
	}
	qs = zone.Queries()
	got := strings.Join(qs, " ")
	// The trailing dots are normalized; compare on the rooted forms.
	wantStr := strings.Join([]string{
		ext + "." + search[0] + "./1",
		ext + "." + search[1] + "./1",
		ext + "." + search[2] + "./1",
		ext + "./1",
	}, " ")
	if got != wantStr {
		t.Fatalf("zone saw\n got: %s\nwant: %s", got, wantStr)
	}

	// The modeled resolver running against the same live zone must visit the
	// same candidate names in the same order as libresolv.
	zone.Reset()
	simSeq := modeledSequence(t, zone, ext, 5, search)
	libcNames := make([]string, len(qs))
	for i, q := range qs {
		libcNames[i] = strings.TrimSuffix(q, "/1")
	}
	if strings.Join(simSeq, " ") != strings.Join(libcNames, " ") {
		t.Fatalf("modeled resolver disagrees with the zone\n sim: %v\n libc: %v", simSeq, libcNames)
	}
}

// modeledSequence runs internal/resolv against the live zone over UDP.
func modeledSequence(t *testing.T, zone *livezone.Zone, name string, ndots int, search []string) []string {
	t.Helper()
	var out []string
	cfg := resolv.Config{
		Nameservers: []string{fmt.Sprintf("127.0.0.1:%d", zone.Port())},
		Ndots:       ndots,
		Timeout:     500 * time.Millisecond,
		Attempts:    1,
		Policy:      resolv.PolicyLibc,
	}
	for _, s := range search {
		cfg.Search = append(cfg.Search, resolv.Rooted(s))
	}
	// Query the live zone for real and record the names put on the wire.
	clock := time.Duration(0)
	env := resolv.Env{
		Now: func() time.Duration { return clock },
		Send: func(server, name string, qtype uint16, timeout time.Duration, done func(resolv.Reply)) {
			out = append(out, name)
			done(zone.Query(name, qtype))
			clock += time.Millisecond
		},
	}
	resolv.Start(env, cfg, name, "ip4", nil, func(resolv.Result) {})
	return out
}
