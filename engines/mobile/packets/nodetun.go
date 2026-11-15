package packets

import (
	"io"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"

	"github.com/tailscale/wireguard-go/tun"
)

// link is one tailnet's side of the Mux: the tun.Device handed to that
// tailnet's tsnet.Server (Server.Tun). WireGuard reads the packets apps send
// to the tailnet from it, and writes the tailnet's packets for this device
// to it.
//
// With Server.Tun set, tsnet still keeps its own netstack for Server.Dial
// and Server.Listen (the proxy path keeps working) and passes it only the
// packets of its own connections; everything else comes here.
type link struct {
	m       *Mux
	tailnet string
	in      chan *pbuf
	events  chan tun.Event
	done    chan struct{}
	once    sync.Once
	self4   atomic.Pointer[netip.Addr]
	self6   atomic.Pointer[netip.Addr]
}

var _ tun.Device = (*link)(nil)

// Link returns a new tun.Device for tailnet's node, to set as
// tsnet.Server.Tun before the node starts. It replaces (and closes) the
// tailnet's previous link. tsnet closes it when the node closes.
func (m *Mux) Link(tailnet string) tun.Device {
	l := &link{
		m: m, tailnet: tailnet,
		in:     make(chan *pbuf, m.lim.NodeQueue),
		events: make(chan tun.Event, 1),
		done:   make(chan struct{}),
	}
	l.events <- tun.EventUp
	m.mu.Lock()
	old := m.links[tailnet]
	m.links[tailnet] = l
	m.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return l
}

// SetAddrs gives a tailnet's node its own Tailscale addresses (from
// tsnet.Server.TailscaleIPs once it is up, and again when they change).
// Until then the tailnet's flows are refused.
func (m *Mux) SetAddrs(tailnet string, addrs []netip.Addr) {
	m.mu.RLock()
	l := m.links[tailnet]
	m.mu.RUnlock()
	if l == nil {
		return
	}
	var v4, v6 netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() && !v4.IsValid() {
			v4 = a
		} else if a.Is6() && !v6.IsValid() {
			v6 = a
		}
	}
	old4, old6 := l.self(true), l.self(false)
	l.self4.Store(&v4)
	l.self6.Store(&v6)
	if old4 != v4 || old6 != v6 {
		m.flows.dropLink(l)
	}
}

func (l *link) self(v4 bool) netip.Addr {
	p := l.self6.Load()
	if v4 {
		p = l.self4.Load()
	}
	if p == nil {
		return netip.Addr{}
	}
	return *p
}

func (l *link) enqueue(pb *pbuf) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.in <- pb:
		return true
	default:
		return false
	}
}

// Read gives WireGuard one packet an app sent to this tailnet.
func (l *link) Read(slab []byte, packets []tun.ReadPacket) (int, error) {
	select {
	case <-l.done:
		return 0, io.EOF
	case pb := <-l.in:
		const sp = tun.ReadPacketSpacing
		if len(slab) < 2*sp+pb.n || len(packets) == 0 {
			l.m.putBuf(pb)
			return 0, tun.ErrTooManySegments
		}
		packets[0].Offset = sp
		packets[0].Size = copy(slab[sp:len(slab)-sp], pb.b[:pb.n])
		l.m.putBuf(pb)
		return 1, nil
	}
}

// Write takes the tailnet's packets for this device.
func (l *link) Write(bufs [][]byte, offset int) (int, error) {
	select {
	case <-l.done:
		return 0, os.ErrClosed
	default:
	}
	for _, b := range bufs {
		if len(b) > offset {
			l.m.fromNode(l, b[offset:])
		}
	}
	return len(bufs), nil
}

func (l *link) File() *os.File           { return nil }
func (l *link) MTU() (int, error)        { return l.m.cfg.MTU, nil }
func (l *link) Name() (string, error)    { return "tailfuse-" + l.tailnet, nil }
func (l *link) Events() <-chan tun.Event { return l.events }
func (l *link) BatchSize() int           { return 1 }

// Close detaches the link: its flows are dropped and queued packets freed.
func (l *link) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.m.mu.Lock()
		if l.m.links[l.tailnet] == l {
			delete(l.m.links, l.tailnet)
		}
		l.m.mu.Unlock()
		l.m.flows.dropLink(l)
		l.m.FlushDNS(l.tailnet)
		close(l.events)
		for {
			select {
			case pb := <-l.in:
				l.m.putBuf(pb)
			default:
				return
			}
		}
	})
	return nil
}
