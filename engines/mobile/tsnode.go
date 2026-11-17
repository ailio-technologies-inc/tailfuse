package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// TSNetFactory makes real nodes: one tsnet.Server per tailnet, its state in
// opts.Dir. This file is the only one that touches tsnet.
func TSNetFactory(opts NodeOptions) (Node, error) {
	return &tsNode{opts: opts}, nil
}

type tsNode struct {
	opts NodeOptions
	mu   sync.Mutex
	s    *tsnet.Server
	lc   *local.Client
}

// logsMu guards the process-wide no-logs switch.
var logsMu sync.Mutex

// setLogsMode sets Tailscale's process-wide no-logs-no-support switch.
// tsnet builds each node's log uploader when the node starts, and it uploads
// nothing (a no-op transport) while the switch is on; the switch also tells
// the control plane this device opted out. It must be set before a node
// starts, and it is shared by every node in the process (the engine restarts
// all nodes when it changes).
func setLogsMode(allowLogs bool) {
	logsMu.Lock()
	defer logsMu.Unlock()
	if allowLogs {
		envknob.Setenv("TS_NO_LOGS_NO_SUPPORT", "")
	} else {
		envknob.SetNoLogsNoSupport()
	}
}

func (n *tsNode) Start() error {
	setLogsMode(n.opts.Logs)
	// tailscale's logpolicy.LogsDir (used for the socket-stats log) panics when it finds no
	// writable place: an Android app process has no HOME, XDG_CACHE_HOME or TMPDIR, and its
	// working directory is "/". Point it into the state folder; harmless elsewhere.
	if os.Getenv("TS_LOGS_DIR") == "" {
		logs := filepath.Join(n.opts.Dir, "logs")
		if os.MkdirAll(logs, 0o700) == nil {
			os.Setenv("TS_LOGS_DIR", logs)
		}
	}
	logf := n.opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &tsnet.Server{
		Dir:      n.opts.Dir,
		Hostname: n.opts.Hostname,
		AuthKey:  n.opts.AuthKey,
		// Backend logs go only to tsnet's own (local, size-capped) buffer and,
		// when allowed, its uploader; UserLogf gets tsnet's few user messages.
		UserLogf: func(format string, args ...any) { logf("["+n.opts.Name+"] "+format, args...) },
		// All-apps mode only; nil keeps tsnet's own netstack-only device.
		Tun: n.opts.Tun,
	}
	if err := s.Start(); err != nil {
		return err
	}
	lc, err := s.LocalClient()
	if err != nil {
		s.Close()
		return err
	}
	n.mu.Lock()
	n.s, n.lc = s, lc
	n.mu.Unlock()
	return nil
}

func (n *tsNode) get() (*tsnet.Server, *local.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.s == nil {
		return nil, nil, errors.New("node not started")
	}
	return n.s, n.lc, nil
}

func (n *tsNode) Close() error {
	n.mu.Lock()
	s := n.s
	n.s, n.lc = nil, nil
	n.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}

// Dial resolves names with the tailnet's own DNS first (MagicDNS and split
// DNS, which tsnet's dialer would otherwise send to the device's resolver),
// then dials over the tailnet.
func (n *tsNode) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	s, lc, err := n.get()
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if ip, ok := tailnetLookup(ctx, lc, host); ok {
			address = net.JoinHostPort(ip.String(), port)
		}
	}
	return s.Dial(ctx, network, address)
}

// QueryDNS asks the tailnet's own resolver (the node's 100.100.100.100).
func (n *tsNode) QueryDNS(ctx context.Context, name, qtype string) ([]byte, error) {
	_, lc, err := n.get()
	if err != nil {
		return nil, err
	}
	raw, _, err := lc.QueryDNS(ctx, strings.TrimSuffix(name, ".")+".", qtype)
	return raw, err
}

// tailnetLookup asks the node's resolver (100.100.100.100) for host.
func tailnetLookup(ctx context.Context, lc *local.Client, host string) (netip.Addr, bool) {
	name := strings.TrimSuffix(host, ".") + "."
	for _, qt := range []string{"A", "AAAA"} {
		raw, _, err := lc.QueryDNS(ctx, name, qt)
		if err != nil {
			return netip.Addr{}, false
		}
		var p dnsmessage.Parser
		h, err := p.Start(raw)
		if err != nil || h.RCode != dnsmessage.RCodeSuccess {
			continue
		}
		if err := p.SkipAllQuestions(); err != nil {
			continue
		}
	answers:
		for {
			ah, err := p.AnswerHeader()
			if err != nil {
				break
			}
			switch ah.Type {
			case dnsmessage.TypeA:
				r, err := p.AResource()
				if err == nil {
					return netip.AddrFrom4(r.A), true
				}
			case dnsmessage.TypeAAAA:
				r, err := p.AAAAResource()
				if err == nil {
					return netip.AddrFrom16(r.AAAA), true
				}
			default:
				if p.SkipAnswer() != nil {
					break answers
				}
			}
		}
	}
	return netip.Addr{}, false
}

func (n *tsNode) Status(ctx context.Context) (NodeStatus, error) {
	_, lc, err := n.get()
	if err != nil {
		return NodeStatus{}, err
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return NodeStatus{}, err
	}
	out := NodeStatus{State: st.BackendState, AuthURL: st.AuthURL, Health: st.Health}
	for _, ip := range st.TailscaleIPs {
		out.IPs = append(out.IPs, ip.String())
	}
	if st.Self != nil && st.Self.DNSName != "" {
		label, _, _ := strings.Cut(st.Self.DNSName, ".")
		out.Hostname = label
		if u, ok := st.User[st.Self.UserID]; ok {
			out.Account = u.LoginName
		}
	}
	out.MagicDNSSuffix = st.MagicDNSSuffix
	if st.CurrentTailnet != nil {
		out.Tailnet = st.CurrentTailnet.Name
		if st.CurrentTailnet.MagicDNSSuffix != "" {
			out.MagicDNSSuffix = st.CurrentTailnet.MagicDNSSuffix
		}
	}
	out.MagicDNSSuffix = strings.ToLower(strings.Trim(out.MagicDNSSuffix, "."))
	return out, nil
}

func (n *tsNode) Login(ctx context.Context) error {
	_, lc, err := n.get()
	if err != nil {
		return err
	}
	return lc.StartLoginInteractive(ctx)
}

// SetPrefs applies the settings with one EditPrefs call. The exit node is
// found among the tailnet's devices by name or address.
func (n *tsNode) SetPrefs(ctx context.Context, p Prefs) error {
	_, lc, err := n.get()
	if err != nil {
		return err
	}
	routes := make([]netip.Prefix, 0, len(p.AdvertiseRoutes)+2)
	for _, r := range p.AdvertiseRoutes {
		pf, err := netip.ParsePrefix(strings.TrimSpace(r))
		if err != nil {
			return err
		}
		routes = append(routes, pf.Masked())
	}
	if p.AdvertiseExitNode {
		routes = append(routes, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))
	}
	mp := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			RouteAll:        p.AcceptRoutes,
			ShieldsUp:       p.ShieldsUp,
			AdvertiseRoutes: routes,
		},
		RouteAllSet:        true,
		ShieldsUpSet:       true,
		AdvertiseRoutesSet: true,
		// Clear any earlier exit node first; set below when one is wanted.
		ExitNodeIDSet: true,
		ExitNodeIPSet: true,
	}
	if want := strings.TrimSpace(p.ExitNode); want != "" {
		ip, err := n.findDevice(ctx, lc, want)
		if err != nil {
			return err
		}
		mp.ExitNodeIP = ip
	}
	_, err = lc.EditPrefs(ctx, mp)
	return err
}

// findDevice resolves a device's name, DNS name or address to its first
// Tailscale address.
func (n *tsNode) findDevice(ctx context.Context, lc *local.Client, want string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(want); err == nil {
		return ip, nil
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	want = strings.ToLower(strings.TrimSuffix(want, "."))
	for _, d := range st.Peer {
		name := strings.ToLower(strings.TrimSuffix(d.DNSName, "."))
		if (strings.ToLower(d.HostName) == want || name == want || strings.HasPrefix(name, want+".")) && len(d.TailscaleIPs) > 0 {
			return d.TailscaleIPs[0], nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no device called %q on this tailnet yet", want)
}

func (n *tsNode) Devices(ctx context.Context) ([]Device, error) {
	_, lc, err := n.get()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(st.Peer)+1)
	add := func(p *ipnstate.PeerStatus, self bool) {
		d := Device{Name: p.HostName, DNSName: strings.TrimSuffix(p.DNSName, "."), OS: p.OS, Online: p.Online || self, Self: self}
		d.IPs = make([]string, 0, len(p.TailscaleIPs))
		for _, ip := range p.TailscaleIPs {
			d.IPs = append(d.IPs, ip.String())
		}
		out = append(out, d)
	}
	if st.Self != nil {
		add(st.Self, true)
	}
	for _, p := range st.Peer {
		add(p, false)
	}
	return out, nil
}

func (n *tsNode) Learn(ctx context.Context) ([]netip.Addr, []string, error) {
	_, lc, err := n.get()
	if err != nil {
		return nil, nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ips []netip.Addr
	for _, p := range st.Peer {
		if p.Online {
			ips = append(ips, p.TailscaleIPs...)
		}
	}
	var routes, search []string
	if dc, err := lc.DNSConfig(ctx); err == nil && dc != nil {
		for k := range dc.Routes {
			routes = append(routes, k)
		}
		search = dc.Domains
	}
	return ips, ServedDomains(routes, search), nil
}
