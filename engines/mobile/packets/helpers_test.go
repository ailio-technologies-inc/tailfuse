package packets

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// fakeRules: names by suffix, addresses by exact match.
type fakeRules struct {
	mu    sync.Mutex
	names map[string]struct {
		tailnet string
		v       Verdict
	}
	addrs map[netip.Addr][]string // tailnets with a device there
	run   map[string]bool         // running tailnets (addresses only count for these)
}

func newRules() *fakeRules {
	return &fakeRules{names: map[string]struct {
		tailnet string
		v       Verdict
	}{}, addrs: map[netip.Addr][]string{}, run: map[string]bool{}}
}

func (r *fakeRules) name(suffix, tailnet string, v Verdict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names[suffix] = struct {
		tailnet string
		v       Verdict
	}{tailnet, v}
}

func (r *fakeRules) addr(a string, tailnets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs[netip.MustParseAddr(a)] = tailnets
}

func (r *fakeRules) ClassifyName(name string) (string, Verdict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	best, bestLen := "", -1
	var v Verdict
	for s, o := range r.names {
		if (name == s || strings.HasSuffix(name, "."+s)) && len(s) > bestLen {
			best, v, bestLen = o.tailnet, o.v, len(s)
		}
	}
	if bestLen < 0 {
		return "", Unowned
	}
	return best, v
}

func (r *fakeRules) ClassifyAddr(ip netip.Addr) (string, Verdict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.addrs[ip]
	switch {
	case len(ts) == 1:
		return ts[0], Owned
	case len(ts) > 1:
		return "", Conflict
	case IsTailscaleIP(ip):
		return "", TailnetSpace
	}
	return "", Unowned
}

// fakeDNS answers A/AAAA from a table; a paused tailnet fails.
type fakeDNS struct {
	mu      sync.Mutex
	records map[string]map[string][]netip.Addr // tailnet -> name -> addrs
	paused  map[string]bool
	ttl     uint32
	calls   int
}

func newDNS() *fakeDNS {
	return &fakeDNS{records: map[string]map[string][]netip.Addr{}, paused: map[string]bool{}, ttl: 600}
}

func (d *fakeDNS) set(tailnet, name string, addrs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.records[tailnet] == nil {
		d.records[tailnet] = map[string][]netip.Addr{}
	}
	var as []netip.Addr
	for _, a := range addrs {
		as = append(as, netip.MustParseAddr(a))
	}
	d.records[tailnet][name] = as
}

func (d *fakeDNS) QueryDNS(ctx context.Context, tailnet, name, qtype string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.paused[tailnet] {
		return nil, errors.New("tailnet " + tailnet + " is not running")
	}
	name = strings.TrimSuffix(name, ".")
	n := dnsmessage.MustNewName(name + ".")
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeSuccess}}
	var qt dnsmessage.Type
	switch qtype {
	case "A":
		qt = dnsmessage.TypeA
	case "AAAA":
		qt = dnsmessage.TypeAAAA
	case "TXT":
		qt = dnsmessage.TypeTXT
	default:
		qt = dnsmessage.TypePTR
	}
	m.Questions = []dnsmessage.Question{{Name: n, Type: qt, Class: dnsmessage.ClassINET}}
	addrs, ok := d.records[tailnet][name]
	if !ok {
		m.RCode = dnsmessage.RCodeNameError
	}
	rh := dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: d.ttl}
	if ok && qt == dnsmessage.TypeTXT {
		m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.TXTResource{TXT: []string{"hello"}}})
		// an address record mixed in: must be dropped by relay
		m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AResource{A: [4]byte{100, 64, 0, 9}}})
	}
	for _, a := range addrs {
		if a.Is4() && qt == dnsmessage.TypeA {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AResource{A: a.As4()}})
		}
		if a.Is6() && qt == dnsmessage.TypeAAAA {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return m.Pack()
}

func (d *fakeDNS) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// fakeUpstream records forwarded queries.
type fakeUpstream struct {
	mu    sync.Mutex
	names []string
}

func (u *fakeUpstream) Exchange(ctx context.Context, query []byte) ([]byte, error) {
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.names = append(u.names, m.Questions[0].Name.String())
	u.mu.Unlock()
	m.Response = true
	m.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 30},
		Body:   &dnsmessage.AResource{A: [4]byte{93, 184, 216, 34}},
	}}
	return m.Pack()
}

func (u *fakeUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.names...)
}

// testStack is a gVisor stack standing in for the phone's kernel (behind
// the tun) or for a tailnet's peers (behind a link).
type testStack struct {
	s  *stack.Stack
	ep *channel.Endpoint
}

func newTestStack(t testing.TB, addrs ...netip.Addr) *testStack {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
	})
	ep := channel.New(512, DefaultMTU, "")
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		proto := ipv4.ProtocolNumber
		if a.Is6() {
			proto = ipv6.ProtocolNumber
		}
		pa := tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: tcpip.AddrFromSlice(a.AsSlice()).WithPrefix()}
		if err := s.AddProtocolAddress(1, pa, stack.AddressProperties{}); err != nil {
			t.Fatal(err)
		}
	}
	any4, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes(make([]byte, 4)))
	any6, _ := tcpip.NewSubnet(tcpip.AddrFrom16([16]byte{}), tcpip.MaskFromBytes(make([]byte, 16)))
	s.SetRouteTable([]tcpip.Route{{Destination: any4, NIC: 1}, {Destination: any6, NIC: 1}})
	ts := &testStack{s: s, ep: ep}
	t.Cleanup(func() { ep.Close(); s.Close(); s.Wait() })
	return ts
}

func (ts *testStack) inject(pkt []byte) {
	proto := header.IPv4ProtocolNumber
	if pkt[0]>>4 == 6 {
		proto = header.IPv6ProtocolNumber
	}
	pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), pkt...))})
	ts.ep.InjectInbound(proto, pb)
	pb.DecRef()
}

// out runs f for every packet the stack sends, until ctx ends.
func (ts *testStack) out(ctx context.Context, f func([]byte)) {
	go func() {
		for {
			pkt := ts.ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			f(append([]byte(nil), v.AsSlice()...))
			v.Release()
			pkt.DecRef()
		}
	}()
}

func fullAddr(ap netip.AddrPort) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	proto := ipv4.ProtocolNumber
	if ap.Addr().Is6() {
		proto = ipv6.ProtocolNumber
	}
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}, proto
}

func (ts *testStack) dialTCP(ctx context.Context, ap netip.AddrPort) (*gonet.TCPConn, error) {
	fa, proto := fullAddr(ap)
	return gonet.DialContextTCP(ctx, ts.s, fa, proto)
}

func (ts *testStack) dialUDP(ap netip.AddrPort) (*gonet.UDPConn, error) {
	fa, proto := fullAddr(ap)
	return gonet.DialUDP(ts.s, nil, &fa, proto)
}

func (ts *testStack) listenTCP(t testing.TB, ap netip.AddrPort) net.Listener {
	fa, proto := fullAddr(ap)
	ln, err := gonet.ListenTCP(ts.s, fa, proto)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func (ts *testStack) listenUDP(t testing.TB, ap netip.AddrPort) *gonet.UDPConn {
	fa, proto := fullAddr(ap)
	c, err := gonet.DialUDP(ts.s, &fa, nil, proto)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// rig is a Mux with a phone stack on its device and a peer stack per
// tailnet on its links.
type rig struct {
	t     testing.TB
	m     *Mux
	dev   *MemDevice
	phone *testStack
	peers map[string]*testStack
	rules *fakeRules
	dns   *fakeDNS
	ctx   context.Context
}

type rigOpts struct {
	upstream Upstream
	limits   Limits
	now      func() time.Time
	pool     *Pool
	noPhone  bool // raw packets only: read dev.Out() directly
}

func newRig(t testing.TB, o rigOpts) *rig {
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, rules: newRules(), dns: newDNS(), peers: map[string]*testStack{}, ctx: ctx}
	m, err := New(Config{Rules: r.rules, DNS: r.dns, Upstream: o.upstream, Limits: o.limits, Now: o.now, Pool: o.pool, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	r.dev = NewMemDevice(1024)
	done := make(chan struct{})
	go func() { defer close(done); m.Run(r.dev) }()
	t.Cleanup(func() { cancel(); m.Close(); <-done })
	if !o.noPhone {
		r.phone = newTestStack(t, TunnelV4, TunnelV6)
		r.phone.out(ctx, func(p []byte) { r.dev.Inject(p) })
		go func() {
			for {
				select {
				case p := <-r.dev.Out():
					r.phone.inject(p)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	return r
}

// tailnet attaches a tailnet: its node's own addresses and a peer stack
// holding the given device addresses.
func (r *rig) tailnet(name string, self4, self6 string, peers ...string) *testStack {
	dev := r.m.Link(name)
	r.m.SetAddrs(name, []netip.Addr{netip.MustParseAddr(self4), netip.MustParseAddr(self6)})
	var addrs []netip.Addr
	for _, p := range peers {
		addrs = append(addrs, netip.MustParseAddr(p))
	}
	ps := newTestStack(r.t, addrs...)
	r.peers[name] = ps
	ps.out(r.ctx, func(p []byte) { dev.Write([][]byte{p}, 0) })
	go func() {
		slab := make([]byte, 4096)
		pk := make([]tun.ReadPacket, 1)
		for {
			n, err := dev.Read(slab, pk)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				ps.inject(slab[pk[i].Offset : pk[i].Offset+pk[i].Size])
			}
		}
	}()
	r.t.Cleanup(func() { dev.Close() })
	return ps
}

// query sends a DNS question straight to the responder.
func (r *rig) query(name string, qt dnsmessage.Type) *dnsmessage.Message {
	r.t.Helper()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 77, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name + "."), Type: qt, Class: dnsmessage.ClassINET}}}
	raw, err := q.Pack()
	if err != nil {
		r.t.Fatal(err)
	}
	resp := r.m.dns.handle(context.Background(), raw, true)
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		r.t.Fatalf("unpack %s: %v", name, err)
	}
	if m.ID != 77 {
		r.t.Fatalf("id %d", m.ID)
	}
	return &m
}

func answerAddrs(m *dnsmessage.Message) []netip.Addr {
	var out []netip.Addr
	for _, a := range m.Answers {
		switch b := a.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A))
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA))
		}
	}
	return out
}
