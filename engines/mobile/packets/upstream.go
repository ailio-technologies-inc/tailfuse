package packets

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Resolvers is an Upstream that asks the phone's own DNS servers (Android:
// the underlying network's LinkProperties.getDnsServers, set by the shell
// and updated when the network changes). UDP first, TCP when the answer is
// truncated; each server in turn.
type Resolvers struct {
	// Dial opens a socket that must not enter the VPN (on Android: a
	// net.Dialer whose Control calls VpnService.protect, the same hook
	// Tailscale's netns uses). nil: net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Protected: Dial's sockets bypass the VPN, so a resolver inside
	// Tailscale's range (some carriers number theirs in 100.64.0.0/10) can be
	// used. Without it such servers are dropped: they would loop into the
	// tunnel. Set before SetServers.
	Protected bool

	mu      sync.RWMutex
	servers []netip.AddrPort
}

// SetServers replaces the servers. Addresses in the tunnel's own ranges are
// dropped (asking them would loop back into the responder), and so are
// Tailscale-range ones unless Protected.
func (r *Resolvers) SetServers(servers []netip.AddrPort) {
	var ok []netip.AddrPort
	for _, s := range servers {
		a := s.Addr().Unmap()
		if !a.IsValid() || SyntheticV4.Contains(a) || SyntheticV6.Contains(a) || (IsTailscaleIP(a) && !r.Protected) {
			continue
		}
		if s.Port() == 0 {
			s = netip.AddrPortFrom(a, 53)
		}
		ok = append(ok, s)
	}
	r.mu.Lock()
	r.servers = ok
	r.mu.Unlock()
}

// Servers returns the current servers.
func (r *Resolvers) Servers() []netip.AddrPort {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]netip.AddrPort(nil), r.servers...)
}

func (r *Resolvers) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if r.Dial != nil {
		return r.Dial(ctx, network, address)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// Exchange implements Upstream.
func (r *Resolvers) Exchange(ctx context.Context, query []byte) ([]byte, error) {
	servers := r.Servers()
	if len(servers) == 0 {
		return nil, errors.New("packets: no upstream DNS servers")
	}
	if len(query) < 12 {
		return nil, errors.New("packets: short DNS query")
	}
	var last error
	for i, s := range servers {
		sctx := ctx
		if left := len(servers) - i; left > 1 {
			if dl, ok := ctx.Deadline(); ok {
				var cancel context.CancelFunc
				sctx, cancel = context.WithTimeout(ctx, time.Until(dl)/time.Duration(left))
				defer cancel()
			}
		}
		resp, err := r.udp(sctx, s, query)
		if err == nil && len(resp) >= 12 && resp[2]&0x02 != 0 { // TC
			resp, err = r.tcp(sctx, s, query)
		}
		if err == nil {
			return resp, nil
		}
		last = err
	}
	return nil, last
}

func (r *Resolvers) udp(ctx context.Context, s netip.AddrPort, query []byte) ([]byte, error) {
	c, err := r.dial(ctx, "udp", s.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(3 * time.Second))
	}
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 12 && buf[0] == query[0] && buf[1] == query[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

func (r *Resolvers) tcp(ctx context.Context, s netip.AddrPort, query []byte) ([]byte, error) {
	c, err := r.dial(ctx, "tcp", s.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	out := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(out, uint16(len(query)))
	copy(out[2:], query)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}
