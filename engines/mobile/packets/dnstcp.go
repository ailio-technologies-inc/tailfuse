package packets

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// DNS over TCP. Clients only use it after a truncated UDP answer (ours are
// small, so rarely) or by choice. It needs a real TCP endpoint, so a small
// gVisor stack terminates TCP on the DNS addresses only. It is started on
// the first TCP packet to port 53 and shut down after TCPDNSIdle without a
// connection, so it costs nothing while unused. gVisor is already in the
// binary (tsnet uses it), at the version Tailscale pins.

type tcpDNS struct {
	m       *Mux
	mu      sync.Mutex
	s       *stack.Stack
	ep      *channel.Endpoint
	cancel  context.CancelFunc
	lns     []*gonet.TCPListener
	conns   int
	last    time.Time
	stopped bool
	wg      sync.WaitGroup
}

const dnsNIC = 1

func (t *tcpDNS) inject(pkt []byte) {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	if t.s == nil {
		if err := t.startLocked(); err != nil {
			t.mu.Unlock()
			t.m.logf("packets: dns tcp: %v", err)
			return
		}
	}
	t.last = t.m.now()
	proto := header.IPv4ProtocolNumber
	if pkt[0]>>4 == 6 {
		proto = header.IPv6ProtocolNumber
	}
	// Under t.mu, so shutdown cannot close the stack mid-injection. The
	// stack only queues replies on ep here; nothing on that path takes t.mu.
	pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
	t.ep.InjectInbound(proto, pb)
	pb.DecRef()
	t.mu.Unlock()
}

func (t *tcpDNS) startLocked() error {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	// DNS messages are at most 64 KiB; keep the windows small.
	rx := tcpip.TCPReceiveBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: 16 << 10, Max: 64 << 10}
	tx := tcpip.TCPSendBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: 16 << 10, Max: 64 << 10}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rx)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tx)
	ep := channel.New(64, uint32(t.m.cfg.MTU), "")
	if err := s.CreateNIC(dnsNIC, ep); err != nil {
		s.Close()
		return errFromTCPIP(err)
	}
	for _, a := range []struct {
		addr  tcpip.Address
		proto tcpip.NetworkProtocolNumber
	}{
		{tcpip.AddrFrom4(DNSV4.As4()), ipv4.ProtocolNumber},
		{tcpip.AddrFrom16(DNSV6.As16()), ipv6.ProtocolNumber},
	} {
		if err := s.AddProtocolAddress(dnsNIC, tcpip.ProtocolAddress{Protocol: a.proto, AddressWithPrefix: a.addr.WithPrefix()}, stack.AddressProperties{}); err != nil {
			s.Close()
			return errFromTCPIP(err)
		}
	}
	any4, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes(make([]byte, 4)))
	any6, _ := tcpip.NewSubnet(tcpip.AddrFrom16([16]byte{}), tcpip.MaskFromBytes(make([]byte, 16)))
	s.SetRouteTable([]tcpip.Route{{Destination: any4, NIC: dnsNIC}, {Destination: any6, NIC: dnsNIC}})
	ctx, cancel := context.WithCancel(t.m.ctx)
	var lns []*gonet.TCPListener
	for _, la := range []struct {
		addr  tcpip.Address
		proto tcpip.NetworkProtocolNumber
	}{
		{tcpip.AddrFrom4(DNSV4.As4()), ipv4.ProtocolNumber},
		{tcpip.AddrFrom16(DNSV6.As16()), ipv6.ProtocolNumber},
	} {
		ln, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: dnsNIC, Addr: la.addr, Port: 53}, la.proto)
		if err != nil {
			cancel()
			for _, l := range lns {
				l.Close()
			}
			s.Close()
			return err
		}
		lns = append(lns, ln)
	}
	t.s, t.ep, t.cancel, t.lns = s, ep, cancel, lns
	t.wg.Add(1 + len(lns))
	go func() { // the stack's packets back to the apps
		defer t.wg.Done()
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			t.m.write(v.AsSlice())
			v.Release()
			pkt.DecRef()
		}
	}()
	for _, ln := range lns {
		go t.accept(ctx, ln)
	}
	return nil
}

func (t *tcpDNS) accept(ctx context.Context, ln *gonet.TCPListener) {
	defer t.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		t.mu.Lock()
		if t.conns >= t.m.lim.TCPDNSConns {
			t.mu.Unlock()
			c.Close()
			continue
		}
		t.conns++
		t.m.stats.TCPDNSConnsOpen.Add(1)
		t.mu.Unlock()
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.serve(ctx, c)
			c.Close()
			t.mu.Lock()
			t.conns--
			t.last = t.m.now()
			t.mu.Unlock()
		}()
	}
}

// serve answers length-prefixed queries (RFC 7766) until the client is
// quiet for 10 seconds.
func (t *tcpDNS) serve(ctx context.Context, c net.Conn) {
	var lenb [2]byte
	for ctx.Err() == nil {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, lenb[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lenb[:]))
		if n < 12 {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp := t.m.dns.handle(ctx, q, false)
		if resp == nil {
			return
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// maybeStop shuts the stack down once it has been idle for TCPDNSIdle.
func (t *tcpDNS) maybeStop() {
	t.mu.Lock()
	idle := t.s != nil && t.conns == 0 && t.m.now().Sub(t.last) >= t.m.lim.TCPDNSIdle
	t.mu.Unlock()
	if idle {
		t.shutdown(false)
	}
}

func (t *tcpDNS) stop() { t.shutdown(true) }

func (t *tcpDNS) shutdown(final bool) {
	t.mu.Lock()
	s, ep, cancel, lns := t.s, t.ep, t.cancel, t.lns
	t.s, t.ep, t.cancel, t.lns = nil, nil, nil, nil
	if final {
		t.stopped = true
	}
	t.mu.Unlock()
	if s == nil {
		return
	}
	cancel()
	for _, l := range lns {
		l.Close()
	}
	ep.Close()
	s.Close()
	t.wg.Wait()
	s.Wait()
}

// running reports whether the stack is up (tests).
func (t *tcpDNS) running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.s != nil
}

type tcpipError struct{ e tcpip.Error }

func (e tcpipError) Error() string { return e.e.String() }

func errFromTCPIP(e tcpip.Error) error { return tcpipError{e} }
