package packets

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/net/packet"
	"tailscale.com/net/packet/checksum"
	"tailscale.com/types/ipproto"
)

var aLongTimeAgo = time.Unix(1, 0)

// DefaultMTU is the tunnel MTU the shells should set: Tailscale's own tun
// MTU, so a packet from an app always fits a WireGuard packet unfragmented.
const DefaultMTU = 1280

// Limits bound what the packet path may hold. Zero values give the defaults
// in DefaultLimits.
type Limits struct {
	// MaxFlows caps the flow table (TCP connections, UDP associations and
	// ICMP echo ids together). A new flow past it is refused.
	MaxFlows int
	// Idle timeouts. TCPOpening: a SYN the tailnet has not answered.
	// TCPClosing: after a reset, or FIN both ways.
	TCPIdle, TCPOpening, TCPClosing, UDPIdle, ICMPIdle time.Duration
	// NodeQueue: packets queued towards one tailnet's engine; more are
	// dropped (TCP retransmits), like a full NIC queue.
	NodeQueue int
	// DNSConcurrency: queries resolved at once; more are dropped (the client
	// retries).
	DNSConcurrency int
	// DNSCacheEntries caps the tailnet lookup cache.
	DNSCacheEntries int
	// TTL caps for answers: a real Tailscale address (whether it stays
	// unique can change), a synthetic one, and negative answers.
	RealTTL, SyntheticTTL, NegativeTTL time.Duration
	// TCPDNSConns caps DNS-over-TCP connections; TCPDNSIdle shuts the
	// small stack that serves them down when unused.
	TCPDNSConns int
	TCPDNSIdle  time.Duration
}

// DefaultLimits are sized for the iOS extension's 50 MB (DESIGN.md).
var DefaultLimits = Limits{
	MaxFlows:        4096,
	TCPIdle:         4 * time.Hour,
	TCPOpening:      30 * time.Second,
	TCPClosing:      10 * time.Second,
	UDPIdle:         2 * time.Minute,
	ICMPIdle:        30 * time.Second,
	NodeQueue:       256,
	DNSConcurrency:  32,
	DNSCacheEntries: 512,
	RealTTL:         60 * time.Second,
	SyntheticTTL:    300 * time.Second,
	NegativeTTL:     15 * time.Second,
	TCPDNSConns:     16,
	TCPDNSIdle:      2 * time.Minute,
}

func (l *Limits) fill() {
	d := DefaultLimits
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	setD := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&l.MaxFlows, d.MaxFlows)
	set(&l.NodeQueue, d.NodeQueue)
	set(&l.DNSConcurrency, d.DNSConcurrency)
	set(&l.DNSCacheEntries, d.DNSCacheEntries)
	set(&l.TCPDNSConns, d.TCPDNSConns)
	setD(&l.TCPIdle, d.TCPIdle)
	setD(&l.TCPOpening, d.TCPOpening)
	setD(&l.TCPClosing, d.TCPClosing)
	setD(&l.UDPIdle, d.UDPIdle)
	setD(&l.ICMPIdle, d.ICMPIdle)
	setD(&l.RealTTL, d.RealTTL)
	setD(&l.SyntheticTTL, d.SyntheticTTL)
	setD(&l.NegativeTTL, d.NegativeTTL)
	setD(&l.TCPDNSIdle, d.TCPDNSIdle)
}

// Config configures a Mux.
type Config struct {
	Rules    Rules
	DNS      TailnetDNS
	Routes   RouteChecker // optional
	Upstream Upstream     // nil on iOS: unowned names are refused
	Pool     *Pool        // nil: an in-memory pool
	MTU      int          // default DefaultMTU
	Limits   Limits
	Logf     func(format string, args ...any)
	Now      func() time.Time // tests
}

// Stats counts what the packet path did.
type Stats struct {
	PacketsIn, PacketsOut       atomic.Uint64 // from apps, to apps
	Refused                     atomic.Uint64 // new flows refused (RST or ICMP)
	Dropped                     atomic.Uint64 // queue full, unparseable, unsolicited
	Unsolicited                 atomic.Uint64 // from a tailnet with no flow
	DNSQueries, DNSForwarded    atomic.Uint64
	DNSRefused, DNSCacheHits    atomic.Uint64
	DNSDropped, TCPDNSConnsOpen atomic.Uint64
}

// Mux joins the phone's tun to every running tailnet's engine.
type Mux struct {
	cfg   Config
	lim   Limits
	pool  *Pool
	flows *flowTable
	dns   *dnsServer
	stats Stats
	logf  func(string, ...any)
	now   func() time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.RWMutex
	links map[string]*link
	dev   Device

	bufs   sync.Pool
	bufLen int
	dnsSem chan struct{}
	tcpDNS *tcpDNS
	wg     sync.WaitGroup
}

// New returns a Mux. Call Run with the tun device, and Link for each
// tailnet's node before it starts.
func New(cfg Config) (*Mux, error) {
	if cfg.Rules == nil || cfg.DNS == nil {
		return nil, errors.New("packets: Rules and DNS are required")
	}
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	lim := cfg.Limits
	lim.fill()
	pool := cfg.Pool
	if pool == nil {
		pool = NewPool(PoolOptions{Now: cfg.Now})
	}
	m := &Mux{
		cfg: cfg, lim: lim, pool: pool, logf: cfg.Logf, now: cfg.Now,
		links:  map[string]*link{},
		bufLen: max(cfg.MTU, 1280) + 64,
		dnsSem: make(chan struct{}, lim.DNSConcurrency),
	}
	m.flows = newFlowTable(&m.lim, cfg.Now)
	m.dns = &dnsServer{
		rules: cfg.Rules, tdns: cfg.DNS, routes: cfg.Routes, up: cfg.Upstream, pool: pool,
		lim: &m.lim, now: cfg.Now, logf: cfg.Logf, stats: &m.stats, cache: newDNSCache(lim.DNSCacheEntries),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.tcpDNS = &tcpDNS{m: m}
	m.wg.Add(1)
	go m.sweepLoop()
	return m, nil
}

// Stats returns the counters.
func (m *Mux) Stats() *Stats { return &m.stats }

// Flows is the number of open flows.
func (m *Mux) Flows() int { return m.flows.len() }

// Pool is the synthetic address pool.
func (m *Mux) Pool() *Pool { return m.pool }

// FlushDNS drops cached lookups of a tailnet (call when it stops or its
// DNS changes).
func (m *Mux) FlushDNS(tailnet string) { m.dns.cache.flushTailnet(tailnet) }

// Close stops the Mux: it closes every link and the device.
func (m *Mux) Close() {
	m.cancel()
	m.mu.Lock()
	links := m.links
	m.links = map[string]*link{}
	dev := m.dev
	m.mu.Unlock()
	for _, l := range links {
		l.Close()
	}
	if dev != nil {
		dev.Close()
	}
	m.tcpDNS.stop()
	m.wg.Wait()
}

func (m *Mux) sweepLoop() {
	defer m.wg.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.flows.sweep()
			m.tcpDNS.maybeStop()
		}
	}
}

type pbuf struct {
	b []byte
	n int
}

func (m *Mux) getBuf() *pbuf {
	if b, ok := m.bufs.Get().(*pbuf); ok {
		return b
	}
	return &pbuf{b: make([]byte, m.bufLen)}
}

func (m *Mux) putBuf(b *pbuf) { m.bufs.Put(b) }

// Run reads packets from dev until it is closed or Close is called. Only
// one device at a time.
func (m *Mux) Run(dev Device) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("packets: mux closed")
	}
	m.dev = dev
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.dev == dev {
			m.dev = nil
		}
		m.mu.Unlock()
	}()
	for {
		pb := m.getBuf()
		n, err := dev.ReadPacket(pb.b)
		if err != nil {
			m.putBuf(pb)
			if m.ctx.Err() != nil {
				return nil
			}
			return err
		}
		pb.n = n
		m.fromDevice(pb)
	}
}

func (m *Mux) write(pkt []byte) {
	m.mu.RLock()
	dev := m.dev
	m.mu.RUnlock()
	if dev == nil {
		return
	}
	if dev.WritePacket(pkt) == nil {
		m.stats.PacketsOut.Add(1)
	}
}

// appKeyOf is the flow key of an app packet; ok is false for packets that
// never start or belong to a flow.
func appKeyOf(q *packet.Parsed) (appKey, bool) {
	switch q.IPProto {
	case ipproto.TCP, ipproto.UDP:
		return appKey{q.IPProto, q.Src, q.Dst}, true
	case ipproto.ICMPv4, ipproto.ICMPv6:
		if !q.IsEchoRequest() {
			return appKey{}, false
		}
		id := echoID(q)
		return appKey{q.IPProto, netip.AddrPortFrom(q.Src.Addr(), id), netip.AddrPortFrom(q.Dst.Addr(), id)}, true
	}
	return appKey{}, false
}

// fromDevice handles one packet an app sent into the tunnel. It owns pb.
func (m *Mux) fromDevice(pb *pbuf) {
	m.stats.PacketsIn.Add(1)
	pkt := pb.b[:pb.n]
	var q packet.Parsed
	q.Decode(pkt)
	if q.IPVersion != 4 && q.IPVersion != 6 {
		m.drop(pb)
		return
	}
	dst := q.Dst.Addr()
	if dst == DNSV4 || dst == DNSV6 {
		m.dnsPacket(pb, &q)
		return
	}
	if !dst.IsGlobalUnicast() || dst == TunnelV4 || dst == TunnelV6 {
		// Multicast (IPv6 router solicitations and the like), broadcast,
		// link-local, or the tunnel itself: nothing to answer.
		m.drop(pb)
		return
	}
	k, ok := appKeyOf(&q)
	if !ok {
		if q.IPProto != ipproto.ICMPv4 && q.IPProto != ipproto.ICMPv6 && q.IPProto != ipproto.Fragment {
			m.refuse(pkt, &q) // another protocol: say so
		}
		m.drop(pb)
		return
	}
	fin := q.IPProto == ipproto.TCP && q.TCPFlags&packet.TCPFin != 0
	rst := q.IPProto == ipproto.TCP && q.TCPFlags&packet.TCPRst != 0
	f := m.flows.fromApp(k, fin, rst)
	if f == nil {
		if q.IPProto == ipproto.TCP && q.TCPFlags&(packet.TCPSyn|packet.TCPAck) != packet.TCPSyn {
			// Not a connection we know (expired, or its tailnet stopped).
			if !rst {
				m.refuse(pkt, &q)
			}
			m.drop(pb)
			return
		}
		var why string
		f, why = m.newFlow(k, &q)
		if f == nil {
			m.logf("packets: refused %v %v: %s", q.IPProto, q.Dst, why)
			m.refuse(pkt, &q)
			m.putBuf(pb)
			return
		}
	}
	checksum.UpdateSrcAddr(&q, f.self)
	checksum.UpdateDstAddr(&q, f.real)
	if !f.link.enqueue(pb) {
		m.drop(pb)
	}
}

func (m *Mux) drop(pb *pbuf) {
	m.stats.Dropped.Add(1)
	m.putBuf(pb)
}

// refuse answers a packet whose destination nothing may reach: a TCP reset,
// or ICMP "administratively prohibited".
func (m *Mux) refuse(pkt []byte, q *packet.Parsed) {
	m.stats.Refused.Add(1)
	var out []byte
	if q.IPProto == ipproto.TCP {
		out = tcpReset(q)
	} else {
		out = icmpUnreachable(pkt, q)
	}
	if out != nil {
		m.write(out)
	}
}

// newFlow decides where a new flow goes and records it.
func (m *Mux) newFlow(k appKey, q *packet.Parsed) (*flow, string) {
	dst := k.dst.Addr()
	var tailnet string
	var real netip.Addr
	switch {
	case m.pool.Contains(dst):
		t, r, ok := m.pool.Lookup(dst)
		if !ok {
			return nil, "synthetic address not in use"
		}
		tailnet, real = t, r
	case IsTailscaleIP(dst):
		t, v := m.cfg.Rules.ClassifyAddr(dst)
		if v != Owned {
			return nil, v.String()
		}
		tailnet, real = t, dst
	default:
		return nil, "not a tailnet address"
	}
	if real.Is4() != dst.Is4() {
		return nil, "address family mismatch"
	}
	m.mu.RLock()
	l := m.links[tailnet]
	m.mu.RUnlock()
	if l == nil {
		return nil, "tailnet " + tailnet + " is not running"
	}
	self := l.self(real.Is4())
	if !self.IsValid() {
		return nil, "tailnet " + tailnet + " has no address of that family yet"
	}
	f := &flow{app: k, link: l, real: real, self: self}
	f.node = nodeKey{tailnet: tailnet, proto: k.proto, peer: netip.AddrPortFrom(real, k.dst.Port()), local: k.src.Port()}
	if k.proto == ipproto.ICMPv4 || k.proto == ipproto.ICMPv6 {
		f.node.peer = netip.AddrPortFrom(real, 0)
	}
	if err := m.flows.insert(f); err != nil {
		return nil, err.Error()
	}
	return f, ""
}

// fromNode handles one packet a tailnet's engine delivers to this device.
// pkt may be modified.
func (m *Mux) fromNode(l *link, pkt []byte) {
	var q packet.Parsed
	q.Decode(pkt)
	if q.IPVersion != 4 && q.IPVersion != 6 {
		m.stats.Dropped.Add(1)
		return
	}
	if q.IsError() {
		m.icmpErrorFromNode(l, pkt, &q)
		return
	}
	var k nodeKey
	switch q.IPProto {
	case ipproto.TCP, ipproto.UDP:
		k = nodeKey{l.tailnet, q.IPProto, q.Src, q.Dst.Port()}
	case ipproto.ICMPv4, ipproto.ICMPv6:
		if !q.IsEchoResponse() {
			m.stats.Dropped.Add(1)
			return
		}
		k = nodeKey{l.tailnet, q.IPProto, netip.AddrPortFrom(q.Src.Addr(), 0), echoID(&q)}
	default:
		m.stats.Dropped.Add(1)
		return
	}
	fin := q.IPProto == ipproto.TCP && q.TCPFlags&packet.TCPFin != 0
	rst := q.IPProto == ipproto.TCP && q.TCPFlags&packet.TCPRst != 0
	f := m.flows.fromNode(k, fin, rst)
	if f == nil || f.link != l || q.Dst.Addr() != f.self {
		// Nothing on the phone asked for this: inbound connections from a
		// tailnet are not accepted in this mode.
		m.stats.Unsolicited.Add(1)
		m.stats.Dropped.Add(1)
		return
	}
	checksum.UpdateSrcAddr(&q, f.app.dst.Addr())
	checksum.UpdateDstAddr(&q, f.app.src.Addr())
	m.write(pkt)
}

// icmpErrorFromNode passes an ICMP error (port unreachable, packet too big,
// time exceeded) about one of our flows back to the app, with the outer and
// the quoted inner addresses translated.
func (m *Mux) icmpErrorFromNode(l *link, pkt []byte, q *packet.Parsed) {
	hl := 20
	if q.IPVersion == 4 {
		hl = int(pkt[0]&0x0f) * 4
	} else {
		hl = 40
	}
	if len(pkt) < hl+8 {
		m.stats.Dropped.Add(1)
		return
	}
	inner := pkt[hl+8:]
	var iq packet.Parsed
	iq.Decode(inner)
	var proto ipproto.Proto
	var isrc, idst netip.Addr
	var sport, dport uint16
	ihl := 0
	switch {
	case q.IPVersion == 4 && len(inner) >= 28 && inner[0]>>4 == 4:
		ihl = int(inner[0]&0x0f) * 4
		if len(inner) < ihl+8 {
			m.stats.Dropped.Add(1)
			return
		}
		proto = ipproto.Proto(inner[9])
		isrc, idst = netip.AddrFrom4([4]byte(inner[12:16])), netip.AddrFrom4([4]byte(inner[16:20]))
	case q.IPVersion == 6 && len(inner) >= 48 && inner[0]>>4 == 6:
		ihl = 40
		proto = ipproto.Proto(inner[6])
		isrc, idst = netip.AddrFrom16([16]byte(inner[8:24])), netip.AddrFrom16([16]byte(inner[24:40]))
	default:
		m.stats.Dropped.Add(1)
		return
	}
	if proto != ipproto.TCP && proto != ipproto.UDP {
		m.stats.Dropped.Add(1)
		return
	}
	sport = binary.BigEndian.Uint16(inner[ihl:])
	dport = binary.BigEndian.Uint16(inner[ihl+2:])
	f := m.flows.fromNode(nodeKey{l.tailnet, proto, netip.AddrPortFrom(idst, dport), sport}, false, false)
	if f == nil || isrc != f.self {
		m.stats.Unsolicited.Add(1)
		m.stats.Dropped.Add(1)
		return
	}
	// The quoted packet: source self -> the app's address, destination real
	// -> what the app sent to. Its own checksums are left (the kernel only
	// matches addresses and ports in a quote).
	appSrc, appDst := f.app.src.Addr(), f.app.dst.Addr()
	if q.IPVersion == 4 {
		s, d := appSrc.As4(), appDst.As4()
		copy(inner[12:16], s[:])
		copy(inner[16:20], d[:])
		inner[10], inner[11] = 0, 0
		binary.BigEndian.PutUint16(inner[10:], fold(sum16(inner[:ihl], 0)))
	} else {
		s, d := appSrc.As16(), appDst.As16()
		copy(inner[8:24], s[:])
		copy(inner[24:40], d[:])
	}
	// The outer packet: from the peer (or whatever hop reported it, shown as
	// the peer) to the app.
	outSrc := appDst
	if q.IPVersion == 4 {
		s, d := outSrc.As4(), appSrc.As4()
		copy(pkt[12:16], s[:])
		copy(pkt[16:20], d[:])
		pkt[10], pkt[11] = 0, 0
		binary.BigEndian.PutUint16(pkt[10:], fold(sum16(pkt[:hl], 0)))
		icmp := pkt[hl:]
		icmp[2], icmp[3] = 0, 0
		binary.BigEndian.PutUint16(icmp[2:], fold(sum16(icmp, 0)))
	} else {
		s, d := outSrc.As16(), appSrc.As16()
		copy(pkt[8:24], s[:])
		copy(pkt[24:40], d[:])
		icmp := pkt[hl:]
		icmp[2], icmp[3] = 0, 0
		binary.BigEndian.PutUint16(icmp[2:], fold(sum16(icmp, pseudo(outSrc, appSrc, ipproto.ICMPv6, len(icmp)))))
	}
	m.write(pkt)
}

// dnsPacket handles a packet to the DNS address. It owns pb.
func (m *Mux) dnsPacket(pb *pbuf, q *packet.Parsed) {
	switch {
	case q.IPProto == ipproto.UDP && q.Dst.Port() == 53:
		payload := append([]byte(nil), q.Payload()...)
		src, dst := q.Src, q.Dst
		m.putBuf(pb)
		select {
		case m.dnsSem <- struct{}{}:
		default:
			m.stats.DNSDropped.Add(1)
			return
		}
		go func() {
			defer func() { <-m.dnsSem }()
			resp := m.dns.handle(m.ctx, payload, true)
			if resp != nil {
				m.write(buildUDP(dst, src, resp))
			}
		}()
	case q.IPProto == ipproto.TCP && q.Dst.Port() == 53:
		pkt := append([]byte(nil), pb.b[:pb.n]...)
		m.putBuf(pb)
		m.tcpDNS.inject(pkt)
	default:
		m.refuse(pb.b[:pb.n], q)
		m.putBuf(pb)
	}
}

// echoID is an ICMP echo's identifier (packet.Parsed.EchoIDSeq is little
// endian and meant for Tailscale's own lookups).
func echoID(q *packet.Parsed) uint16 {
	t := q.Transport()
	if len(t) < 8 {
		return 0
	}
	return binary.BigEndian.Uint16(t[4:])
}
