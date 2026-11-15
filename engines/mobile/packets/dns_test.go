package packets

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsRig: two tailnets that both use 100.64.0.2, one device only "work" has,
// a paused tailnet, a conflict and an off tailnet's old domain.
func dnsRig(t *testing.T, up Upstream, now func() time.Time) *rig {
	r := newRig(t, rigOpts{upstream: up, now: now, noPhone: true})
	r.rules.name("work.example", "work", Owned)
	r.rules.name("home.example", "home", Owned)
	r.rules.name("off.example", "off", Owned)
	r.rules.name("shared.example", "", Conflict)
	r.rules.name("old.example", "gone", Claimed)
	r.rules.addr("100.64.0.2", "work", "home")
	r.rules.addr("100.80.0.5", "work")
	r.rules.addr("fd7a:115c:a1e0::5", "work")
	r.dns.set("work", "db.work.example", "100.64.0.2", "fd7a:115c:a1e0::2")
	r.dns.set("work", "solo.work.example", "100.80.0.5", "fd7a:115c:a1e0::5")
	r.dns.set("work", "nas.work.example", "192.168.50.10")
	r.dns.set("home", "pi.home.example", "100.64.0.2")
	r.dns.set("off", "x.off.example", "100.90.0.1")
	r.dns.paused["off"] = true
	return r
}

func TestDNSOwnedUniqueGetsRealAddress(t *testing.T) {
	r := dnsRig(t, nil, nil)
	m := r.query("solo.work.example", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || !slices.Equal(answerAddrs(m), []netip.Addr{netip.MustParseAddr("100.80.0.5")}) {
		t.Fatalf("A: %v %v", m.RCode, answerAddrs(m))
	}
	if ttl := m.Answers[0].Header.TTL; ttl != 60 {
		t.Fatalf("real address TTL %d, want the 60 s cap", ttl)
	}
	m = r.query("solo.work.example", dnsmessage.TypeAAAA)
	if !slices.Equal(answerAddrs(m), []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::5")}) {
		t.Fatalf("AAAA: %v", answerAddrs(m))
	}
	// Not a Tailscale address, and no RouteChecker: answered unchanged.
	m = r.query("nas.work.example", dnsmessage.TypeA)
	if !slices.Equal(answerAddrs(m), []netip.Addr{netip.MustParseAddr("192.168.50.10")}) {
		t.Fatalf("nas: %v", answerAddrs(m))
	}
}

func TestDNSSharedAddressGetsSynthetic(t *testing.T) {
	r := dnsRig(t, nil, nil)
	db := answerAddrs(r.query("db.work.example", dnsmessage.TypeA))
	pi := answerAddrs(r.query("pi.home.example", dnsmessage.TypeA))
	if len(db) != 1 || len(pi) != 1 || !r.m.pool.Contains(db[0]) || !r.m.pool.Contains(pi[0]) || db[0] == pi[0] {
		t.Fatalf("db %v pi %v: want two distinct synthetic addresses", db, pi)
	}
	if tn, real, _ := r.m.pool.Lookup(db[0]); tn != "work" || real != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("db maps to %s %v", tn, real)
	}
	if tn, _, _ := r.m.pool.Lookup(pi[0]); tn != "home" {
		t.Fatalf("pi maps to %s", tn)
	}
	// stable
	r.m.FlushDNS("work")
	if again := answerAddrs(r.query("db.work.example", dnsmessage.TypeA)); !slices.Equal(again, db) {
		t.Fatalf("not stable: %v then %v", db, again)
	}
	// v6 of a device whose v6 address is not learned: synthetic v6 too
	db6 := answerAddrs(r.query("db.work.example", dnsmessage.TypeAAAA))
	if len(db6) != 1 || !SyntheticV6.Contains(db6[0]) {
		t.Fatalf("db AAAA %v", db6)
	}
}

func TestDNSRefusals(t *testing.T) {
	up := &fakeUpstream{}
	r := dnsRig(t, up, nil)
	for _, c := range []struct {
		name string
		want dnsmessage.RCode
	}{
		{"a.shared.example", dnsmessage.RCodeRefused},
		{"x.off.example", dnsmessage.RCodeServerFailure}, // paused: never elsewhere
		{"y.old.example", dnsmessage.RCodeServerFailure}, // an off tailnet's old domain
		{"missing.work.example", dnsmessage.RCodeNameError},
	} {
		m := r.query(c.name, dnsmessage.TypeA)
		if m.RCode != c.want || len(m.Answers) != 0 {
			t.Errorf("%s: %v %v, want %v", c.name, m.RCode, answerAddrs(m), c.want)
		}
	}
	if seen := up.seen(); len(seen) != 0 {
		t.Fatalf("owned or refused names reached upstream DNS: %v", seen)
	}
	// HTTPS/SVCB for an owned name: no data (their hints would bypass the mapping)
	if m := r.query("db.work.example", dnsmessage.Type(65)); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("HTTPS: %v %d", m.RCode, len(m.Answers))
	}
	// Other types are relayed without address records.
	m := r.query("db.work.example", dnsmessage.TypeTXT)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 || m.Answers[0].Header.Type != dnsmessage.TypeTXT {
		t.Fatalf("TXT relay: %v %+v", m.RCode, m.Answers)
	}
}

func TestDNSUnownedIOSRefusesAndroidForwards(t *testing.T) {
	ios := dnsRig(t, nil, nil)
	if m := ios.query("example.com", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("iOS: %v", m.RCode)
	}
	up := &fakeUpstream{}
	android := dnsRig(t, up, nil)
	m := android.query("example.com", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || !slices.Equal(answerAddrs(m), []netip.Addr{netip.MustParseAddr("93.184.216.34")}) {
		t.Fatalf("android: %v %v", m.RCode, answerAddrs(m))
	}
	if !slices.Equal(up.seen(), []string{"example.com."}) {
		t.Fatalf("upstream saw %v", up.seen())
	}
}

func TestDNSReverse(t *testing.T) {
	up := &fakeUpstream{}
	r := dnsRig(t, up, nil)
	// Tailscale space nobody (or two tailnets) has: answered here, never
	// forwarded.
	for _, n := range []string{"9.0.64.100.in-addr.arpa", "2.0.64.100.in-addr.arpa", "5.0.18.198.in-addr.arpa"} {
		if m := r.query(n, dnsmessage.TypePTR); m.RCode != dnsmessage.RCodeNameError {
			t.Errorf("%s: %v", n, m.RCode)
		}
	}
	if len(up.seen()) != 0 {
		t.Fatalf("reverse names leaked: %v", up.seen())
	}
	if got, ok := reverseAddr(reverseName(netip.MustParseAddr("fd7a:115c:a1e0::5"))); !ok || got != netip.MustParseAddr("fd7a:115c:a1e0::5") {
		t.Fatalf("ip6.arpa round trip: %v %v", got, ok)
	}
}

func TestDNSCacheHonoursTTL(t *testing.T) {
	now := time.Unix(5000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	r := dnsRig(t, nil, clock)
	r.dns.ttl = 20
	r.query("solo.work.example", dnsmessage.TypeA)
	n := r.dns.callCount()
	advance(10 * time.Second)
	m := r.query("solo.work.example", dnsmessage.TypeA)
	if r.dns.callCount() != n {
		t.Fatal("not served from the cache")
	}
	if ttl := m.Answers[0].Header.TTL; ttl != 10 {
		t.Fatalf("cached TTL %d, want 10", ttl)
	}
	advance(11 * time.Second)
	r.query("solo.work.example", dnsmessage.TypeA)
	if r.dns.callCount() != n+1 {
		t.Fatal("expired entry served")
	}
}

func TestDNSOverUDPAndTCPThroughTheTunnel(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.rules.name("work.example", "work", Owned)
	r.rules.addr("100.80.0.5", "work")
	r.dns.set("work", "solo.work.example", "100.80.0.5")
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 9, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("solo.work.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	raw, _ := q.Pack()
	check := func(how string, resp []byte) {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(resp); err != nil || m.ID != 9 || !slices.Equal(answerAddrs(&m), []netip.Addr{netip.MustParseAddr("100.80.0.5")}) {
			t.Fatalf("%s: %v %+v", how, err, m)
		}
	}

	for _, dns := range []netip.Addr{DNSV4, DNSV6} {
		u, err := r.phone.dialUDP(netip.AddrPortFrom(dns, 53))
		if err != nil {
			t.Fatal(err)
		}
		u.SetDeadline(time.Now().Add(5 * time.Second))
		u.Write(raw)
		buf := make([]byte, 1500)
		n, err := u.Read(buf)
		if err != nil {
			t.Fatalf("udp %v: %v", dns, err)
		}
		check("udp "+dns.String(), buf[:n])
		u.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := r.phone.dialTCP(ctx, netip.AddrPortFrom(dns, 53))
		cancel()
		if err != nil {
			t.Fatalf("tcp %v: %v", dns, err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		for i := 0; i < 2; i++ { // two queries on one connection
			out := binary.BigEndian.AppendUint16(nil, uint16(len(raw)))
			c.Write(append(out, raw...))
			var lb [2]byte
			if _, err := io.ReadFull(c, lb[:]); err != nil {
				t.Fatalf("tcp read: %v", err)
			}
			resp := make([]byte, binary.BigEndian.Uint16(lb[:]))
			io.ReadFull(c, resp)
			check("tcp "+dns.String(), resp)
		}
		c.Close()
	}
	if !r.m.tcpDNS.running() {
		t.Fatal("tcp dns stack not running")
	}
}

func TestDNSTruncatesOverUDP(t *testing.T) {
	r := dnsRig(t, nil, nil)
	var many []string
	for i := 0; i < 60; i++ {
		many = append(many, netip.AddrFrom4([4]byte{192, 168, 1, byte(i)}).String())
	}
	r.dns.set("work", "big.work.example", many...)
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 1},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("big.work.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	raw, _ := q.Pack()
	var m dnsmessage.Message
	m.Unpack(r.m.dns.handle(context.Background(), raw, true))
	if !m.Truncated || len(m.Answers) != 0 {
		t.Fatalf("udp: truncated=%v answers=%d", m.Truncated, len(m.Answers))
	}
	m.Unpack(r.m.dns.handle(context.Background(), raw, false))
	if m.Truncated || len(m.Answers) != 60 {
		t.Fatalf("tcp: truncated=%v answers=%d", m.Truncated, len(m.Answers))
	}
}

// Resolvers against a real local server: UDP, then TCP when truncated.
func TestResolversUpstream(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Skip("tcp port taken:", err)
	}
	defer ln.Close()
	answer := func(q []byte, tc bool) []byte {
		var m dnsmessage.Message
		m.Unpack(q)
		m.Response, m.Truncated = true, tc
		if !tc {
			m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 5},
				Body: &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}}}}
		}
		out, _ := m.Pack()
		return out
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(answer(buf[:n], true), a)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var lb [2]byte
			io.ReadFull(c, lb[:])
			q := make([]byte, binary.BigEndian.Uint16(lb[:]))
			io.ReadFull(c, q)
			resp := answer(q, false)
			c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(resp))), resp...))
			c.Close()
		}
	}()
	var res Resolvers
	res.SetServers([]netip.AddrPort{
		netip.MustParseAddrPort("198.18.0.2:53"), // our own: dropped
		netip.MustParseAddrPort("100.100.100.100:53"),
		netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)),
	})
	if s := res.Servers(); len(s) != 1 {
		t.Fatalf("servers %v", s)
	}
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 4242},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	raw, _ := q.Pack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := res.Exchange(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if m.Unpack(resp) != nil || m.Truncated || len(m.Answers) != 1 || m.ID != 4242 {
		t.Fatalf("resp %+v", m)
	}
}

func itoa(n int) string {
	return netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(n)).String()[len("0.0.0.0:"):]
}

func TestTCPDNSStackStopsWhenIdle(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := newRig(t, rigOpts{now: clock, limits: Limits{TCPDNSIdle: time.Minute}})
	r.rules.name("work.example", "work", Owned)
	r.dns.set("work", "a.work.example", "192.168.1.1")
	ask := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := r.phone.dialTCP(ctx, netip.AddrPortFrom(DNSV4, 53))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 3},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("a.work.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
		raw, _ := q.Pack()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(raw))), raw...))
		var lb [2]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			t.Fatal(err)
		}
	}
	ask()
	waitFor(t, func() bool { r.m.tcpDNS.mu.Lock(); defer r.m.tcpDNS.mu.Unlock(); return r.m.tcpDNS.conns == 0 })
	r.m.tcpDNS.maybeStop()
	if !r.m.tcpDNS.running() {
		t.Fatal("stopped while fresh")
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	r.m.tcpDNS.maybeStop()
	if r.m.tcpDNS.running() {
		t.Fatal("idle stack still running")
	}
	ask() // starts again on demand
}
