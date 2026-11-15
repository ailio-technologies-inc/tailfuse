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
	"syscall"
	"time"

	"github.com/ailio-technologies-inc/tailfuse/engines/mobile/packets"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/dns/dnsmessage"
)

// All-apps mode: every app reaches every running tailnet by name and by
// address through the tunnel's routes, not only apps that follow the system
// proxy. The packet path, the address pool and the DNS responder are in
// ./packets (see packets/DESIGN.md); this file wires them to the nodes and
// the route table.
//
// It is a saved setting, on by default (Config.ProxyOnly is its inverse),
// and only applies on a platform whose shell can hand over the tun
// (Options.Packets; tfmobile sets it). While it is on, each node is started
// with a Tun from the Mux; the proxy router keeps running beside it.
//
// The shell decides its network settings from state: all-apps settings when
// all_apps is true and all_apps_error is "", proxy settings otherwise. Any
// change to that choice fires "domains", the event on which the shells
// already re-apply their settings. If the packets cannot be started (no utun
// fd, a bad fd), all_apps_error says why and the shell falls back to proxy
// mode for this session: the phone is never left with neither.

// PoolFile keeps the synthetic addresses between runs.
const PoolFile = "synthetic-addresses.json"

// TunnelSettings is what a shell configures in all-apps mode (state.tunnel).
type TunnelSettings struct {
	Addresses []string `json:"addresses"` // the tunnel's own, CIDR: v4 /32 and v6 /128
	Routes    []string `json:"routes"`    // routed into the tunnel, CIDR
	DNS       []string `json:"dns"`       // the responder's addresses
	MTU       int      `json:"mtu"`
}

func tunnelSettings() TunnelSettings {
	return TunnelSettings{
		Addresses: []string{packets.TunnelV4.String() + "/32", packets.TunnelV6.String() + "/128"},
		Routes: []string{
			packets.SyntheticV4.String(), packets.TailscaleV4.String(),
			packets.SyntheticV6.String(), packets.TailscaleV6.String(),
		},
		DNS: []string{packets.DNSV4.String(), packets.DNSV6.String()},
		MTU: packets.DefaultMTU,
	}
}

// allApps is the engine's packet state. Guarded by Engine.mu.
type allApps struct {
	mux    *packets.Mux
	dev    packets.Device // nil while no tun is attached
	active bool
	err    string // why packets could not start this session; "" when none
}

// allAppsWanted: the nodes of a running engine should get a Tun.
func (e *Engine) allAppsWanted(cfg *Config) bool { return e.opts.Packets && !cfg.ProxyOnly }

// newMuxLocked makes the Mux for a starting engine. Caller holds opMu.
func (e *Engine) newMuxLocked(stateDir string, routes *RouteTable) error {
	var up packets.Upstream
	if e.forwardDNS() {
		up = e.resolvers
	}
	m, err := packets.New(packets.Config{
		Rules:    rulesAdapter{routes},
		DNS:      dnsAdapter{e},
		Upstream: up,
		Pool:     packets.NewPool(packets.PoolOptions{File: filepath.Join(stateDir, PoolFile)}),
		Logf:     e.opts.Logf,
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.aa.mux = m
	e.mu.Unlock()
	return nil
}

// linkFor is the Tun for a node about to start, nil in proxy mode.
func (e *Engine) linkFor(name string) tun.Device {
	e.mu.RLock()
	m := e.aa.mux
	e.mu.RUnlock()
	if m == nil {
		return nil
	}
	return m.Link(name)
}

// closeMux stops the packets and the Mux (engine stopping or mode change).
func (e *Engine) closeMux() {
	e.mu.Lock()
	m, dev := e.aa.mux, e.aa.dev
	e.aa.mux, e.aa.dev, e.aa.active = nil, nil, false
	e.mu.Unlock()
	if dev != nil {
		dev.Close()
	}
	if m != nil {
		m.Close()
	}
	e.applyPassthrough()
}

// SetAllApps saves the mode. While on, the nodes are restarted with (or
// without) their Tun and "domains" fires so the shell re-applies its
// settings; no tunnel restart is needed. Clears a previous all_apps_error,
// so setting it on again retries the packets.
func (e *Engine) SetAllApps(on bool) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	cfg := e.cfg.Clone()
	cfg.ProxyOnly = !on
	if err := e.commitLocked(cfg); err != nil {
		return err
	}
	e.mu.Lock()
	e.aa.err = ""
	hasMux := e.aa.mux != nil
	e.mu.Unlock()
	if !e.Running() || hasMux == e.allAppsWanted(cfg) {
		return nil
	}
	e.opts.Logf("all-apps mode %v: restarting nodes", on)
	e.closeMux()
	e.mu.Lock()
	nodes := e.nodes
	e.nodes = map[string]*nodeEntry{}
	e.mu.Unlock()
	closeNodes(nodes)
	if e.allAppsWanted(cfg) {
		if err := e.newMuxLocked(e.stateDir, e.routes); err != nil {
			e.setAllAppsError("could not start all-apps mode: " + err.Error())
		}
	}
	for _, name := range e.cfg.Names() {
		if shouldRun(e.cfg.Profiles[name]) {
			e.startNodeLocked(name)
		}
	}
	return nil
}

func (e *Engine) setAllAppsError(msg string) {
	e.mu.Lock()
	e.aa.err = msg
	e.mu.Unlock()
	e.opts.Logf("all-apps mode: %s; using proxy mode for this session", msg)
	e.changed()
}

// StartPacketsFD attaches the phone's tun. fd < 0 finds the utun fd of this
// process (iOS). utun: the darwin 4-byte header; then the fd is the
// system's and is never closed. Otherwise (Android) the engine owns fd.
// On failure the session falls back to proxy mode (all_apps_error).
func (e *Engine) StartPacketsFD(fd int, utun bool) error {
	e.mu.RLock()
	up, m := e.up, e.aa.mux
	e.mu.RUnlock()
	if !up {
		return errOff
	}
	if m == nil {
		return errors.New("all-apps mode is off")
	}
	if fd < 0 {
		found, err := packets.FindUtunFD()
		if err != nil {
			e.setAllAppsError(err.Error())
			return err
		}
		fd = found
	}
	dev, err := packets.NewFileDevice(fd, utun, !utun)
	if err != nil {
		e.setAllAppsError(err.Error())
		return err
	}
	return e.StartPackets(dev)
}

// StartPackets attaches a tun device (replacing one already attached).
func (e *Engine) StartPackets(dev packets.Device) error {
	e.mu.Lock()
	if !e.up {
		e.mu.Unlock()
		return errOff
	}
	m := e.aa.mux
	if m == nil {
		e.mu.Unlock()
		return errors.New("all-apps mode is off")
	}
	old := e.aa.dev
	e.aa.dev, e.aa.active, e.aa.err = dev, true, ""
	e.mu.Unlock()
	if old != nil {
		old.Close()
	}
	e.applyPassthrough()
	go func() {
		err := m.Run(dev)
		e.mu.Lock()
		mine := e.aa.dev == dev
		if mine {
			e.aa.dev, e.aa.active = nil, false
			if err != nil {
				e.aa.err = "the tunnel stopped: " + err.Error()
			}
		}
		e.mu.Unlock()
		if mine {
			if err != nil {
				e.opts.Logf("packets: %v", err)
			}
			e.applyPassthrough()
			e.changed()
		}
	}()
	e.opts.Logf("all-apps mode: packets started")
	e.changed()
	return nil
}

// StopPackets detaches the tun (the shell is about to replace or drop it).
func (e *Engine) StopPackets() {
	e.mu.Lock()
	dev := e.aa.dev
	e.aa.dev, e.aa.active = nil, false
	e.mu.Unlock()
	if dev != nil {
		dev.Close()
	}
	e.applyPassthrough()
	e.changed()
}

// applyPassthrough: passthrough only applies while the system proxy is the
// way apps reach the router, so never while packets run.
func (e *Engine) applyPassthrough() {
	e.mu.RLock()
	r, active := e.router, e.aa.active
	e.mu.RUnlock()
	if r != nil {
		r.SetPassthrough(e.passthrough.Load() && !active)
	}
}

// forwardDNS: unowned names are forwarded to the device's resolvers
// (Android, whose VPN DNS server gets every query). On iOS the system only
// sends match domains, and the rest are refused.
func (e *Engine) forwardDNS() bool { return e.opts.ForwardDNS || goos == "android" }

// SetDNSServers gives the engine the underlying network's DNS servers
// (Android), one per line: "1.1.1.1", "1.1.1.1:53", "2001:db8::1" or
// "[2001:db8::1]:53". Unparseable lines are skipped.
func (e *Engine) SetDNSServers(list string) {
	var out []netip.AddrPort
	for _, l := range strings.Fields(list) {
		if ap, err := netip.ParseAddrPort(l); err == nil {
			out = append(out, ap)
		} else if a, err := netip.ParseAddr(strings.Trim(l, "[]")); err == nil {
			out = append(out, netip.AddrPortFrom(a.WithZone(""), 53))
		}
	}
	e.dnsMu.Lock()
	e.dnsList = out
	e.dnsMu.Unlock()
	e.resolvers.SetServers(out)
}

// SetProtect registers VpnService.protect (Android): the nodes' sockets and
// the upstream DNS sockets bypass the VPN.
func (e *Engine) SetProtect(protect func(fd int) bool) {
	if protect == nil {
		return
	}
	setSocketProtector(protect)
	directProtect.Store(&protect)
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		ok := false
		if err := c.Control(func(fd uintptr) { ok = protect(int(fd)) }); err != nil {
			return err
		}
		if !ok {
			return errors.New("could not protect the socket from the vpn")
		}
		return nil
	}}
	e.dnsMu.Lock()
	list := e.dnsList
	e.resolvers.Dial = d.DialContext
	e.resolvers.Protected = true
	e.dnsMu.Unlock()
	e.resolvers.SetServers(list)
}

// forgetPoolLocked drops a removed tailnet's synthetic addresses.
func (e *Engine) forgetPoolLocked(name string) {
	e.mu.RLock()
	m := e.aa.mux
	e.mu.RUnlock()
	if m != nil {
		m.Pool().Forget(name)
		return
	}
	file := filepath.Join(e.stateDir, PoolFile)
	if _, err := os.Stat(file); err == nil {
		packets.NewPool(packets.PoolOptions{File: file}).Forget(name)
	}
}

// rulesAdapter gives the packet path the route table's rules.
type rulesAdapter struct{ rt *RouteTable }

func toVerdict(v Verdict) packets.Verdict {
	switch v {
	case Owned:
		return packets.Owned
	case Conflict:
		return packets.Conflict
	case TailnetSpace:
		return packets.TailnetSpace
	case Claimed:
		return packets.Claimed
	}
	return packets.Unowned
}

func (r rulesAdapter) ClassifyName(name string) (string, packets.Verdict) {
	p, v := r.rt.Classify(name)
	return p, toVerdict(v)
}

func (r rulesAdapter) ClassifyAddr(ip netip.Addr) (string, packets.Verdict) {
	p, v := r.rt.Classify(ip.String())
	return p, toVerdict(v)
}

// dnsAdapter asks a running tailnet's own resolver. For a domain of the
// person's own (RouteTable.PublicFallback) that the tailnet cannot answer,
// it asks ordinary DNS, as the proxy path does: the device's resolvers on
// Android (never the system resolver there: its queries come back into this
// VPN's DNS), the system resolver on iOS. A paused or stopped tailnet's
// names are never looked up anywhere.
type dnsAdapter struct{ e *Engine }

var (
	fallbackMu  sync.Mutex
	fallbackNow = map[string]int{} // names being looked up publicly now
)

func (d dnsAdapter) QueryDNS(ctx context.Context, tailnet, name, qtype string) ([]byte, error) {
	e := d.e
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	e.mu.RLock()
	var node Node
	running := false
	if ne := e.nodes[tailnet]; ne != nil && ne.node != nil {
		node, running = ne.node, ne.status.State == StateRunning
	}
	routes := e.routes
	e.mu.RUnlock()
	if node == nil || !running {
		return nil, fmt.Errorf("tailnet %s is not connected", tailnet)
	}
	public := routes != nil && routes.PublicFallback(name) && (qtype == "A" || qtype == "AAAA")
	if public {
		fallbackMu.Lock()
		busy := fallbackNow[name] > 0
		fallbackMu.Unlock()
		if busy {
			// Our own public lookup of this name came back to us: answer it
			// with a failure instead of asking again.
			return nil, errors.New("lookup loop for " + name)
		}
	}
	var raw []byte
	err := errors.New("the node cannot answer dns")
	if q, ok := node.(DNSQuerier); ok {
		raw, err = q.QueryDNS(ctx, name, qtype)
	}
	if !public || (err == nil && hasAnswer(raw)) {
		return raw, err
	}
	if alt, aerr := d.public(ctx, name, qtype); aerr == nil {
		return alt, nil
	}
	return raw, err
}

func hasAnswer(raw []byte) bool {
	var p dnsmessage.Parser
	h, err := p.Start(raw)
	if err != nil || h.RCode != dnsmessage.RCodeSuccess || p.SkipAllQuestions() != nil {
		return false
	}
	for {
		ah, err := p.AnswerHeader()
		if err != nil {
			return false
		}
		if ah.Type == dnsmessage.TypeA || ah.Type == dnsmessage.TypeAAAA {
			return true
		}
		if p.SkipAnswer() != nil {
			return false
		}
	}
}

// public looks name up in ordinary DNS and returns it as a DNS response.
func (d dnsAdapter) public(ctx context.Context, name, qtype string) ([]byte, error) {
	e := d.e
	fallbackMu.Lock()
	fallbackNow[name]++
	fallbackMu.Unlock()
	defer func() {
		fallbackMu.Lock()
		if fallbackNow[name]--; fallbackNow[name] <= 0 {
			delete(fallbackNow, name)
		}
		fallbackMu.Unlock()
	}()
	qt := dnsmessage.TypeA
	if qtype == "AAAA" {
		qt = dnsmessage.TypeAAAA
	}
	qn, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, err
	}
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 1, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: qn, Type: qt, Class: dnsmessage.ClassINET}}}
	if e.forwardDNS() && len(e.resolvers.Servers()) > 0 {
		raw, err := q.Pack()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		return e.resolvers.Exchange(ctx, raw)
	}
	network := "ip4"
	if qt == dnsmessage.TypeAAAA {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	addrs, err := e.opts.Resolver.LookupNetIP(ctx, network, name)
	if err != nil {
		return nil, err
	}
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 1, Response: true, RecursionAvailable: true}, Questions: q.Questions}
	for _, a := range addrs {
		a = a.Unmap()
		rh := dnsmessage.ResourceHeader{Name: qn, Class: dnsmessage.ClassINET, TTL: 60}
		if a.Is4() && qt == dnsmessage.TypeA {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AResource{A: a.As4()}})
		} else if a.Is6() && qt == dnsmessage.TypeAAAA {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return m.Pack()
}
