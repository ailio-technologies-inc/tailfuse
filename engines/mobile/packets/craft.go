package packets

import (
	"encoding/binary"
	"net/netip"

	"tailscale.com/net/packet"
	"tailscale.com/types/ipproto"
)

// Packets the package writes itself: DNS answers over UDP, TCP resets and
// ICMP "unreachable" for refused destinations.

// sum16 is the ones' complement sum of b folded onto acc (not inverted).
func sum16(b []byte, acc uint32) uint32 {
	for len(b) >= 2 {
		acc += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		acc += uint32(b[0]) << 8
	}
	return acc
}

func fold(acc uint32) uint16 {
	for acc>>16 != 0 {
		acc = acc&0xffff + acc>>16
	}
	return ^uint16(acc)
}

func pseudo(src, dst netip.Addr, proto ipproto.Proto, length int) uint32 {
	var acc uint32
	acc = sum16(src.AsSlice(), acc)
	acc = sum16(dst.AsSlice(), acc)
	acc += uint32(proto)
	acc += uint32(length)
	return acc
}

// ipHeader writes an IPv4 or IPv6 header for a transport payload of length
// n into b (20 or 40 bytes) and returns the header length.
func ipHeader(b []byte, src, dst netip.Addr, proto ipproto.Proto, n int) int {
	if src.Is4() {
		b[0] = 0x45
		b[1] = 0
		binary.BigEndian.PutUint16(b[2:], uint16(20+n))
		binary.BigEndian.PutUint32(b[4:], 0)
		b[8] = 64
		b[9] = byte(proto)
		b[10], b[11] = 0, 0
		s, d := src.As4(), dst.As4()
		copy(b[12:16], s[:])
		copy(b[16:20], d[:])
		binary.BigEndian.PutUint16(b[10:], fold(sum16(b[:20], 0)))
		return 20
	}
	binary.BigEndian.PutUint32(b[0:], 6<<28)
	binary.BigEndian.PutUint16(b[4:], uint16(n))
	b[6] = byte(proto)
	b[7] = 64
	s, d := src.As16(), dst.As16()
	copy(b[8:24], s[:])
	copy(b[24:40], d[:])
	return 40
}

func ipHeaderLen(a netip.Addr) int {
	if a.Is4() {
		return 20
	}
	return 40
}

// buildUDP returns an IP+UDP packet carrying payload.
func buildUDP(src, dst netip.AddrPort, payload []byte) []byte {
	hl := ipHeaderLen(src.Addr())
	n := 8 + len(payload)
	b := make([]byte, hl+n)
	ipHeader(b, src.Addr(), dst.Addr(), ipproto.UDP, n)
	u := b[hl:]
	binary.BigEndian.PutUint16(u[0:], src.Port())
	binary.BigEndian.PutUint16(u[2:], dst.Port())
	binary.BigEndian.PutUint16(u[4:], uint16(n))
	copy(u[8:], payload)
	cs := fold(sum16(u, pseudo(src.Addr(), dst.Addr(), ipproto.UDP, n)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	return b
}

// tcpReset returns the RST answering q (RFC 9293 3.10.7.1), or nil when q
// is itself a reset or too short.
func tcpReset(q *packet.Parsed) []byte {
	t := q.Transport()
	if len(t) < 20 || q.TCPFlags&packet.TCPRst != 0 {
		return nil
	}
	dataOff := int(t[12]>>4) * 4
	if dataOff < 20 || dataOff > len(t) {
		return nil
	}
	segLen := uint32(len(t) - dataOff)
	if q.TCPFlags&packet.TCPSyn != 0 {
		segLen++
	}
	if q.TCPFlags&packet.TCPFin != 0 {
		segLen++
	}
	var seq, ack uint32
	flags := byte(packet.TCPRst)
	if q.TCPFlags&packet.TCPAck != 0 {
		seq = binary.BigEndian.Uint32(t[8:])
	} else {
		ack = binary.BigEndian.Uint32(t[4:]) + segLen
		flags |= byte(packet.TCPAck)
	}
	src, dst := q.Dst, q.Src
	hl := ipHeaderLen(src.Addr())
	b := make([]byte, hl+20)
	ipHeader(b, src.Addr(), dst.Addr(), ipproto.TCP, 20)
	h := b[hl:]
	binary.BigEndian.PutUint16(h[0:], src.Port())
	binary.BigEndian.PutUint16(h[2:], dst.Port())
	binary.BigEndian.PutUint32(h[4:], seq)
	binary.BigEndian.PutUint32(h[8:], ack)
	h[12] = 5 << 4
	h[13] = flags
	binary.BigEndian.PutUint16(h[16:], fold(sum16(h, pseudo(src.Addr(), dst.Addr(), ipproto.TCP, 20))))
	return b
}

// icmpUnreachable returns "destination unreachable: administratively
// prohibited" for the packet orig (parsed as q), sent from the address the
// app tried to reach. nil for packets that must not get one (ICMP errors,
// broadcasts).
func icmpUnreachable(orig []byte, q *packet.Parsed) []byte {
	if q.IsError() {
		return nil
	}
	src, dst := q.Dst.Addr(), q.Src.Addr()
	if q.IPVersion == 4 {
		quote := orig
		ihl := int(orig[0]&0x0f) * 4
		if len(quote) > ihl+8 {
			quote = quote[:ihl+8]
		}
		n := 8 + len(quote)
		b := make([]byte, 20+n)
		ipHeader(b, src, dst, ipproto.ICMPv4, n)
		m := b[20:]
		m[0], m[1] = 3, 13 // destination unreachable, communication administratively prohibited
		copy(m[8:], quote)
		binary.BigEndian.PutUint16(m[2:], fold(sum16(m, 0)))
		return b
	}
	quote := orig
	if max := 1280 - 40 - 8; len(quote) > max {
		quote = quote[:max]
	}
	n := 8 + len(quote)
	b := make([]byte, 40+n)
	ipHeader(b, src, dst, ipproto.ICMPv6, n)
	m := b[40:]
	m[0], m[1] = 1, 1 // destination unreachable, administratively prohibited
	copy(m[8:], quote)
	binary.BigEndian.PutUint16(m[2:], fold(sum16(m, pseudo(src, dst, ipproto.ICMPv6, n))))
	return b
}
