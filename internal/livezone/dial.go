package livezone

import (
	"net"
	"time"

	"dnsim/internal/dnswire"
	"dnsim/internal/resolv"
)

// Query sends one question to the zone over UDP and reports the result in the
// resolver's vocabulary. It is exported for conformance harnesses; it is a
// blocking convenience wrapper around the simulated zone.
func (z *Zone) Query(name string, qtype uint16) resolv.Reply {
	out := make(chan resolv.Reply, 1)
	go func() { z.queryAsync(name, qtype, func(r resolv.Reply) { out <- r }) }()
	select {
	case r := <-out:
		return r
	case <-time.After(2 * time.Second):
		return resolv.Reply{Status: resolv.StatusTimeout}
	}
}

func (z *Zone) queryAsync(name string, qtype uint16, done func(resolv.Reply)) {
	conn, err := net.Dial("udp", z.conn.LocalAddr().String())
	if err != nil {
		done(resolv.Reply{Status: resolv.StatusServFail})
		return
	}
	defer conn.Close()
	req := &dnswire.Message{Questions: []dnswire.Question{{Name: name, Type: qtype, Class: dnswire.ClassINET}},
		Header: dnswire.Header{ID: 0x4242, RecursionDesired: true}}
	buf, err := dnswire.Pack(req)
	if err != nil {
		done(resolv.Reply{Status: resolv.StatusServFail})
		return
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write(buf); err != nil {
		done(resolv.Reply{Status: resolv.StatusTimeout})
		return
	}
	respBuf := make([]byte, 1500)
	n, err := conn.Read(respBuf)
	if err != nil {
		done(resolv.Reply{Status: resolv.StatusTimeout})
		return
	}
	resp, err := dnswire.Parse(respBuf[:n])
	if err != nil {
		done(resolv.Reply{Status: resolv.StatusServFail})
		return
	}
	switch resp.Header.RCode {
	case dnswire.RCodeNXDomain:
		done(resolv.Reply{Status: resolv.StatusNXDomain})
	case dnswire.RCodeSuccess:
		var ips []string
		for _, rr := range resp.Answers {
			if rr.Type == qtype {
				ips = append(ips, rr.IP.String())
			}
		}
		if len(ips) == 0 {
			done(resolv.Reply{Status: resolv.StatusNoData})
			return
		}
		done(resolv.Reply{Status: resolv.StatusAnswer, IPs: ips})
	default:
		done(resolv.Reply{Status: resolv.StatusServFail})
	}
}
