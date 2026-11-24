// Package oracle drives the system C resolver (libresolv on macOS/glibc on
// Linux) against a fake UDP nameserver. It exists so the simulator's
// client-side resolver can be checked against real libc behavior instead of
// against the simulator's own assumptions.
//
// It is used by the conformance test suite and by the libresolv-oracle demo
// command. Compilation needs a C compiler; tests skip cleanly when none is
// available.
package oracle

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Parameters fully describe one resolver state override.
type Parameters struct {
	Port    int
	Name    string
	QType   string // "A" or "AAAA"
	Ndots   int
	Timeout int // seconds
	Retry   int
	Search  []string
}

// Result is the outcome printed by the harness.
type Result struct {
	RC        int `json:"rc"`
	HErrno    int `json:"h_errno"`
	ElapsedMS int `json:"elapsed_ms"`
}

// Available reports whether a C compiler is present.
func Available() bool {
	_, err := exec.LookPath("cc")
	return err == nil
}

// Run compiles and runs the harness.
func Run(p Parameters) (Result, error) {
	dir, err := os.MkdirTemp("", "dnsim-oracle")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	cpath := filepath.Join(dir, "oracle.c")
	bin := filepath.Join(dir, "oracle")
	if err := os.WriteFile(cpath, []byte(Source(p)), 0o644); err != nil {
		return Result{}, err
	}
	args := []string{"-O0", "-o", bin, cpath}
	if runtime.GOOS == "darwin" {
		args = append(args, "-lresolv")
	}
	cc := exec.Command("cc", args...)
	if out, err := cc.CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("cc: %v\n%s", err, out)
	}
	out, err := exec.Command(bin).Output()
	if err != nil {
		return Result{}, fmt.Errorf("oracle: %v", err)
	}
	var r Result
	if err := json.Unmarshal(out, &r); err != nil {
		return Result{}, fmt.Errorf("oracle output %q: %w", out, err)
	}
	return r, nil
}

func cstr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// Source renders the C harness.
func Source(p Parameters) string {
	nsType := "ns_t_a"
	if strings.EqualFold(p.QType, "AAAA") {
		nsType = "ns_t_aaaa"
	}
	if p.Timeout < 1 {
		p.Timeout = 1
	}
	if p.Retry < 1 {
		p.Retry = 1
	}
	if len(p.Search) > 6 {
		p.Search = p.Search[:6]
	}
	var sb strings.Builder
	sb.WriteString(`#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <resolv.h>
#include <netdb.h>
#include <arpa/nameser.h>
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/time.h>

static long now_ms(void) {
    struct timeval tv; gettimeofday(&tv, NULL);
    return (long)tv.tv_sec * 1000 + tv.tv_usec / 1000;
}

int main(void) {
    struct __res_state st;
    memset(&st, 0, sizeof st);
    if (res_ninit(&st) != 0) { fprintf(stderr, "res_ninit failed\n"); return 2; }
    st.nscount = 1;
    memset(&st.nsaddr_list[0], 0, sizeof(st.nsaddr_list[0]));
    st.nsaddr_list[0].sin_len = sizeof(struct sockaddr_in);
    st.nsaddr_list[0].sin_family = AF_INET;
    st.nsaddr_list[0].sin_port = htons((unsigned short)PORT);
    st.nsaddr_list[0].sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    st.options |= RES_DNSRCH | RES_RECURSE;
    st.retrans = TIMEOUT;
    st.retry = RETRY;
    st.ndots = NDOTS;
`)
	for i, d := range p.Search {
		fmt.Fprintf(&sb, "    st.dnsrch[%d] = (char*)%s;\n", i, cstr(d))
	}
	fmt.Fprintf(&sb, "    st.dnsrch[%d] = NULL;\n", len(p.Search))
	sb.WriteString(`
    unsigned char answer[4096];
    long t0 = now_ms();
    int n = res_nsearch(&st, NAME, ns_c_in, QTYPE, answer, sizeof answer);
    long t1 = now_ms();
    printf("{\"rc\":%d,\"h_errno\":%d,\"elapsed_ms\":%ld}\n", n, h_errno, t1 - t0);
    res_nclose(&st);
    return 0;
}
`)
	src := sb.String()
	src = strings.ReplaceAll(src, "PORT", fmt.Sprint(p.Port))
	src = strings.ReplaceAll(src, "TIMEOUT", fmt.Sprint(p.Timeout))
	src = strings.ReplaceAll(src, "RETRY", fmt.Sprint(p.Retry))
	src = strings.ReplaceAll(src, "NDOTS", fmt.Sprint(p.Ndots))
	src = strings.ReplaceAll(src, "QTYPE", nsType)
	src = strings.ReplaceAll(src, "NAME", cstr(p.Name))
	return src
}

// Query is one observed query.
type Query struct {
	Name  string
	Type  uint16
	At    time.Duration
	rcode int
}

// Server is a recording fake UDP nameserver.
type Server struct {
	Conn net.PacketConn

	mu      sync.Mutex
	queries []Query
	drop    bool
	rcode   int
	answer  bool
	start   time.Time
}

// NewServer starts a recording nameserver on 127.0.0.1:0.
func NewServer() (*Server, error) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{Conn: pc, rcode: 3, start: time.Now()}
	go s.serve()
	return s, nil
}

// Port returns the UDP port.
func (s *Server) Port() int { return s.Conn.LocalAddr().(*net.UDPAddr).Port }

// SetDrop makes the server ignore all queries (simulating packet loss).
func (s *Server) SetDrop(drop bool) {
	s.mu.Lock()
	s.drop = drop
	s.mu.Unlock()
}

// SetResponse configures the rcode and whether an A answer is returned.
func (s *Server) SetResponse(rcode int, answerA bool) {
	s.mu.Lock()
	s.rcode = rcode
	s.answer = answerA
	s.mu.Unlock()
}

// Queries returns a copy of the queries observed so far.
func (s *Server) Queries() []Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Query(nil), s.queries...)
}

// Reset clears the query log.
func (s *Server) Reset() {
	s.mu.Lock()
	s.queries = nil
	s.start = time.Now()
	s.mu.Unlock()
}

// Close stops the server.
func (s *Server) Close() { s.Conn.Close() }

func (s *Server) serve() {
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.Conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		name, qtype, ok := parseQuestion(buf[:n])
		if !ok {
			continue
		}
		s.mu.Lock()
		s.queries = append(s.queries, Query{Name: name, Type: qtype, At: time.Since(s.start)})
		drop, rcode, answerA := s.drop, s.rcode, s.answer
		s.mu.Unlock()
		if drop {
			continue
		}
		s.Conn.WriteTo(response(buf[:n], name, qtype, rcode, answerA), addr)
	}
}

func parseQuestion(b []byte) (string, uint16, bool) {
	i := 12
	var sb strings.Builder
	for {
		if i >= len(b) {
			return "", 0, false
		}
		l := int(b[i])
		if l == 0 {
			i++
			break
		}
		if l&0xc0 != 0 || i+1+l > len(b) {
			return "", 0, false
		}
		sb.Write(b[i+1 : i+1+l])
		sb.WriteByte('.')
		i += 1 + l
	}
	if i+4 > len(b) {
		return "", 0, false
	}
	qtype := uint16(b[i])<<8 | uint16(b[i+1])
	return sb.String(), qtype, true
}

func response(query []byte, name string, qtype uint16, rcode int, answerA bool) []byte {
	// Reuse the question section from the query.
	i := 12
	for i < len(query) && query[i] != 0 {
		i += int(query[i]) + 1
	}
	i++ // root label
	if i+4 > len(query) {
		return nil
	}
	i += 4
	question := query[12:i]
	hdr := make([]byte, 12)
	copy(hdr[0:2], query[0:2])
	hdr[2] = 0x81
	hdr[3] = 0x80 | byte(rcode)
	hdr[5] = 1 // QDCOUNT
	if rcode == 0 && answerA && qtype == 1 {
		hdr[7] = 1
	}
	out := append(hdr, question...)
	if rcode == 0 && answerA && qtype == 1 {
		// Pointer to the question name, A record, 10.0.0.1, TTL 30.
		out = append(out, 0xc0, 0x0c, 0x00, 0x01, 0x00, 0x01,
			0x00, 0x00, 0x00, 0x1e, 0x00, 0x04, 10, 0, 0, 1)
	}
	return out
}
