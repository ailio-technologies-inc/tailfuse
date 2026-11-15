package packets

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/net/packet"
	"tailscale.com/types/ipproto"
)

// twoTailnets: "work" and "home" both have a device at 100.64.0.2 (each
// tailnet hands out its own 100.x addresses); "home" alone has 100.100.1.5.
// The phone is 100.64.0.1 on work and 100.70.0.1 on home.
func twoTailnets(t *testing.T, o rigOpts) (*rig, *testStack, *testStack) {
	r := newRig(t, o)
	work := r.tailnet("work", "100.64.0.1", "fd7a:115c:a1e0::1", "100.64.0.2", "fd7a:115c:a1e0::2")
	home := r.tailnet("home", "100.70.0.1", "fd7a:115c:a1e0::71", "100.64.0.2", "100.100.1.5")
	r.rules.name("work.example", "work", Owned)
	r.rules.name("home.example", "home", Owned)
	r.rules.addr("100.64.0.2", "work", "home")
	r.rules.addr("fd7a:115c:a1e0::2", "work")
	r.rules.addr("100.100.1.5", "home")
	r.dns.set("work", "db.work.example", "100.64.0.2", "fd7a:115c:a1e0::2")
	r.dns.set("home", "pi.home.example", "100.64.0.2")
	r.dns.set("home", "nas.home.example", "100.100.1.5")
	return r, work, home
}

// serveEcho answers each TCP connection with prefix + what it reads.
func serveEcho(t *testing.T, s *testStack, ap string, prefix string) {
	ln := s.listenTCP(t, netip.MustParseAddrPort(ap))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte(prefix))
				io.Copy(c, c)
			}()
		}
	}()
}

func resolve(t *testing.T, r *rig, name string, qt dnsmessage.Type) netip.Addr {
	t.Helper()
	as := answerAddrs(r.query(name, qt))
	if len(as) != 1 {
		t.Fatalf("%s: %v", name, as)
	}
	return as[0]
}

func roundTrip(t *testing.T, r *rig, to netip.AddrPort, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := r.phone.dialTCP(ctx, to)
	if err != nil {
		t.Fatalf("dial %v: %v", to, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	hello := make([]byte, len(want))
	if _, err := io.ReadFull(c, hello); err != nil || string(hello) != want {
		t.Fatalf("%v greeted %q (%v), want %q", to, hello, err, want)
	}
	// a bigger payload both ways, so it spans many packets
	payload := bytes.Repeat([]byte("tailfuse"), 64<<10/8)
	go c.Write(payload)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("%v echo: %v", to, err)
	}
}

func TestTCPThroughTheRightTailnet(t *testing.T) {
	r, work, home := twoTailnets(t, rigOpts{})
	serveEcho(t, work, "100.64.0.2:22", "work-db ")
	serveEcho(t, work, "[fd7a:115c:a1e0::2]:22", "work-db6 ")
	serveEcho(t, home, "100.64.0.2:22", "home-pi ")
	serveEcho(t, home, "100.100.1.5:445", "home-nas ")

	// Same real address on two tailnets: by name, each gets its own
	// synthetic address and reaches its own device.
	db := resolve(t, r, "db.work.example", dnsmessage.TypeA)
	pi := resolve(t, r, "pi.home.example", dnsmessage.TypeA)
	roundTrip(t, r, netip.AddrPortFrom(db, 22), "work-db ")
	roundTrip(t, r, netip.AddrPortFrom(pi, 22), "home-pi ")
	// A uniquely owned address works by name and as a literal.
	nas := resolve(t, r, "nas.home.example", dnsmessage.TypeA)
	if nas != netip.MustParseAddr("100.100.1.5") {
		t.Fatalf("nas answered %v, want its real address", nas)
	}
	roundTrip(t, r, netip.MustParseAddrPort("100.100.1.5:445"), "home-nas ")
	// IPv6, real address.
	roundTrip(t, r, netip.MustParseAddrPort("[fd7a:115c:a1e0::2]:22"), "work-db6 ")
	if r.m.Flows() < 4 {
		t.Fatalf("flows %d", r.m.Flows())
	}
}

func TestTCPRefusedDestinations(t *testing.T) {
	r, _, _ := twoTailnets(t, rigOpts{})
	for _, to := range []string{
		"100.64.0.2:22",   // two tailnets have it: refused, never guessed
		"100.64.0.99:22",  // nobody has it
		"198.18.77.77:22", // synthetic address not in use
		"[fd7a:115c:a1e0::99]:22",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		start := time.Now()
		c, err := r.phone.dialTCP(ctx, netip.MustParseAddrPort(to))
		cancel()
		if err == nil {
			c.Close()
			t.Fatalf("%s: connected", to)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !bytes.Contains([]byte(err.Error()), []byte("refused")) {
			t.Fatalf("%s: %v, want connection refused", to, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("%s: refusal took %v", to, time.Since(start))
		}
	}
	if r.m.Stats().Refused.Load() < 4 {
		t.Fatalf("refused %d", r.m.Stats().Refused.Load())
	}
}

func TestUDPRoundTrip(t *testing.T) {
	r, work, _ := twoTailnets(t, rigOpts{})
	srv := work.listenUDP(t, netip.MustParseAddrPort("100.64.0.2:5353"))
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := srv.ReadFrom(buf)
			if err != nil {
				return
			}
			srv.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	db := resolve(t, r, "db.work.example", dnsmessage.TypeA)
	c, err := r.phone.dialUDP(netip.AddrPortFrom(db, 5353))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 3; i++ {
		c.Write([]byte("ping"))
		buf := make([]byte, 100)
		n, err := c.Read(buf)
		if err != nil || string(buf[:n]) != "echo:ping" {
			t.Fatalf("udp: %q %v", buf[:n], err)
		}
	}
	if r.m.Flows() != 1 {
		t.Fatalf("one association, %d flows", r.m.Flows())
	}
}

// Raw packets, no phone stack: what exactly comes back.
func rawRig(t *testing.T, o rigOpts) (*rig, *testStack) {
	o.noPhone = true
	r := newRig(t, o)
	work := r.tailnet("work", "100.64.0.1", "fd7a:115c:a1e0::1", "100.64.0.2")
	r.rules.addr("100.64.0.2", "work")
	return r, work
}

func udpPacket(src, dst string, payload string) []byte {
	return buildUDP(netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst), []byte(payload))
}

func synPacket(src, dst string) []byte {
	s, d := netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst)
	hl := ipHeaderLen(s.Addr())
	b := make([]byte, hl+20)
	ipHeader(b, s.Addr(), d.Addr(), ipproto.TCP, 20)
	h := b[hl:]
	binary.BigEndian.PutUint16(h[0:], s.Port())
	binary.BigEndian.PutUint16(h[2:], d.Port())
	binary.BigEndian.PutUint32(h[4:], 1000)
	h[12] = 5 << 4
	h[13] = byte(packet.TCPSyn)
	binary.BigEndian.PutUint16(h[14:], 65535)
	binary.BigEndian.PutUint16(h[16:], fold(sum16(h, pseudo(s.Addr(), d.Addr(), ipproto.TCP, 20))))
	return b
}

func ackPacket(src, dst string) []byte {
	b := synPacket(src, dst)
	hl := ipHeaderLen(netip.MustParseAddrPort(src).Addr())
	h := b[hl:]
	h[13] = byte(packet.TCPAck)
	h[16], h[17] = 0, 0
	s, d := netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst)
	binary.BigEndian.PutUint16(h[16:], fold(sum16(h, pseudo(s.Addr(), d.Addr(), ipproto.TCP, 20))))
	return b
}

func echoRequest(src, dst string, id uint16) []byte {
	s, d := netip.MustParseAddr(src), netip.MustParseAddr(dst)
	b := make([]byte, 20+16)
	ipHeader(b, s, d, ipproto.ICMPv4, 16)
	m := b[20:]
	m[0] = 8
	binary.BigEndian.PutUint16(m[4:], id)
	binary.BigEndian.PutUint16(m[6:], 1)
	copy(m[8:], "pingpong")
	binary.BigEndian.PutUint16(m[2:], fold(sum16(m, 0)))
	return b
}

func next(t *testing.T, r *rig) *packet.Parsed {
	t.Helper()
	select {
	case p := <-r.dev.Out():
		var q packet.Parsed
		q.Decode(p)
		return &q
	case <-time.After(3 * time.Second):
		t.Fatal("no packet from the mux")
	}
	return nil
}

func TestRawRefusals(t *testing.T) {
	r, _ := rawRig(t, rigOpts{})
	// UDP to an address nobody has: ICMP administratively prohibited, from
	// that address.
	r.dev.Inject(udpPacket("198.18.0.1:4000", "100.64.0.50:53", "x"))
	q := next(t, r)
	if q.IPProto != ipproto.ICMPv4 || q.Src.Addr() != netip.MustParseAddr("100.64.0.50") || q.Dst.Addr() != TunnelV4 {
		t.Fatalf("got %v", q)
	}
	if h := q.Transport(); h[0] != 3 || h[1] != 13 || fold(sum16(h, 0)) != 0 {
		t.Fatalf("icmp type %d code %d", h[0], h[1])
	}
	// TCP SYN: RST|ACK acking the SYN.
	r.dev.Inject(synPacket("198.18.0.1:4001", "100.64.0.50:80"))
	q = next(t, r)
	if q.IPProto != ipproto.TCP || q.TCPFlags != packet.TCPRst|packet.TCPAck || q.Dst.Port() != 4001 {
		t.Fatalf("got %v flags %v", q, q.TCPFlags)
	}
	if ack := binary.BigEndian.Uint32(q.Transport()[8:]); ack != 1001 {
		t.Fatalf("rst acks %d", ack)
	}
	// A stray ACK for no known flow: RST with its ack as seq.
	r.dev.Inject(ackPacket("198.18.0.1:4002", "100.64.0.2:80"))
	q = next(t, r)
	if q.TCPFlags != packet.TCPRst {
		t.Fatalf("stray ack got %v", q.TCPFlags)
	}
	// v6 UDP to unowned Tailscale space
	r.dev.Inject(udpPacket("[fd5e:9b07:4a1c::1]:4000", "[fd7a:115c:a1e0::50]:53", "x"))
	q = next(t, r)
	if q.IPProto != ipproto.ICMPv6 || q.Transport()[0] != 1 {
		t.Fatalf("v6 got %v", q)
	}
	if fold(sum16(q.Transport(), pseudo(q.Src.Addr(), q.Dst.Addr(), ipproto.ICMPv6, len(q.Transport())))) != 0 {
		t.Fatal("bad icmpv6 checksum")
	}
}

func TestICMPEchoThroughTailnet(t *testing.T) {
	r, _ := rawRig(t, rigOpts{})
	r.dev.Inject(echoRequest("198.18.0.1", "100.64.0.2", 7))
	q := next(t, r)
	if !q.IsEchoResponse() || q.Src.Addr() != netip.MustParseAddr("100.64.0.2") || q.Dst.Addr() != TunnelV4 || echoID(q) != 7 {
		t.Fatalf("got %v %x", q, q.Transport())
	}
	if fold(sum16(q.Transport(), 0)) != 0 {
		t.Fatal("bad icmp checksum after translation")
	}
}

func TestUDPPortUnreachableFromPeerIsTranslated(t *testing.T) {
	r, _ := rawRig(t, rigOpts{})
	// No UDP listener on the peer: the peer's stack answers ICMP port
	// unreachable, which must reach the app with the app's addresses.
	r.dev.Inject(udpPacket("198.18.0.1:4500", "100.64.0.2:9999", "hello"))
	q := next(t, r)
	if q.IPProto != ipproto.ICMPv4 || q.Dst.Addr() != TunnelV4 || q.Src.Addr() != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("got %v", q)
	}
	h := q.Transport()
	if h[0] != 3 || h[1] != 3 {
		t.Fatalf("type %d code %d", h[0], h[1])
	}
	inner := h[8:]
	if netip.AddrFrom4([4]byte(inner[12:16])) != TunnelV4 || binary.BigEndian.Uint16(inner[20:]) != 4500 {
		t.Fatalf("inner not translated: src %v", netip.AddrFrom4([4]byte(inner[12:16])))
	}
}

func TestFlowLimit(t *testing.T) {
	r, work := rawRig(t, rigOpts{limits: Limits{MaxFlows: 2}})
	_ = work
	r.dev.Inject(udpPacket("198.18.0.1:1", "100.64.0.2:7", "a"))
	r.dev.Inject(udpPacket("198.18.0.1:2", "100.64.0.2:7", "b"))
	waitFor(t, func() bool { return r.m.Flows() == 2 })
	drain(r)
	r.dev.Inject(synPacket("198.18.0.1:3", "100.64.0.2:80"))
	q := next(t, r)
	for q.IPProto != ipproto.TCP { // port unreachables for the UDP packets may come first
		q = next(t, r)
	}
	if q.TCPFlags&packet.TCPRst == 0 {
		t.Fatalf("over the limit: %v", q.TCPFlags)
	}
	if r.m.Flows() != 2 {
		t.Fatalf("flows %d", r.m.Flows())
	}
}

func TestIdleTimeout(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r, _ := rawRig(t, rigOpts{now: clock, limits: Limits{UDPIdle: time.Minute, MaxFlows: 1}})
	r.dev.Inject(udpPacket("198.18.0.1:1", "100.64.0.2:7", "a"))
	waitFor(t, func() bool { return r.m.Flows() == 1 })
	mu.Lock()
	now = now.Add(30 * time.Second)
	mu.Unlock()
	if r.m.flows.sweep() != 0 {
		t.Fatal("swept a live flow")
	}
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	// The table is full (MaxFlows 1) but the old flow has expired: a new
	// one takes its place.
	r.dev.Inject(udpPacket("198.18.0.1:2", "100.64.0.2:7", "b"))
	waitFor(t, func() bool {
		r.m.flows.mu.Lock()
		defer r.m.flows.mu.Unlock()
		_, ok := r.m.flows.byApp[appKey{ipproto.UDP, netip.MustParseAddrPort("198.18.0.1:2"), netip.MustParseAddrPort("100.64.0.2:7")}]
		return ok && len(r.m.flows.byApp) == 1
	})
}

func TestUnsolicitedAndStoppedTailnet(t *testing.T) {
	r, _ := rawRig(t, rigOpts{})
	// A packet from the tailnet nobody on the phone asked for: dropped.
	r.m.mu.RLock()
	l := r.m.links["work"]
	r.m.mu.RUnlock()
	l.Write([][]byte{udpPacket("100.64.0.2:7", "100.64.0.1:999", "hi")}, 0)
	if r.m.Stats().Unsolicited.Load() != 1 {
		t.Fatal("unsolicited packet not dropped")
	}
	select {
	case p := <-r.dev.Out():
		t.Fatalf("unsolicited packet delivered: %x", p)
	default:
	}
	// A flow, then the node stops: its flows go and new ones are refused.
	r.dev.Inject(udpPacket("198.18.0.1:1", "100.64.0.2:7", "a"))
	waitFor(t, func() bool { return r.m.Flows() == 1 })
	l.Close()
	if r.m.Flows() != 0 {
		t.Fatal("flows survived their link")
	}
	drain(r)
	r.dev.Inject(synPacket("198.18.0.1:5", "100.64.0.2:80"))
	if q := next(t, r); q.TCPFlags&packet.TCPRst == 0 {
		t.Fatalf("stopped tailnet: %v", q.TCPFlags)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func drain(r *rig) {
	time.Sleep(50 * time.Millisecond)
	for {
		select {
		case <-r.dev.Out():
		default:
			return
		}
	}
}
