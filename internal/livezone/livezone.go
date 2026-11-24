// Package livezone serves the simulated cluster's authoritative zones over
// real UDP so an unmodified system resolver (libresolv, dig, Go's net
// package) can query the simulation. The conformance tests use it to check
// that the simulator's zone answers and search-list rules match a real client.
package livezone

import (
	"fmt"
	"net"
	"net/netip"
	"sync"

	"dnsim/internal/dnswire"
	"dnsim/internal/sim"
)

// Zone is a UDP DNS server backed by a simulated cluster.
type Zone struct {
	cluster *sim.Cluster
	conn    net.PacketConn

	mu      sync.Mutex
	queries []string // qname/qtype of every request served
	closed  bool
}

// Start begins serving on 127.0.0.1:0.
func Start(cluster *sim.Cluster) (*Zone, error) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	z := &Zone{cluster: cluster, conn: pc}
	go z.serve()
	return z, nil
}

// Port returns the UDP port.
func (z *Zone) Port() int {
	return z.conn.LocalAddr().(*net.UDPAddr).Port
}

// Close stops the server.
func (z *Zone) Close() {
	z.mu.Lock()
	z.closed = true
	z.mu.Unlock()
	z.conn.Close()
}

// Queries returns every question served so far, as "name/type".
func (z *Zone) Queries() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.queries...)
}

// Reset clears the query log.
func (z *Zone) Reset() {
	z.mu.Lock()
	z.queries = nil
	z.mu.Unlock()
}

func (z *Zone) serve() {
	buf := make([]byte, 4096)
	for {
		n, addr, err := z.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req, err := dnswire.Parse(buf[:n])
		if err != nil || len(req.Questions) != 1 {
			continue
		}
		q := req.Questions[0]
		z.mu.Lock()
		z.queries = append(z.queries, fmt.Sprintf("%s/%d", q.Name, q.Type))
		z.mu.Unlock()
		resp := z.answer(req, q)
		out, err := dnswire.Pack(resp)
		if err != nil {
			continue
		}
		if len(out) > 512 {
			// Truncate like a real server would; the conformance harness uses
			// UDP and small answers, so this is only a safety net.
			resp.Answers = nil
			resp.Header.Truncated = true
			out, _ = dnswire.Pack(resp)
		}
		z.conn.WriteTo(out, addr)
	}
}

func (z *Zone) answer(req *dnswire.Message, q dnswire.Question) *dnswire.Message {
	resp := &dnswire.Message{
		Header: dnswire.Header{
			ID:                 req.Header.ID,
			Response:           true,
			Authoritative:      true,
			RecursionDesired:   req.Header.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnswire.RCodeSuccess,
		},
		Questions: req.Questions,
	}
	switch q.Type {
	case dnswire.TypeA, dnswire.TypeAAAA:
	default:
		resp.Header.RCode = dnswire.RCodeNotImp
		return resp
	}
	ips, ttl, exists, noData := z.cluster.LookupName(q.Name, q.Type)
	switch {
	case !exists:
		resp.Header.RCode = dnswire.RCodeNXDomain
		resp.Authority = []dnswire.RR{soa(q.Name)}
		return resp
	case noData || len(ips) == 0:
		resp.Authority = []dnswire.RR{soa(q.Name)}
		return resp
	}
	for _, s := range ips {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		resp.Answers = append(resp.Answers, dnswire.RR{
			Name: q.Name, Type: q.Type, Class: dnswire.ClassINET, TTL: ttl, IP: ip,
		})
	}
	return resp
}

// soa returns a negative-caching SOA. Its minimum field is the negative TTL.
func soa(name string) dnswire.RR {
	return dnswire.RR{
		Name: sim.ClusterDomain + ".", Type: dnswire.TypeSOA, Class: dnswire.ClassINET, TTL: 30,
		SOA: &dnswire.SOA{
			MName: "ns.dns." + sim.ClusterDomain + ".", RName: "hostmaster." + sim.ClusterDomain + ".",
			Serial: 1, Refresh: 7200, Retry: 1800, Expire: 86400, Minimum: 5,
		},
	}
}
