package dnswire

import (
	"net/netip"
	"testing"
)

func TestPackParseRoundTrip(t *testing.T) {
	ip4 := netip.MustParseAddr("10.96.12.7")
	ip6 := netip.MustParseAddr("fd00:10:96::42")
	in := &Message{
		Header: Header{ID: 0x1234, Response: true, Authoritative: true,
			RecursionDesired: true, RecursionAvailable: true, RCode: RCodeSuccess},
		Questions: []Question{{Name: "redis.prod.svc.cluster.local.", Type: TypeA, Class: ClassINET}},
		Answers: []RR{
			{Name: "redis.prod.svc.cluster.local.", Type: TypeA, Class: ClassINET, TTL: 30, IP: ip4},
			{Name: "redis.prod.svc.cluster.local.", Type: TypeAAAA, Class: ClassINET, TTL: 30, IP: ip6},
			{Name: "alias.cluster.local.", Type: TypeCNAME, Class: ClassINET, TTL: 60, Target: "redis.prod.svc.cluster.local."},
		},
		Authority: []RR{{Name: "cluster.local.", Type: TypeSOA, Class: ClassINET, TTL: 30,
			SOA: &SOA{MName: "ns.dns.cluster.local.", RName: "hostmaster.cluster.local.",
				Serial: 1, Refresh: 7200, Retry: 1800, Expire: 86400, Minimum: 5}}},
	}
	buf, err := Pack(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Header != in.Header {
		t.Fatalf("header %+v != %+v", out.Header, in.Header)
	}
	if len(out.Questions) != 1 || out.Questions[0] != in.Questions[0] {
		t.Fatalf("questions %+v", out.Questions)
	}
	if len(out.Answers) != 3 || out.Answers[0].IP != ip4 || out.Answers[1].IP != ip6 {
		t.Fatalf("answers %+v", out.Answers)
	}
	if out.Answers[2].Target != "redis.prod.svc.cluster.local." {
		t.Fatalf("cname %q", out.Answers[2].Target)
	}
	if out.Authority[0].SOA.Minimum != 5 {
		t.Fatalf("soa %+v", out.Authority[0].SOA)
	}
}

func TestParseCompressedName(t *testing.T) {
	// Header + question a.b.c. + answer with pointer to the question name.
	buf := []byte{
		0x00, 0x01, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		1, 'a', 1, 'b', 1, 'c', 0, // a.b.c.
		0, 1, 0, 1, // type A class IN
		0xc0, 0x0c, // pointer to offset 12
		0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 10, 0, 0, 1,
	}
	m, err := Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if m.Questions[0].Name != "a.b.c." {
		t.Fatalf("question name %q", m.Questions[0].Name)
	}
	if m.Answers[0].Name != "a.b.c." {
		t.Fatalf("answer name %q", m.Answers[0].Name)
	}
	if m.Answers[0].IP.String() != "10.0.0.1" {
		t.Fatalf("ip %v", m.Answers[0].IP)
	}
}

func TestRejectMalformed(t *testing.T) {
	ptrLoop := make([]byte, 12)
	ptrLoop[4] = 0x00
	ptrLoop[5] = 0x01 // QDCOUNT 1: name is a pointer to itself
	ptrLoop = append(ptrLoop, 0xc0, 0x0c, 0, 1, 0, 1)
	cases := [][]byte{
		nil,
		{0, 1, 0, 0},
		ptrLoop,
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Fatalf("accepted malformed %v", c)
		}
	}
}
