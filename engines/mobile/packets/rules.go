// Package packets is Tailfuse's "all apps" mode: every app on the phone
// reaches every running tailnet by name and by address, through the VPN's
// tun device. See DESIGN.md.
//
// The package translates packets; it does not terminate connections. A
// packet an app sends to a tailnet address is rewritten (source: that
// tailnet's own Tailscale address; destination: the peer's real address) and
// handed to the owning tailnet's WireGuard engine through a tun.Device given
// to tsnet.Server.Tun. Replies come back the same way, rewritten in reverse.
// The app's TCP state lives in the phone's kernel and the peer; this process
// keeps only a small flow table, so idle and busy connections cost almost no
// memory here.
//
// It also answers DNS on an address inside the tunnel, with the engine's
// ownership rules.
//
// Everything the package needs from the engine is behind the small
// interfaces in this file, so it is tested with fakes and wired to the real
// RouteTable and nodes by a thin adapter.
package packets

import (
	"context"
	"net/netip"
)

// Verdict is how the engine's rules see a name or address. Same values and
// meaning as engine.Verdict (routes.go).
type Verdict int

const (
	// Unowned: no tailnet has anything to do with it.
	Unowned Verdict = iota
	// Owned: one tailnet owns it (it may be off; then it goes nowhere).
	Owned
	// Conflict: two or more tailnets claim it; refused.
	Conflict
	// TailnetSpace: a Tailscale address no running tailnet has; refused.
	TailnetSpace
	// Claimed: under a domain an off tailnet served when it last ran; refused.
	Claimed
)

func (v Verdict) String() string {
	switch v {
	case Unowned:
		return "unowned"
	case Owned:
		return "owned"
	case Conflict:
		return "conflict"
	case TailnetSpace:
		return "tailnet-space"
	case Claimed:
		return "claimed"
	}
	return "unknown"
}

// Rules is the engine's ownership rules. The adapter is RouteTable.Classify
// (it takes a host name or an IP literal).
type Rules interface {
	// ClassifyName: the tailnet that owns a DNS name (no trailing dot, lower
	// case) and why. Never does a DNS lookup.
	ClassifyName(name string) (tailnet string, v Verdict)
	// ClassifyAddr: the one running tailnet that has a device at ip, or why
	// none does (Conflict when two do, TailnetSpace for an unclaimed
	// Tailscale address).
	ClassifyAddr(ip netip.Addr) (tailnet string, v Verdict)
}

// TailnetDNS asks one tailnet's own resolver (MagicDNS and its split DNS).
// The adapter is local.Client.QueryDNS of that tailnet's node. It must fail,
// never fall back to public DNS, when the tailnet is not running.
type TailnetDNS interface {
	// QueryDNS returns the raw DNS response for name (with or without the
	// trailing dot) and qtype ("A", "AAAA", "PTR", ...).
	QueryDNS(ctx context.Context, tailnet, name, qtype string) ([]byte, error)
}

// RouteChecker is optional (Config.Routes). It reports whether a tailnet's
// node has a route for ip: a peer, or a subnet route it accepts. An owned
// name that resolves to such a non-Tailscale address (a host behind a subnet
// router) is then answered with a synthetic address, so its traffic enters
// the tunnel and leaves through that tailnet. Without it, such addresses are
// answered unchanged and reached over the phone's normal network, as the
// proxy path does.
type RouteChecker interface {
	Routes(tailnet string, ip netip.Addr) bool
}

// Upstream answers names no tailnet has anything to do with. Android only:
// its VPN DNS server receives every query. Nil on iOS, where the system only
// sends the owned names (NEDNSSettings.matchDomains); anything else that
// arrives is refused.
type Upstream interface {
	// Exchange sends a raw DNS query and returns the raw response.
	Exchange(ctx context.Context, query []byte) ([]byte, error)
}

// IsTailscaleIP reports whether ip is in 100.64.0.0/10 or fd7a:115c:a1e0::/48.
// Same as engine.IsTailscaleIP.
func IsTailscaleIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		b := ip.As4()
		return b[0] == 100 && b[1]&0xc0 == 64
	}
	if ip.Is6() {
		b := ip.As16()
		return b[0] == 0xfd && b[1] == 0x7a && b[2] == 0x11 && b[3] == 0x5c && b[4] == 0xa1 && b[5] == 0xe0
	}
	return false
}

// Tailscale's address ranges, routed into the tunnel.
var (
	TailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	TailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)
