package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
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

// dialDirect tries each address in turn, sharing the dial timeout.
func (r *Router) dialDirect(targets []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
	defer cancel()
	d := net.Dialer{KeepAlive: 30 * time.Second}
	var last error
	for i, ap := range targets {
		actx := ctx
		if left := len(targets) - i; left > 1 {
			if dl, ok := ctx.Deadline(); ok {
				var c context.CancelFunc
				actx, c = context.WithTimeout(ctx, time.Until(dl)/time.Duration(left))
				defer c()
			}
		}
		conn, err := d.DialContext(actx, "tcp", ap.String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
