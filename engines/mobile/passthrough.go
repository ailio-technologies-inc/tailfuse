package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// Passthrough (Android only). Android's VpnService can only give apps one
// plain HTTP proxy for everything (no PAC over a VPN), so every app's web
// traffic reaches the router. With passthrough on, a name NO tailnet has
// anything to do with is connected directly from the device, never through a
// node. Everything else keeps the strict rules:
//
//   - a name a tailnet owns goes to that tailnet, even while it is off or not
//     signed in (then it fails; it is never looked up publicly or sent out);
//   - a name or address two tailnets claim is refused;
//   - Tailscale address space (100.64.0.0/10, fd7a:115c:a1e0::/48) no
//     running tailnet has is refused, also when a name resolves into it;
//   - a domain an off tailnet served when it last ran is refused;
//   - this device's loopback and unspecified addresses are refused unless the
//     target is one of Tailfuse's own local ports (the router itself never is).
//
// Private LAN addresses (10/8, 172.16/12, 192.168/16, fc00::/7, link-local)
// are allowed: the browser on the phone reaches them through its normal
// network anyway, and the router only listens on loopback, so it gives no app
// anything it could not already reach.

// SetPassthrough switches passthrough on or off. Off (the default) keeps the
// desktop rule: a name no tailnet owns is refused.
func (r *Router) SetPassthrough(on bool) { r.passthrough.Store(on) }

// SetLocalPorts tells the router which loopback addresses are Tailfuse's own
// local ports (the only loopback targets passthrough may reach).
func (r *Router) SetLocalPorts(f func(netip.AddrPort) bool) { r.localPort = f }

// openErr is why a connection could not be opened.
type openErr struct {
	status int  // HTTP status for CONNECT and plain HTTP
	socks  byte // SOCKS5 reply code
	msg    string
}

func refused(msg string) *openErr {
	return &openErr{status: http.StatusForbidden, socks: socksNotAllowed, msg: msg}
}

// open connects to host:port the way the rules say: through the owning
// tailnet, directly (passthrough, unowned names only), or not at all.
func (r *Router) open(host, port string) (net.Conn, *openErr) {
	pass := r.passthrough.Load()
	var owner string
	if !pass {
		owner = r.routes.ResolveOwner(r.ctx, host)
	} else if owner = r.routes.Owner(host); owner == "" {
		// Refuse conflicts and an off tailnet's names before anything is
		// looked up in public DNS; only then try custom-domain discovery.
		if oe := classifyRefusal(r.routes, host); oe != nil {
			r.logf("router: refused %s: %s", host, oe.msg)
			return nil, oe
		}
		owner = r.routes.ResolveOwner(r.ctx, host)
	}
	if owner != "" {
		up, err := r.dialUp(owner, host, port)
		if err != nil {
			r.logf("router: %s via %s: %v", host, owner, err)
			return nil, &openErr{status: http.StatusBadGateway, socks: socksHostUnreachable,
				msg: fmt.Sprintf("tailnet %s could not reach %s: %v", owner, net.JoinHostPort(host, port), err)}
		}
		return up, nil
	}
	if !pass {
		r.logf("router: refused %s: no tailnet owns it", host)
		return nil, refused(refusal(host))
	}
	targets, oe := r.directTargets(host, port)
	if oe != nil {
		r.logf("router: refused %s: %s", host, oe.msg)
		return nil, oe
	}
	up, err := r.dialDirect(targets)
	if err != nil {
		return nil, &openErr{status: http.StatusBadGateway, socks: socksHostUnreachable,
			msg: fmt.Sprintf("could not reach %s: %v", net.JoinHostPort(host, port), err)}
	}
	return up, nil
}

// withOtherFamily adds what a second lookup finds when the first returned
// only one address family: some resolvers answer a dual-stack name with just
// the AAAA records, and a network without IPv6 then has nothing to connect to.
func (r *Router) withOtherFamily(host string, addrs []netip.Addr) []netip.Addr {
	has4, has6 := false, false
	for _, a := range addrs {
		if a.Unmap().Is4() {
			has4 = true
		} else {
			has6 = true
		}
	}
	if has4 && has6 {
		return addrs
	}
	network := "ip4"
	if has4 {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(r.ctx, 3*time.Second)
	defer cancel()
	more, err := r.resolver().LookupNetIP(ctx, network, host)
	if err != nil {
		return addrs
	}
	return append(addrs, more...)
}

// directTargets checks a name nothing routes and returns the addresses it
// may be dialed at directly.
func (r *Router) directTargets(host, portStr string) ([]netip.AddrPort, *openErr) {
	n, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || n == 0 {
		return nil, &openErr{status: http.StatusBadRequest, socks: socksFailure, msg: "bad port " + portStr}
	}
	port := uint16(n)
	if oe := classifyRefusal(r.routes, host); oe != nil {
		return nil, oe
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		if host == "" {
			return nil, refused("no host given")
		}
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		addrs, err = r.resolver().LookupNetIP(ctx, "ip", host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return nil, &openErr{status: http.StatusBadGateway, socks: socksHostUnreachable, msg: "could not resolve " + host}
		}
		addrs = r.withOtherFamily(host, addrs)
	}
	var ok []netip.AddrPort
	local := false
	for _, a := range addrs {
		a = a.Unmap()
		if IsTailscaleIP(a) {
			// Resolves into tailnet space that no running tailnet (or more
			// than one) has: never send it out to the internet.
			return nil, refused(host + " resolves to a tailscale address no running tailnet has")
		}
		ap := netip.AddrPortFrom(a, port)
		if r.isLocalAddr(a) {
			local = true
			if r.localPort != nil && r.localPort(ap) && ap.String() != r.addr {
				ok = append(ok, ap)
			}
			continue
		}
		if a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			continue
		}
		ok = append(ok, ap)
	}
	if len(ok) == 0 {
		if local {
			return nil, refused(host + " is this device; only tailfuse's local ports can be reached that way")
		}
		return nil, refused(host + " has no address that can be connected to")
	}
	return ok, nil
}

// classifyRefusal is the refusal for a name the rules hold back from
// passthrough, or nil for a name no tailnet has anything to do with.
func classifyRefusal(rt *RouteTable, host string) *openErr {
	switch p, v := rt.Classify(host); v {
	case Conflict:
		return refused(host + " is claimed by more than one tailnet")
	case TailnetSpace:
		return refused(host + " is a tailscale address no running tailnet has")
	case Claimed, Owned:
		return refused(fmt.Sprintf("%s belongs to tailnet %s, which is not connected", host, p))
	}
	return nil
}

func (r *Router) isLocalAddr(a netip.Addr) bool {
	if r.localAddr != nil {
		return r.localAddr(a)
	}
	return a.IsLoopback() || a.IsUnspecified()
}

func (r *Router) resolver() Resolver {
	if r.routes.resolver != nil {
		return r.routes.resolver
	}
	return net.DefaultResolver
}

// eyeballsDelay is how long a dial attempt runs alone before the next
// address (the other family first) starts beside it (RFC 8305: 250 ms).
const eyeballsDelay = 250 * time.Millisecond

// directProtect, when set (Android, by SetProtect), keeps the passthrough
// sockets out of the VPN, whatever routes it claims.
var directProtect atomic.Pointer[func(fd int) bool]

// directDialer dials a pre-resolved address: protected on Android.
func directDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{KeepAlive: 30 * time.Second}
	if p := directProtect.Load(); p != nil {
		protect := *p
		d.Control = func(_, _ string, c syscall.RawConn) error {
			ok := false
			if err := c.Control(func(fd uintptr) { ok = protect(int(fd)) }); err != nil {
				return err
			}
			if !ok {
				return errors.New("could not protect the socket from the vpn")
			}
			return nil
		}
	}
	return d.DialContext(ctx, network, addr)
}

// interleave orders addresses the way Happy Eyeballs tries them: the first
// address's family first, then alternating, each family in resolver order.
func interleave(targets []netip.AddrPort) []netip.AddrPort {
	var a, b []netip.AddrPort
	for _, t := range targets {
		if len(targets) > 0 && t.Addr().Is6() == targets[0].Addr().Is6() {
			a = append(a, t)
		} else {
			b = append(b, t)
		}
	}
	out := make([]netip.AddrPort, 0, len(targets))
	for len(a) > 0 || len(b) > 0 {
		if len(a) > 0 {
			out = append(out, a[0])
			a = a[1:]
		}
		if len(b) > 0 {
			out = append(out, b[0])
			b = b[1:]
		}
	}
	return out
}

// dialDirect connects to the first address that answers, Happy Eyeballs
// style: addresses alternate between families, the next starts when the
// previous fails or after eyeballsDelay, and the rest are cancelled once one
// connects. An unreachable IPv6 therefore costs at most one quick failure
// before IPv4 is tried.
func (r *Router) dialDirect(targets []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
	defer cancel()
	dial := r.directDial
	if dial == nil {
		dial = directDialer
	}
	targets = interleave(targets)
	type result struct {
		conn net.Conn
		err  error
		ap   netip.AddrPort
	}
	results := make(chan result, len(targets))
	attempt := func(ap netip.AddrPort) {
		c, err := dial(ctx, "tcp", ap.String())
		results <- result{c, err, ap}
	}
	var (
		started, finished int
		last              error
	)
	start := func() {
		go attempt(targets[started])
		started++
	}
	start()
	timer := time.NewTimer(eyeballsDelay)
	defer timer.Stop()
	for finished < started || started < len(targets) {
		select {
		case res := <-results:
			finished++
			if res.err == nil {
				cancel()
				go func() { // losers that connect anyway are closed
					for ; finished < started; finished++ {
						if lr := <-results; lr.conn != nil {
							lr.conn.Close()
						}
					}
				}()
				return res.conn, nil
			}
			r.logf("router: dial %s: %v", res.ap, res.err)
			last = res.err
			if started < len(targets) {
				start()
				resetTimer(timer, eyeballsDelay)
			}
		case <-timer.C:
			if started < len(targets) {
				start()
				timer.Reset(eyeballsDelay)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
