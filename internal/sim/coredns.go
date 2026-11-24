package sim

import (
	"fmt"
	"time"

	"dnsim/internal/resolv"
)

// CoreDNS models the cluster DNS service: a fixed number of replicas, each
// with per-second query capacity and a token bucket that drops packets at
// overload, and a shared service-time model that queues requests. Replicas
// answer from the simulated zones; external names still consume capacity,
// which is exactly what client-side amplification exhausts.
type CoreDNS struct {
	sim      *Sim
	replicas []*replica
	rr       int
}

type replica struct {
	id          int
	busyUntil   time.Duration
	windowStart time.Duration
	accepted    int
	capacity    int
	dropped     int
	served      int
}

// CoreDNSReply is the answer CoreDNS sends.
type CoreDNSReply struct {
	RCode  int
	IPs    []string
	TTL    uint32
	NoData bool
	Server int
}

func newCoreDNS(s *Sim) *CoreDNS {
	c := &CoreDNS{sim: s}
	for i := 0; i < s.cfg.CoreDNSReplicas; i++ {
		c.replicas = append(c.replicas, &replica{id: i, capacity: int(s.cfg.CoreDNSCapacity)})
	}
	return c
}

// begin reserves capacity on the next replica. It returns the queueing delay,
// whether the query was dropped by the token bucket, and the replica.
func (c *CoreDNS) begin(now time.Duration) (*replica, time.Duration, bool) {
	r := c.replicas[c.rr%len(c.replicas)]
	c.rr++
	if now-r.windowStart >= time.Second {
		r.windowStart = now
		r.accepted = 0
	}
	r.accepted++
	// A replica is provisioned to its rated capacity; below capacity nothing
	// is dropped. Above it, the UDP/conntrack path and the kernel receive
	// buffer overflow: the further over capacity, the more is dropped, up to
	// 70% in a severe storm.
	if r.accepted > r.capacity {
		over := float64(r.accepted-r.capacity) / float64(r.capacity)
		drop := 0.55 * clamp01(over*2)
		if c.sim.rnd.Chance(drop) {
			r.dropped++
			return r, 0, true
		}
	}
	ready := now
	if r.busyUntil > ready {
		ready = r.busyUntil
	}
	// Only a positive backlog is queueing delay; a replica that finished its
	// last query before now has no queue to sit in.
	queueDelay := ready - now
	if queueDelay < 0 {
		queueDelay = 0
	}
	// A replica serves its queries concurrently at a fixed aggregate rate:
	// one query per 1/capacity seconds. Below capacity the queue stays empty.
	// Above it a real replica does not queue without bound: its UDP socket
	// buffer and upstream concurrency limit overflow and requests are
	// dropped, which the client sees as a timeout. We cap the queue at
	// maxQueueDelay, so the observed latency stays near the 5s client timeout
	// instead of growing to tens of seconds.
	svc := time.Second / time.Duration(r.capacity)
	const maxQueueDelay = 40 * time.Millisecond
	if queueDelay > maxQueueDelay {
		r.dropped++
		return r, 0, true
	}
	r.busyUntil = ready + svc
	r.served++
	return r, queueDelay, false
}

var debugDNS = false

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// answerAuthoritatively looks a rooted name up in the simulated zones.
func (s *Sim) answerAuthoritatively(name string, qtype uint16) CoreDNSReply {
	ips, ttl, exists, noData := s.cluster.LookupName(name, qtype)
	switch {
	case !exists:
		return CoreDNSReply{RCode: 3}
	case noData || len(ips) == 0:
		return CoreDNSReply{RCode: 0, NoData: true, TTL: ttl}
	default:
		return CoreDNSReply{RCode: 0, IPs: ips, TTL: ttl}
	}
}

// lossNow returns the current packet-loss rate with jitter.
func (s *Sim) lossNow() float64 {
	return s.cfg.lossAt(s.eng.Now(), s.netRnd)
}

func (c *CoreDNS) String() string {
	return fmt.Sprintf("CoreDNS: %d replicas, %d qps each", len(c.replicas), c.replicas[0].capacity)
}

// sendUpstream models one query from a client (an application resolver or a
// node cache) to the CoreDNS service:
//
//	request loss -> one-way delay -> replica queue + service -> response loss
//
// A lost request or response costs the caller its full timeout, because a UDP
// stub resolver has no way to tell loss from slowness.
func (s *Sim) sendUpstream(name string, qtype uint16, timeout time.Duration, done func(CoreDNSReply, resolv.Status)) time.Duration {
	now := s.eng.Now()
	rtt := s.sampleRTT()
	oneWay := rtt / 2
	// Network conditions are sampled when the packet is sent, and the response
	// inherits them, so a request sent just before the peak is not "answered"
	// with peak-time loss applied later.
	loss := s.lossNow()
	respLost := s.netRnd.Chance(loss)
	if s.netRnd.Chance(loss) {
		s.countLost()
		s.eng.At(timeout, func() { done(CoreDNSReply{}, resolv.StatusTimeout) })
		return timeout
	}
	arrival := now + oneWay
	s.eng.AtAbs(arrival, func() {
		rep, queueDelay, dropped := s.dns.begin(arrival)
		s.countServerQuery(arrival, queueDelay)
		if dropped {
			// The request reached the cluster DNS service but an overloaded
			// replica discarded it; the client cannot distinguish this from
			// network loss, so it waits out its timeout.
			s.metrics.ServerDrops++
			s.eng.At(timeout, func() { done(CoreDNSReply{}, resolv.StatusTimeout) })
			return
		}
		reply := s.answerAuthoritatively(name, qtype)
		reply.Server = rep.id
		svc := time.Duration((120 + 200*s.netRnd.IntN(120))) * time.Microsecond
		s.metrics.ServerLatency.Add(queueDelay + svc + oneWay + oneWay)
		finish := arrival + queueDelay + svc + oneWay
		// A response that would arrive after the caller's deadline is
		// indistinguishable from a loss, and the client has already given up,
		// so it is accounted as a timeout at the deadline.
		if respLost || finish-now > timeout {
			s.countLost()
			s.eng.At(timeout, func() { done(CoreDNSReply{}, resolv.StatusTimeout) })
			return
		}
		if finish-now > timeout {
			s.eng.At(timeout, func() { done(CoreDNSReply{}, resolv.StatusTimeout) })
			return
		}
		s.eng.AtAbs(finish, func() {
			st := resolv.StatusAnswer
			switch {
			case reply.RCode == 3:
				st = resolv.StatusNXDomain
			case reply.RCode != 0:
				st = resolv.StatusServFail
			case reply.NoData || len(reply.IPs) == 0:
				st = resolv.StatusNoData
			}
			done(reply, st)
		})
	})
	return timeout
}

func (s *Sim) countLost() {
	s.metrics.NetworkLost++
	s.metrics.bucket(s.eng.Now()).Lost++
}

func (s *Sim) countServerQuery(at time.Duration, queue time.Duration) {
	s.metrics.ServerQueries++
	s.metrics.bucket(at).ServerQueries++
	s.metrics.ServerQueueWait.Add(queue)
}

func (s *Sim) sampleRTT() time.Duration {
	if s.cfg.RTTMeanMs <= 0 {
		return 500 * time.Microsecond
	}
	mean := time.Duration(s.cfg.RTTMeanMs * float64(time.Millisecond))
	std := time.Duration(s.cfg.RTTStdMs * float64(time.Millisecond))
	return s.netRnd.NormalDuration(mean, std, 100*time.Microsecond, 20*time.Millisecond)
}
