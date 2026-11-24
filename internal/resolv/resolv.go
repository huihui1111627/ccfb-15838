// Package resolv implements the container-side stub resolver that the
// simulator drives: search-list expansion, ndots, address-family parallelism,
// per-query timeouts and retries.
//
// The search-list expansion follows the documented glibc/BIND rules
// (resolv.conf(5), glibc __res_context_search, Go's dnsConfig.nameList) and
// was checked against the system libresolv:
//
//   - a trailing dot makes a name rooted: it is looked up as-is only;
//   - at or above ndots the as-is name is tried first, then every search
//     suffix in order;
//   - below ndots every suffix is tried first and the as-is name last;
//   - suffixed names longer than 254 bytes are skipped and at most six search
//     domains are used.
//
// Two failure policies are modeled after the two dominant client stacks:
//
//   - PolicyLibc (glibc): each address family independently walks the search
//     list. NXDOMAIN, NODATA and SERVFAIL move to the next candidate after
//     the retry rounds, while a lost query stops that family's walk with
//     h_errno TRY_AGAIN. All of this was verified against libresolv with a
//     fake zone for every response code (see docs/resolver-notes.md).
//   - PolicyGo (Go's net resolver): for each candidate both families are
//     queried at once, the first candidate with addresses wins, and NXDOMAIN,
//     NODATA, SERVFAIL and timeouts all move on.
//
// The package never blocks and never reads the wall clock: all I/O and time
// come from Env, which the simulator implements on its virtual clock.
package resolv

import (
	"strings"
	"time"
)

// Policy selects failure handling.
type Policy int

const (
	// PolicyLibc models glibc res_search / getaddrinfo.
	PolicyLibc Policy = iota
	// PolicyGo models the Go standard library resolver.
	PolicyGo
)

func (p Policy) String() string {
	if p == PolicyGo {
		return "go"
	}
	return "glibc"
}

// ParsePolicy parses "glibc" or "go".
func ParsePolicy(s string) (Policy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "glibc", "libc", "":
		return PolicyLibc, true
	case "go":
		return PolicyGo, true
	}
	return PolicyLibc, false
}

// FamilyMode selects which address families an application asks for.
type FamilyMode int

const (
	// FamiliesBoth queries A and AAAA.
	FamiliesBoth FamilyMode = iota
	// FamiliesIPv4 queries A only.
	FamiliesIPv4
)

func (f FamilyMode) String() string {
	if f == FamiliesIPv4 {
		return "ipv4"
	}
	return "both"
}

// ParseFamilyMode parses "both" or "ipv4".
func ParseFamilyMode(s string) (FamilyMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both", "dual":
		return FamiliesBoth, true
	case "ipv4", "4", "a":
		return FamiliesIPv4, true
	}
	return FamiliesBoth, false
}

// Record types used by the resolver.
const (
	TypeA    uint16 = 1
	TypeAAAA uint16 = 28
)

// MaxSearchDomains is the resolv.conf search-list limit.
const MaxSearchDomains = 6

// Config is one container's resolver configuration.
type Config struct {
	// Nameservers in resolv.conf order. The simulator usually puts the
	// kube-dns service VIP here.
	Nameservers []string
	// Search contains rooted names (trailing dot), in order.
	Search []string
	// Ndots is the dot threshold. resolv.conf default is 1.
	Ndots int
	// Timeout is the per-query timeout. resolv.conf default is 5s.
	Timeout time.Duration
	// Attempts is the number of retry rounds per candidate. Default 2.
	Attempts int
	// Policy is the client failure policy.
	Policy Policy
	// Families selects which families the application asks for.
	Families FamilyMode
	// SerialFamilies issues A then AAAA (single-request).
	SerialFamilies bool
}

// DefaultConfig returns the libc defaults: ndots:1, timeout:5s, attempts:2.
func DefaultConfig() Config {
	return Config{Ndots: 1, Timeout: 5 * time.Second, Attempts: 2}
}

// Rooted returns name with exactly one trailing dot.
func Rooted(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "."
	}
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

// Dots counts label separators in a presentation name.
func Dots(name string) int { return strings.Count(name, ".") }

// NameList returns the candidate query names in resolver order.
func (c Config) NameList(name string) []string {
	if name == "" || strings.HasSuffix(name, ".") {
		return []string{Rooted(name)}
	}
	rooted := Rooted(name)
	search := c.Search
	if len(search) > MaxSearchDomains {
		search = search[:MaxSearchDomains]
	}
	hasNdots := Dots(name) >= c.Ndots
	out := make([]string, 0, 1+len(search))
	if hasNdots && len(rooted) <= 254 {
		out = append(out, rooted)
	}
	for _, suffix := range search {
		fqdn := rooted + Rooted(strings.TrimPrefix(strings.TrimSpace(suffix), "."))
		if len(fqdn) <= 254 {
			out = append(out, fqdn)
		}
	}
	if !hasNdots {
		out = append(out, rooted)
	}
	return out
}

// Status is the outcome of one query or one family walk.
type Status int

const (
	// StatusAnswer means records of the asked type were returned.
	StatusAnswer Status = iota
	// StatusNoData means NOERROR with no records of the asked type.
	StatusNoData
	// StatusNXDomain means the name does not exist.
	StatusNXDomain
	// StatusServFail means the server returned an error rcode and retries
	// were exhausted.
	StatusServFail
	// StatusTimeout means no response arrived and retries were exhausted.
	StatusTimeout
)

func (s Status) String() string {
	switch s {
	case StatusAnswer:
		return "answer"
	case StatusNoData:
		return "nodata"
	case StatusNXDomain:
		return "nxdomain"
	case StatusServFail:
		return "servfail"
	case StatusTimeout:
		return "timeout"
	}
	return "unknown"
}

// Reply is one DNS response as the stub resolver sees it.
type Reply struct {
	Status Status
	IPs    []string
}

// SendFunc issues one query. Implementations must call done exactly once,
// after Send returns, using StatusTimeout when no reply arrived in time.
type SendFunc func(server, name string, qtype uint16, timeout time.Duration, done func(Reply))

// Env is the resolver's world.
type Env struct {
	Send SendFunc
	Now  func() time.Duration
}

// QueryEvent describes one DNS packet the resolver caused to be sent.
type QueryEvent struct {
	Name      string
	Type      uint16
	Server    string
	Candidate int // index in Result.Candidates
	Attempt   int // 0 is the first transmission of this candidate
	At        time.Duration
}

// Result is the outcome of one hostname lookup.
type Result struct {
	Name        string
	IPs         []string
	Status      Status
	Latency     time.Duration
	Candidates  []string
	Queries     int
	WinningName string
}

// Session is one in-flight lookup.
type Session struct {
	env     Env
	cfg     Config
	name    string
	names   []string
	qtypes  []uint16
	rec     func(QueryEvent)
	onDone  func(Result)
	res     Result
	walks   []*walk
	started time.Duration
	done    bool
}

// Start resolves name for network ("ip", "ip4", "ip6"), calling rec for every
// packet sent and onDone exactly once. rec may be nil.
func Start(env Env, cfg Config, name, network string, rec func(QueryEvent), onDone func(Result)) *Session {
	s := &Session{
		env:     env,
		cfg:     cfg,
		name:    name,
		names:   cfg.NameList(name),
		rec:     rec,
		onDone:  onDone,
		started: env.Now(),
		res:     Result{Name: name},
	}
	// network may pin the family ("ip4"/"ip6"); "ip" and "" leave the choice
	// to the family mode in the configuration.
	switch network {
	case "ip4":
		s.qtypes = []uint16{TypeA}
	case "ip6":
		s.qtypes = []uint16{TypeAAAA}
	default:
		s.qtypes = []uint16{TypeA, TypeAAAA}
		if cfg.Families == FamiliesIPv4 {
			s.qtypes = []uint16{TypeA}
		}
	}
	if len(cfg.Nameservers) == 0 || len(s.names) == 0 {
		s.finish(StatusNXDomain)
		return s
	}
	if cfg.Policy == PolicyGo {
		w := &walk{s: s, qtype: 0, goStyle: true}
		s.walks = []*walk{w}
		w.begin()
		return s
	}
	// Create every walk before starting any: a synchronous transport can
	// complete the first family's walk inside begin(), and walkCompletions
	// must see the other families or it would end the session early.
	for _, qt := range s.qtypes {
		s.walks = append(s.walks, &walk{s: s, qtype: qt})
	}
	for i := range s.walks {
		if i == 0 || !cfg.SerialFamilies {
			s.walks[i].begin()
		}
	}
	return s
}

// Cancel abandons the lookup; late replies are ignored.
func (s *Session) Cancel() { s.done = true }

// Finished reports whether the lookup is complete.
func (s *Session) Finished() bool { return s.done }

// finish completes the session exactly once and computes the final status.
func (s *Session) finish(st Status) {
	if s.done {
		return
	}
	s.done = true
	s.res.Status = st
	s.res.Latency = s.env.Now() - s.started
	if s.onDone != nil {
		s.onDone(s.res)
	}
}

// walkCompletions handles family walks finishing: when every walk is done the
// session is done.
func (s *Session) walkCompletions() {
	if s.done {
		return
	}
	for _, w := range s.walks {
		if !w.done {
			return
		}
	}
	// Fold the per-family outcomes.
	var ips []string
	st := StatusNXDomain
	sawNoData, sawFail, sawTimeout := false, false, false
	for _, w := range s.walks {
		switch w.final {
		case StatusAnswer:
			ips = append(ips, w.ips...)
		case StatusNoData:
			sawNoData = true
		case StatusServFail:
			sawFail = true
		case StatusTimeout:
			sawTimeout = true
		}
	}
	if len(ips) > 0 {
		s.res.IPs = ips
		s.finish(StatusAnswer)
		return
	}
	switch {
	case sawTimeout:
		st = StatusTimeout
	case sawFail:
		st = StatusServFail
	case sawNoData:
		st = StatusNoData
	}
	s.finish(st)
}

// startNextSerialFamily launches the next serial family once the previous one
// is terminal.
func (s *Session) startNextSerialFamily(finished *walk) {
	if !s.cfg.SerialFamilies {
		return
	}
	for i, w := range s.walks {
		if w == finished && i+1 < len(s.walks) && !s.walks[i+1].started {
			s.walks[i+1].begin()
		}
	}
}

// walk is one independent search-list traversal. For PolicyLibc there is one
// walk per address family; for PolicyGo a single walk queries all families
// per candidate.
type walk struct {
	s       *Session
	qtype   uint16 // 0 means "all families" (PolicyGo)
	goStyle bool
	idx     int
	started bool
	done    bool
	final   Status
	ips     []string
	// Go-style per-candidate results.
	candStatus []Status
	candIPs    [][]string
	pending    int
}

func (w *walk) begin() {
	if w.done || w.s.done {
		return
	}
	w.started = true
	w.next()
}

// next tries the current candidate; if the list is exhausted the walk ends.
func (w *walk) next() {
	s := w.s
	if s.done {
		return
	}
	if w.idx >= len(s.names) {
		w.finish(StatusNXDomain)
		return
	}
	fqdn := s.names[w.idx]
	addCandidate(&s.res, fqdn)

	if w.goStyle {
		w.nextGo(fqdn)
		return
	}
	p := &probe{s: s, fqdn: fqdn, qtype: w.qtype, idx: w.idx}
	p.done = func(st Status, ips []string) {
		switch st {
		case StatusAnswer:
			w.ips = ips
			w.finish(StatusAnswer)
		case StatusTimeout:
			if s.cfg.Policy == PolicyLibc {
				// Measured libresolv behavior: a lost query (h_errno
				// TRY_AGAIN) ends the search after the retry rounds instead
				// of walking the remaining candidates.
				w.finish(StatusTimeout)
				return
			}
			w.remember(st)
			w.idx++
			w.next()
		default:
			// NXDOMAIN, NODATA and SERVFAIL all move to the next candidate
			// in libresolv's res_search.
			w.remember(st)
			w.idx++
			w.next()
		}
	}
	p.start()
}

// nextGo queries all families for one candidate and waits for all of them
// before deciding, mirroring Go's goLookupIPCNAMEOrder.
func (w *walk) nextGo(fqdn string) {
	s := w.s
	w.pending = len(s.qtypes)
	w.candStatus = make([]Status, len(s.qtypes))
	w.candIPs = make([][]string, len(s.qtypes))
	for i, qt := range s.qtypes {
		i, qt := i, qt
		p := &probe{s: s, fqdn: fqdn, qtype: qt, idx: w.idx}
		p.done = func(st Status, ips []string) {
			w.candStatus[i] = st
			w.candIPs[i] = ips
			w.pending--
			if w.pending > 0 || w.done || s.done {
				return
			}
			w.decideGo()
		}
		p.start()
	}
}

// decideGo applies the Go policy to one candidate's family results.
func (w *walk) decideGo() {
	s := w.s
	var addrs []string
	sawNoData, sawFail, sawTimeout := false, false, false
	for i, st := range w.candStatus {
		switch st {
		case StatusAnswer:
			addrs = append(addrs, w.candIPs[i]...)
		case StatusNoData:
			sawNoData = true
		case StatusServFail:
			sawFail = true
		case StatusTimeout:
			sawTimeout = true
		}
	}
	if len(addrs) > 0 {
		w.ips = addrs
		s.res.WinningName = s.names[w.idx]
		w.finish(StatusAnswer)
		return
	}
	// Go keeps searching. Remember the most severe failure for the final
	// error, matching the stdlib's lastErr behavior.
	if sawNoData {
		w.final = StatusNoData
	}
	if sawFail {
		w.final = StatusServFail
	}
	if sawTimeout {
		w.final = StatusTimeout
	}
	w.idx++
	w.next()
}

// remember keeps the most severe failure seen for the final error status.
func (w *walk) remember(st Status) {
	switch st {
	case StatusNoData:
		if w.final == 0 {
			w.final = StatusNoData
		}
	case StatusServFail:
		w.final = StatusServFail
	case StatusTimeout:
		w.final = StatusTimeout
	}
}

func (w *walk) finish(st Status) {
	if w.done {
		return
	}
	w.done = true
	w.final = st
	if st == StatusAnswer && w.s.res.WinningName == "" {
		w.s.res.WinningName = w.s.names[w.idx]
	}
	w.s.startNextSerialFamily(w)
	w.s.walkCompletions()
}

// probe sends one candidate/family query with the nameserver and attempt
// retry policy.
type probe struct {
	s     *Session
	fqdn  string
	qtype uint16
	idx   int

	attempt int
	server  int
	over    bool
	lastErr Status // StatusServFail or StatusTimeout
	done    func(Status, []string)
}

func (p *probe) start() {
	p.attempt = 0
	p.server = 0
	p.lastErr = 0
	p.send()
}

func (p *probe) send() {
	s := p.s
	if p.over || s.done {
		return
	}
	attempts := s.cfg.Attempts
	if attempts < 1 {
		attempts = 1
	}
	if p.attempt >= attempts {
		p.complete(p.lastFailure(), nil)
		return
	}
	servers := s.cfg.Nameservers
	server := servers[p.server%len(servers)]
	s.res.Queries++
	if s.rec != nil {
		s.rec(QueryEvent{
			Name: p.fqdn, Type: p.qtype, Server: server,
			Candidate: p.idx, Attempt: p.attempt, At: s.env.Now(),
		})
	}
	timeout := s.cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	s.env.Send(server, p.fqdn, p.qtype, timeout, func(r Reply) {
		if p.over || s.done {
			return
		}
		switch r.Status {
		case StatusAnswer:
			p.complete(StatusAnswer, r.IPs)
		case StatusNXDomain, StatusNoData:
			p.complete(r.Status, nil)
		default:
			p.lastErr = r.Status
			p.retry()
		}
	})
}

func (p *probe) retry() {
	s := p.s
	servers := s.cfg.Nameservers
	p.server++
	if p.server >= len(servers) {
		p.server = 0
		p.attempt++
	}
	if p.attempt >= maxInt(s.cfg.Attempts, 1) {
		p.complete(p.lastFailure(), nil)
		return
	}
	p.send()
}

// lastFailure reports how the last attempt failed: an answered error rcode
// is SERVFAIL, a lost packet is a timeout.
func (p *probe) lastFailure() Status {
	if p.lastErr == 0 {
		return StatusTimeout
	}
	return p.lastErr
}

func (p *probe) complete(st Status, ips []string) {
	if p.over {
		return
	}
	p.over = true
	p.done(st, ips)
}

func addCandidate(res *Result, name string) {
	if n := len(res.Candidates); n > 0 && res.Candidates[n-1] == name {
		return
	}
	for _, c := range res.Candidates {
		if c == name {
			return
		}
	}
	res.Candidates = append(res.Candidates, name)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
