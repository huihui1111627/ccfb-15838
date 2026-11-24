// Package dnswire implements the small subset of the DNS wire format that the
// simulator needs. It is used for the conformance bridge (so a real resolver
// such as libresolv can query the simulated cluster) and for round-trip tests.
//
// Only standard-class INET queries for A, AAAA, CNAME, NS and SOA records are
// supported. Message compression is decoded on input and never produced on
// output, which keeps the encoder simple and is legal DNS.
package dnswire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Record types.
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypeAAAA  uint16 = 28
	TypeOPT   uint16 = 41
)

// ClassINET is the Internet class.
const ClassINET uint16 = 1

// Response codes.
const (
	RCodeSuccess  = 0
	RCodeFormErr  = 1
	RCodeServFail = 2
	RCodeNXDomain = 3
	RCodeNotImp   = 4
	RCodeRefused  = 5
)

// Header is the DNS message header.
type Header struct {
	ID                 uint16
	Response           bool
	Opcode             int
	Authoritative      bool
	Truncated          bool
	RecursionDesired   bool
	RecursionAvailable bool
	RCode              int
}

// Question is a DNS question.
type Question struct {
	Name  string // presentation form, always rooted (trailing dot)
	Type  uint16
	Class uint16
}

// SOA is the content of an SOA record.
type SOA struct {
	MName   string
	RName   string
	Serial  uint32
	Refresh uint32
	Retry   uint32
	Expire  uint32
	Minimum uint32
}

// RR is a resource record. Only one of A/AAAA/Target/SOA is meaningful.
type RR struct {
	Name   string
	Type   uint16
	Class  uint16
	TTL    uint32
	IP     netip.Addr
	Target string
	SOA    *SOA
}

// Message is a DNS message.
type Message struct {
	Header     Header
	Questions  []Question
	Answers    []RR
	Authority  []RR
	Additional []RR
}

// ErrMalformed is returned for unparsable messages.
var ErrMalformed = errors.New("dnswire: malformed message")

// PackQuestionName encodes a presentation name.
func PackQuestionName(dst []byte, name string) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(dst, 0), nil
	}
	if len(name) > 253 {
		return nil, fmt.Errorf("dnswire: name too long: %d", len(name))
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return nil, fmt.Errorf("dnswire: empty label in %q", name)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("dnswire: label too long in %q", name)
		}
		dst = append(dst, byte(len(label)))
		dst = append(dst, label...)
	}
	return append(dst, 0), nil
}

// Pack serializes a message.
func Pack(m *Message) ([]byte, error) {
	buf := make([]byte, 0, 512)
	var h [12]byte
	binary.BigEndian.PutUint16(h[0:2], m.Header.ID)
	if m.Header.Response {
		h[2] |= 0x80
	}
	h[2] |= byte(m.Header.Opcode&0xf) << 3
	if m.Header.Authoritative {
		h[2] |= 0x04
	}
	if m.Header.Truncated {
		h[2] |= 0x02
	}
	if m.Header.RecursionDesired {
		h[2] |= 0x01
	}
	if m.Header.RecursionAvailable {
		h[3] |= 0x80
	}
	h[3] |= byte(m.Header.RCode & 0xf)
	binary.BigEndian.PutUint16(h[4:6], uint16(len(m.Questions)))
	binary.BigEndian.PutUint16(h[6:8], uint16(len(m.Answers)))
	binary.BigEndian.PutUint16(h[8:10], uint16(len(m.Authority)))
	binary.BigEndian.PutUint16(h[10:12], uint16(len(m.Additional)))
	buf = append(buf, h[:]...)

	var err error
	for _, q := range m.Questions {
		buf, err = PackQuestionName(buf, q.Name)
		if err != nil {
			return nil, err
		}
		var t [4]byte
		binary.BigEndian.PutUint16(t[0:2], q.Type)
		binary.BigEndian.PutUint16(t[2:4], q.Class)
		buf = append(buf, t[:]...)
	}
	sections := [][]RR{m.Answers, m.Authority, m.Additional}
	for _, sec := range sections {
		for _, rr := range sec {
			buf, err = packRR(buf, rr)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(buf) > 65535 {
		return nil, errors.New("dnswire: message too large")
	}
	return buf, nil
}

func packRR(buf []byte, rr RR) ([]byte, error) {
	var err error
	buf, err = PackQuestionName(buf, rr.Name)
	if err != nil {
		return nil, err
	}
	class := rr.Class
	if class == 0 {
		class = ClassINET
	}
	var fixed [10]byte
	binary.BigEndian.PutUint16(fixed[0:2], rr.Type)
	binary.BigEndian.PutUint16(fixed[2:4], class)
	binary.BigEndian.PutUint32(fixed[4:8], rr.TTL)
	buf = append(buf, fixed[:8]...)
	rdataStart := len(buf)
	buf = append(buf, 0, 0) // length placeholder
	switch rr.Type {
	case TypeA:
		if !rr.IP.Is4() {
			return nil, fmt.Errorf("dnswire: A record with %v", rr.IP)
		}
		a := rr.IP.As4()
		buf = append(buf, a[:]...)
	case TypeAAAA:
		if !rr.IP.Is6() || rr.IP.Is4In6() {
			return nil, fmt.Errorf("dnswire: AAAA record with %v", rr.IP)
		}
		a := rr.IP.As16()
		buf = append(buf, a[:]...)
	case TypeCNAME, TypeNS:
		buf, err = PackQuestionName(buf, rr.Target)
		if err != nil {
			return nil, err
		}
	case TypeSOA:
		if rr.SOA == nil {
			return nil, errors.New("dnswire: SOA record without data")
		}
		buf, err = PackQuestionName(buf, rr.SOA.MName)
		if err != nil {
			return nil, err
		}
		buf, err = PackQuestionName(buf, rr.SOA.RName)
		if err != nil {
			return nil, err
		}
		var nums [20]byte
		binary.BigEndian.PutUint32(nums[0:4], rr.SOA.Serial)
		binary.BigEndian.PutUint32(nums[4:8], rr.SOA.Refresh)
		binary.BigEndian.PutUint32(nums[8:12], rr.SOA.Retry)
		binary.BigEndian.PutUint32(nums[12:16], rr.SOA.Expire)
		binary.BigEndian.PutUint32(nums[16:20], rr.SOA.Minimum)
		buf = append(buf, nums[:]...)
	case TypeOPT:
		// Empty OPT, only for parsing convenience.
	default:
		return nil, fmt.Errorf("dnswire: unsupported record type %d", rr.Type)
	}
	rdlen := len(buf) - rdataStart - 2
	if rdlen > 65535 {
		return nil, errors.New("dnswire: rdata too long")
	}
	binary.BigEndian.PutUint16(buf[rdataStart:rdataStart+2], uint16(rdlen))
	return buf, nil
}

// Parse decodes a message.
func Parse(data []byte) (*Message, error) {
	if len(data) < 12 {
		return nil, ErrMalformed
	}
	m := &Message{}
	flags := binary.BigEndian.Uint16(data[2:4])
	m.Header = Header{
		ID:                 binary.BigEndian.Uint16(data[0:2]),
		Response:           flags&0x8000 != 0,
		Opcode:             int(flags>>11) & 0xf,
		Authoritative:      flags&0x0400 != 0,
		Truncated:          flags&0x0200 != 0,
		RecursionDesired:   flags&0x0100 != 0,
		RecursionAvailable: flags&0x0080 != 0,
		RCode:              int(flags & 0xf),
	}
	qd := int(binary.BigEndian.Uint16(data[4:6]))
	an := int(binary.BigEndian.Uint16(data[6:8]))
	ns := int(binary.BigEndian.Uint16(data[8:10]))
	ar := int(binary.BigEndian.Uint16(data[10:12]))
	off := 12
	var err error
	for i := 0; i < qd; i++ {
		var q Question
		q.Name, off, err = unpackName(data, off)
		if err != nil || off+4 > len(data) {
			return nil, ErrMalformed
		}
		q.Type = binary.BigEndian.Uint16(data[off : off+2])
		q.Class = binary.BigEndian.Uint16(data[off+2 : off+4])
		off += 4
		m.Questions = append(m.Questions, q)
	}
	for i := 0; i < an; i++ {
		var rr RR
		rr, off, err = unpackRR(data, off)
		if err != nil {
			return nil, err
		}
		m.Answers = append(m.Answers, rr)
	}
	for i := 0; i < ns; i++ {
		var rr RR
		rr, off, err = unpackRR(data, off)
		if err != nil {
			return nil, err
		}
		m.Authority = append(m.Authority, rr)
	}
	for i := 0; i < ar; i++ {
		var rr RR
		rr, off, err = unpackRR(data, off)
		if err != nil {
			return nil, err
		}
		m.Additional = append(m.Additional, rr)
	}
	return m, nil
}

func unpackRR(data []byte, off int) (RR, int, error) {
	var rr RR
	var err error
	rr.Name, off, err = unpackName(data, off)
	if err != nil || off+10 > len(data) {
		return rr, off, ErrMalformed
	}
	rr.Type = binary.BigEndian.Uint16(data[off : off+2])
	rr.Class = binary.BigEndian.Uint16(data[off+2 : off+4])
	rr.TTL = binary.BigEndian.Uint32(data[off+4 : off+8])
	rdlen := int(binary.BigEndian.Uint16(data[off+8 : off+10]))
	off += 10
	end := off + rdlen
	if end > len(data) {
		return rr, off, ErrMalformed
	}
	switch rr.Type {
	case TypeA:
		if rdlen != 4 {
			return rr, off, ErrMalformed
		}
		var a [4]byte
		copy(a[:], data[off:end])
		rr.IP = netip.AddrFrom4(a)
	case TypeAAAA:
		if rdlen != 16 {
			return rr, off, ErrMalformed
		}
		var a [16]byte
		copy(a[:], data[off:end])
		rr.IP = netip.AddrFrom16(a)
	case TypeCNAME, TypeNS:
		target, _, err := unpackName(data, off)
		if err != nil {
			return rr, off, err
		}
		rr.Target = target
	case TypeSOA:
		mname, n, err := unpackName(data, off)
		if err != nil {
			return rr, off, err
		}
		rname, n2, err := unpackName(data, n)
		if err != nil || n2+20 > end {
			return rr, off, ErrMalformed
		}
		rr.SOA = &SOA{
			MName:   mname,
			RName:   rname,
			Serial:  binary.BigEndian.Uint32(data[n2 : n2+4]),
			Refresh: binary.BigEndian.Uint32(data[n2+4 : n2+8]),
			Retry:   binary.BigEndian.Uint32(data[n2+8 : n2+12]),
			Expire:  binary.BigEndian.Uint32(data[n2+12 : n2+16]),
			Minimum: binary.BigEndian.Uint32(data[n2+16 : n2+20]),
		}
	}
	off = end
	return rr, off, nil
}

// unpackName decodes a possibly compressed name and returns the offset just
// after the name in the original stream.
func unpackName(data []byte, off int) (string, int, error) {
	var sb strings.Builder
	next := -1
	jumps := 0
	for {
		if off >= len(data) {
			return "", off, ErrMalformed
		}
		l := int(data[off])
		switch l & 0xc0 {
		case 0x00:
			off++
			if l == 0 {
				if next < 0 {
					next = off
				}
				name := sb.String()
				if name == "" {
					name = "."
				}
				return name, next, nil
			}
			if off+l > len(data) || sb.Len()+l+1 > 255 {
				return "", off, ErrMalformed
			}
			sb.Write(data[off : off+l])
			sb.WriteByte('.')
			off += l
		case 0xc0:
			if off+1 >= len(data) {
				return "", off, ErrMalformed
			}
			ptr := int(binary.BigEndian.Uint16(data[off:off+2]) & 0x3fff)
			if next < 0 {
				next = off + 2
			}
			jumps++
			if jumps > 16 || ptr >= len(data) {
				return "", off, ErrMalformed
			}
			off = ptr
		default:
			return "", off, ErrMalformed
		}
	}
}
