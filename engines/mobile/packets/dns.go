package packets

import (
	"context"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The DNS responder. It answers on DNSV4/DNSV6 inside the tunnel with the
// engine's rules:
//
//   - a name one tailnet owns is looked up in that tailnet's own DNS and
//     answered with addresses the phone can route: the device's real
//     Tailscale address when only that tailnet has it, otherwise a stable
//     synthetic address mapped to (tailnet, real address);
//   - a name two tailnets claim: REFUSED;
//   - a name of an off tailnet (paused, signed out, or a domain it served
//     when it last ran): SERVFAIL, never looked up anywhere else;
//   - any other name: forwarded to the device's own resolvers when an
//     Upstream is set (Android), else REFUSED (iOS sends us only owned names).
//
// No owned name is ever sent to Upstream.

type dnsServer struct {
	rules  Rules
	tdns   TailnetDNS
	routes RouteChecker
	up     Upstream
	pool   *Pool
	lim    *Limits
	now    func() time.Time
	logf   func(string, ...any)
	stats  *Stats
	cache  *dnsCache
}

const (
	typeSVCB  = dnsmessage.Type(64)
	typeHTTPS = dnsmessage.Type(65)
	ednsSize  = 1232 // the DNS flag day 2020 recommendation
)

// handle answers one query. udp is false for DNS over TCP (no size limit).
// It returns nil for a message that is not a query worth answering.
func (s *dnsServer) handle(ctx context.Context, query []byte, udp bool) []byte {
	s.stats.DNSQueries.Add(1)
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(h, nil, dnsmessage.RCodeFormatError, nil, false, 0)
	}
	if _, err := p.Question(); err != dnsmessage.ErrSectionDone {
		return reply(h, &q, dnsmessage.RCodeFormatError, nil, false, 0)
	}
	edns, size := false, 512
	if p.SkipAllAnswers() == nil && p.SkipAllAuthorities() == nil {
		for {
			rh, err := p.AdditionalHeader()
			if err != nil {
				break
			}
			if rh.Type == dnsmessage.TypeOPT {
				edns = true
				size = max(512, min(int(rh.Class), ednsSize))
			}
			if p.SkipAdditional() != nil {
				break
			}
		}
	}
	if !udp {
		size = 0
	}
	if h.OpCode != 0 {
		return reply(h, &q, dnsmessage.RCodeNotImplemented, nil, edns, size)
	}
	if q.Class != dnsmessage.ClassINET {
		return reply(h, &q, dnsmessage.RCodeRefused, nil, edns, size)
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	rcode, answers, forward := s.answer(ctx, name, q)
	if forward {
		if s.up == nil {
			s.stats.DNSRefused.Add(1)
			return reply(h, &q, dnsmessage.RCodeRefused, nil, edns, size)
		}
		s.stats.DNSForwarded.Add(1)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resp, err := s.up.Exchange(ctx, query)
		if err != nil || len(resp) < 12 {
			return reply(h, &q, dnsmessage.RCodeServerFailure, nil, edns, size)
		}
		resp[0], resp[1] = query[0], query[1]
		return resp
	}
	return reply(h, &q, rcode, answers, edns, size)
}

// answer decides a query. forward: the name is nobody's (Upstream decides).
func (s *dnsServer) answer(ctx context.Context, name string, q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource, bool) {
	if ip, ok := reverseAddr(name); ok {
		return s.answerReverse(ctx, ip, q)
	}
	tailnet, v := s.rules.ClassifyName(name)
	switch v {
	case Owned:
		return s.answerOwned(ctx, tailnet, name, q)
	case Conflict:
		s.stats.DNSRefused.Add(1)
		return dnsmessage.RCodeRefused, nil, false
	case Claimed, TailnetSpace:
		return dnsmessage.RCodeServerFailure, nil, false
	}
	return 0, nil, true
}

// answerReverse: PTR names for Tailscale and synthetic addresses go to the
// one tailnet that has the address and never anywhere else.
func (s *dnsServer) answerReverse(ctx context.Context, ip netip.Addr, q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource, bool) {
	tailnet, real := "", ip
	switch {
	case s.pool.Contains(ip):
		t, r, ok := s.pool.Lookup(ip)
		if !ok {
			return dnsmessage.RCodeNameError, nil, false
		}
		tailnet, real = t, r
	case SyntheticV4.Contains(ip) || SyntheticV6.Contains(ip):
		return dnsmessage.RCodeNameError, nil, false
	case IsTailscaleIP(ip):
		t, v := s.rules.ClassifyAddr(ip)
		if v != Owned {
			return dnsmessage.RCodeNameError, nil, false
		}
		tailnet = t
	default:
		return 0, nil, true
	}
	if q.Type != dnsmessage.TypePTR {
		return dnsmessage.RCodeSuccess, nil, false
	}
	return s.relay(ctx, tailnet, reverseName(real), q)
}

func (s *dnsServer) answerOwned(ctx context.Context, tailnet, name string, q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource, bool) {
	switch q.Type {
	case dnsmessage.TypeA, dnsmessage.TypeAAAA:
	case typeHTTPS, typeSVCB:
		// Their address hints would bypass the mapping below: answer "no
		// such record" and let the app ask for A/AAAA.
		return dnsmessage.RCodeSuccess, nil, false
	default:
		return s.relay(ctx, tailnet, name, q)
	}
	e, err := s.lookup(ctx, tailnet, name, q.Type)
	if err != nil {
		s.logf("dns: %s via %s: %v", name, tailnet, err)
		return dnsmessage.RCodeServerFailure, nil, false
	}
	if e.rcode != dnsmessage.RCodeSuccess {
		return e.rcode, nil, false
	}
	var out []dnsmessage.Resource
	ttl := e.ttl(s.now())
	for _, a := range e.addrs {
		mapped, real, ok := s.mapAddr(tailnet, a)
		if !ok {
			continue
		}
		t := min(ttl, uint32(s.lim.SyntheticTTL/time.Second))
		if real {
			t = min(ttl, uint32(s.lim.RealTTL/time.Second))
		}
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: t}
		if mapped.Is4() {
			out = append(out, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AResource{A: mapped.As4()}})
		} else {
			out = append(out, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AAAAResource{AAAA: mapped.As16()}})
		}
	}
	if len(out) == 0 && len(e.addrs) > 0 {
		return dnsmessage.RCodeServerFailure, nil, false // pool full
	}
	return dnsmessage.RCodeSuccess, out, false
}

// mapAddr is the address the phone should use for a, which tailnet's DNS
// returned. real: a is answered unchanged as a uniquely owned Tailscale
// address (a shorter TTL, since ownership can change).
func (s *dnsServer) mapAddr(tailnet string, a netip.Addr) (netip.Addr, bool, bool) {
	a = a.Unmap()
	if IsTailscaleIP(a) {
		if t, v := s.rules.ClassifyAddr(a); v == Owned && t == tailnet {
			return a, true, true
		}
	} else if s.routes == nil || !s.routes.Routes(tailnet, a) {
		return a, false, true // reached over the phone's own network
	}
	synth, err := s.pool.Get(tailnet, a)
	if err != nil {
		s.logf("dns: %v", err)
		return netip.Addr{}, false, false
	}
	return synth, false, true
}

// relay passes a tailnet's answer for a type with no addresses in it (TXT,
// SRV, PTR, ...), dropping any address records.
func (s *dnsServer) relay(ctx context.Context, tailnet, name string, q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.tdns.QueryDNS(ctx, tailnet, name, typeName(q.Type))
	if err != nil {
		return dnsmessage.RCodeServerFailure, nil, false
	}
	var m dnsmessage.Message
	if m.Unpack(raw) != nil {
		return dnsmessage.RCodeServerFailure, nil, false
	}
	var out []dnsmessage.Resource
	for _, r := range m.Answers {
		switch r.Header.Type {
		case dnsmessage.TypeA, dnsmessage.TypeAAAA, typeHTTPS, typeSVCB:
			continue
		}
		r.Header.TTL = min(r.Header.TTL, uint32(s.lim.SyntheticTTL/time.Second))
		out = append(out, r)
	}
	return m.RCode, out, false
}

// lookup returns a tailnet's addresses for name, from the cache when fresh.
func (s *dnsServer) lookup(ctx context.Context, tailnet, name string, qt dnsmessage.Type) (*dnsEntry, error) {
	k := dnsKey{tailnet, name, qt}
	now := s.now()
	if e := s.cache.get(k, now); e != nil {
		s.stats.DNSCacheHits.Add(1)
		return e, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.tdns.QueryDNS(ctx, tailnet, name, typeName(qt))
	if err != nil {
		return nil, err
	}
	e, err := parseAddrs(raw, qt)
	if err != nil {
		return nil, err
	}
	ttl := e.minTTL
	if e.rcode != dnsmessage.RCodeSuccess || len(e.addrs) == 0 {
		ttl = uint32(s.lim.NegativeTTL / time.Second)
	}
	e.expires = now.Add(time.Duration(ttl) * time.Second)
	if ttl > 0 && (e.rcode == dnsmessage.RCodeSuccess || e.rcode == dnsmessage.RCodeNameError) {
		s.cache.put(k, e, now)
	}
	return e, nil
}

func parseAddrs(raw []byte, qt dnsmessage.Type) (*dnsEntry, error) {
	var p dnsmessage.Parser
	h, err := p.Start(raw)
	if err != nil {
		return nil, err
	}
	e := &dnsEntry{rcode: h.RCode, minTTL: ^uint32(0)}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}
	for {
		ah, err := p.AnswerHeader()
		if err != nil {
			break
		}
		switch {
		case ah.Type == dnsmessage.TypeA && qt == dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil, err
			}
			e.addrs = append(e.addrs, netip.AddrFrom4(r.A))
		case ah.Type == dnsmessage.TypeAAAA && qt == dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil, err
			}
			e.addrs = append(e.addrs, netip.AddrFrom16(r.AAAA).Unmap())
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil, err
			}
			continue
		}
		e.minTTL = min(e.minTTL, ah.TTL)
	}
	if len(e.addrs) == 0 {
		e.minTTL = 0
	}
	return e, nil
}

// reply builds a response. size > 0 limits it (UDP): an answer that does not
// fit is sent empty with TC set, so the client retries over TCP.
func reply(h dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, answers []dnsmessage.Resource, edns bool, size int) []byte {
	build := func(answers []dnsmessage.Resource, tc bool) []byte {
		m := dnsmessage.Message{
			Header: dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired,
				RecursionAvailable: true, RCode: rcode, Truncated: tc},
			Answers: answers,
		}
		if q != nil {
			m.Questions = []dnsmessage.Question{*q}
		}
		if edns {
			var opt dnsmessage.ResourceHeader
			_ = opt.SetEDNS0(ednsSize, dnsmessage.RCodeSuccess, false)
			m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
		}
		out, err := m.AppendPack(make([]byte, 0, 512))
		if err != nil {
			m.Answers, m.Header.RCode = nil, dnsmessage.RCodeServerFailure
			out, _ = m.AppendPack(make([]byte, 0, 512))
		}
		return out
	}
	out := build(answers, false)
	if size > 0 && len(out) > size {
		out = build(nil, true)
	}
	return out
}

// typeName is the name QueryDNS takes for a type ("A", "AAAA", "TYPE65").
func typeName(t dnsmessage.Type) string {
	return strings.TrimPrefix(t.String(), "Type")
}

// reverseAddr parses an in-addr.arpa or ip6.arpa name.
func reverseAddr(name string) (netip.Addr, bool) {
	if rest, ok := strings.CutSuffix(name, ".in-addr.arpa"); ok {
		parts := strings.Split(rest, ".")
		if len(parts) != 4 {
			return netip.Addr{}, false
		}
		var b [4]byte
		for i, p := range parts {
			n, err := strconv.ParseUint(p, 10, 8)
			if err != nil {
				return netip.Addr{}, false
			}
			b[3-i] = byte(n)
		}
		return netip.AddrFrom4(b), true
	}
	if rest, ok := strings.CutSuffix(name, ".ip6.arpa"); ok {
		parts := strings.Split(rest, ".")
		if len(parts) != 32 {
			return netip.Addr{}, false
		}
		var hexs [32]byte
		for i, p := range parts {
			if len(p) != 1 {
				return netip.Addr{}, false
			}
			hexs[31-i] = p[0]
		}
		var b [16]byte
		if _, err := hex.Decode(b[:], hexs[:]); err != nil {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16(b), true
	}
	return netip.Addr{}, false
}

// reverseName is the PTR name for ip.
func reverseName(ip netip.Addr) string {
	if ip.Is4() {
		b := ip.As4()
		return strconv.Itoa(int(b[3])) + "." + strconv.Itoa(int(b[2])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[0])) + ".in-addr.arpa"
	}
	b := ip.As16()
	var sb strings.Builder
	const digits = "0123456789abcdef"
	for i := 15; i >= 0; i-- {
		sb.WriteByte(digits[b[i]&0xf])
		sb.WriteByte('.')
		sb.WriteByte(digits[b[i]>>4])
		sb.WriteByte('.')
	}
	sb.WriteString("ip6.arpa")
	return sb.String()
}

// The cache of tailnet lookups (before mapping: whether an address is
// answered real or synthetic is decided on every answer).

type dnsKey struct {
	tailnet, name string
	qt            dnsmessage.Type
}

type dnsEntry struct {
	rcode   dnsmessage.RCode
	addrs   []netip.Addr
	minTTL  uint32
	expires time.Time
}

// ttl is the seconds left.
func (e *dnsEntry) ttl(now time.Time) uint32 {
	d := e.expires.Sub(now)
	if d <= 0 {
		if e.minTTL == ^uint32(0) {
			return 0
		}
		return e.minTTL // fresh from the tailnet, never cached
	}
	return uint32((d + time.Second - 1) / time.Second)
}

type dnsCache struct {
	mu  sync.Mutex
	max int
	m   map[dnsKey]*dnsEntry
}

func newDNSCache(n int) *dnsCache { return &dnsCache{max: n, m: map[dnsKey]*dnsEntry{}} }

func (c *dnsCache) get(k dnsKey, now time.Time) *dnsEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[k]
	if e == nil {
		return nil
	}
	if !now.Before(e.expires) {
		delete(c.m, k)
		return nil
	}
	return e
}

func (c *dnsCache) put(k dnsKey, e *dnsEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.max <= 0 {
		return
	}
	if len(c.m) >= c.max {
		for k2, e2 := range c.m {
			if !now.Before(e2.expires) {
				delete(c.m, k2)
			}
		}
		for k2 := range c.m { // still full: drop an arbitrary entry
			if len(c.m) < c.max {
				break
			}
			delete(c.m, k2)
		}
	}
	c.m[k] = e
}

// flushTailnet drops a tailnet's entries (it stopped or changed).
func (c *dnsCache) flushTailnet(tailnet string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if k.tailnet == tailnet {
			delete(c.m, k)
		}
	}
}

func (c *dnsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
