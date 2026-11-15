package engine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ailio-technologies-inc/tailfuse/engines/mobile/packets"
	"golang.org/x/net/dns/dnsmessage"
)

// The fake nodes answer DNS from this table: "profile name" -> addresses.
var fakeDNS sync.Map

func (n *fakeNode) QueryDNS(ctx context.Context, name, qtype string) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	qn := dnsmessage.MustNewName(name + ".")
	qt := dnsmessage.TypeA
	if qtype == "AAAA" {
		qt = dnsmessage.TypeAAAA
	}
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true},
		Questions: []dnsmessage.Question{{Name: qn, Type: qt, Class: dnsmessage.ClassINET}}}
	v, ok := fakeDNS.Load(n.opts.Name + " " + name)
	if !ok {
		m.RCode = dnsmessage.RCodeNameError
		return m.Pack()
	}
	for _, a := range v.([]netip.Addr) {
		rh := dnsmessage.ResourceHeader{Name: qn, Class: dnsmessage.ClassINET, TTL: 300}
		if a.Is4() && qt == dnsmessage.TypeA {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: rh, Body: &dnsmessage.AResource{A: a.As4()}})
		}
	}
	return m.Pack()
}

// allAppsEngine: a signed-in "work" tailnet (its own ts.net name and a
// domain of the person's, example.com) and a paused "home", with Packets on.
func allAppsEngine(t *testing.T, proxyOnly bool) (*Engine, *fakeNet, *fakeResolver) {
	t.Helper()
	fn := newFakeNet(t)
	fn.serve("work")
	dir := t.TempDir()
	c := NewConfig()
	c.ProxyOnly = proxyOnly
	c.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net", ".example.com"}}
	c.Profiles["home"] = &Profile{Suffixes: []string{".home.ts.net", ".home.example"}, Paused: true}
	writeConfig(t, dir, c)
	for _, p := range []string{"work", "home"} {
		os.MkdirAll(filepath.Join(dir, "nodes", p), 0o700)
		data, _ := json.Marshal(fakeState{Suffix: p + ".ts.net", Account: "ada@example.com"})
		os.WriteFile(filepath.Join(dir, "nodes", p, "fake.json"), data, 0o600)
	}
	e, _ := newTestEngine(t, fn, dir)
	e.opts.Packets = true
	res := e.opts.Resolver.(*fakeResolver)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	eventually(t, "work online", func() bool { p := profile(getState(t, e), "work"); return p != nil && p.Online })
	return e, fn, res
}

func TestAllAppsIsTheDefaultAndNodesGetATun(t *testing.T) {
	e, fn, _ := allAppsEngine(t, false)
	st := getState(t, e)
	if !st.AllApps || st.AllAppsActive || st.AllAppsError != "" {
		t.Fatalf("state %+v", st)
	}
	if st.Tunnel.MTU != 1280 || len(st.Tunnel.Routes) != 4 || st.Tunnel.DNS[0] != "198.18.0.2" || st.Tunnel.Addresses[0] != "198.18.0.1/32" {
		t.Fatalf("tunnel %+v", st.Tunnel)
	}
	if n := fn.node("work"); n == nil || n.opts.Tun == nil {
		t.Fatal("work started without a Tun in all-apps mode")
	}
	if fn.node("home") != nil {
		t.Fatal("a paused tailnet was started")
	}
	// The router keeps working beside it (ports, the Call API, Safari in
	// proxy fallback).
	if got := proxyGet(t, e, "http://app.work.ts.net/x"); !strings.HasPrefix(got, "work ") {
		t.Fatalf("router in all-apps mode: %q", got)
	}
}

func TestSetAllAppsSwitchesNodesAndFiresDomains(t *testing.T) {
	e, fn, _ := allAppsEngine(t, false)
	before := fn.count("work")
	var kinds []string
	var mu sync.Mutex
	e.SetListener(func(k string) { mu.Lock(); kinds = append(kinds, k); mu.Unlock() })

	mustOK(t, e, "set_all_apps", map[string]any{"on": false})
	if readConfig(t, e.opts.StateDir).ProxyOnly != true {
		t.Fatal("not saved")
	}
	if fn.count("work") != before+1 || fn.node("work").opts.Tun != nil {
		t.Fatal("proxy mode: work not restarted without a Tun")
	}
	if st := getState(t, e); st.AllApps {
		t.Fatal("state still all_apps")
	}
	eventually(t, "domains event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, k := range kinds {
			if k == "domains" {
				return true
			}
		}
		return false
	})
	if got := proxyGet(t, e, "http://app.work.ts.net/x"); !strings.HasPrefix(got, "work ") {
		t.Fatalf("router in proxy mode: %q", got)
	}
	mustOK(t, e, "set_all_apps", map[string]any{"on": true})
	if fn.node("work").opts.Tun == nil {
		t.Fatal("all-apps again: no Tun")
	}
	// Same mode again: no restart.
	n := fn.count("work")
	mustOK(t, e, "set_all_apps", map[string]any{"on": true})
	if fn.count("work") != n {
		t.Fatal("restarted for no change")
	}
	mustFail(t, e, "set_all_apps", map[string]any{}, "on is required")
}

func TestSetAllAppsWorksOff(t *testing.T) {
	dir := t.TempDir()
	e := New(Options{StateDir: dir, Packets: true})
	if r := call(t, e, "set_all_apps", map[string]any{"on": false}); !r.OK {
		t.Fatal(r.Error)
	}
	if !readConfig(t, dir).ProxyOnly {
		t.Fatal("not saved while off")
	}
	if st := getState(t, e); st.AllApps || st.Up {
		t.Fatalf("%+v", st)
	}
}

func TestPacketsStartStopAndFallback(t *testing.T) {
	e, _, _ := allAppsEngine(t, false)
	e.SetPassthrough(true)
	dev := packets.NewMemDevice(16)
	if err := e.StartPackets(dev); err != nil {
		t.Fatal(err)
	}
	st := getState(t, e)
	if !st.AllAppsActive || st.Passthrough {
		t.Fatalf("active %v passthrough %v: passthrough must be held off while packets run", st.AllAppsActive, st.Passthrough)
	}
	e.StopPackets()
	if st := getState(t, e); st.AllAppsActive || !st.Passthrough {
		t.Fatalf("after stop: %+v", st)
	}
	// The device going away on its own ends the session's packets.
	dev2 := packets.NewMemDevice(16)
	e.StartPackets(dev2)
	dev2.Close()
	eventually(t, "packets stopped", func() bool { return !getState(t, e).AllAppsActive })

	// No utun fd in this process: the session falls back to proxy mode,
	// says why, and the shell is told to re-apply its settings.
	if err := e.StartPacketsFD(-1, true); err == nil {
		t.Fatal("found a utun fd in a test process")
	}
	st = getState(t, e)
	if st.AllAppsError == "" || st.AllAppsActive || !st.AllApps {
		t.Fatalf("fallback state %+v", st)
	}
	if !strings.HasSuffix(e.lastDomains, "all_apps=false") {
		t.Fatalf("domains key %q", e.lastDomains)
	}
	if err := e.StartPacketsFD(-7, false); err == nil {
		t.Fatal("bad fd accepted")
	}
	// Setting it on again retries.
	mustOK(t, e, "set_all_apps", map[string]any{"on": true})
	if st := getState(t, e); st.AllAppsError != "" {
		t.Fatalf("error not cleared: %q", st.AllAppsError)
	}
	// A restart clears it too.
	e.setAllAppsError("x")
	e.Stop()
	if err := e.Start(e.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	if st := getState(t, e); st.AllAppsError != "" {
		t.Fatalf("error survived a restart: %q", st.AllAppsError)
	}
}

func TestPacketsUnavailableWithoutTheShell(t *testing.T) {
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, t.TempDir()) // Packets off: tfdev, desktop-parity tests
	if err := e.Start(e.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	st := getState(t, e)
	if !st.AllApps || st.AllAppsError == "" {
		t.Fatalf("%+v", st)
	}
	if err := e.StartPackets(packets.NewMemDevice(1)); err == nil {
		t.Fatal("packets started without all-apps mode")
	}
}

func TestDNSAdapterRules(t *testing.T) {
	e, _, res := allAppsEngine(t, false)
	d := dnsAdapter{e}
	ctx := context.Background()
	fakeDNS.Store("work app.work.ts.net", []netip.Addr{netip.MustParseAddr("100.80.0.5")})
	defer fakeDNS.Delete("work app.work.ts.net")
	addrs := func(raw []byte) []netip.Addr {
		var m dnsmessage.Message
		if err := m.Unpack(raw); err != nil {
			t.Fatal(err)
		}
		var out []netip.Addr
		for _, a := range m.Answers {
			if b, ok := a.Body.(*dnsmessage.AResource); ok {
				out = append(out, netip.AddrFrom4(b.A))
			}
		}
		return out
	}
	// The tailnet's own name: its DNS.
	raw, err := d.QueryDNS(ctx, "work", "app.work.ts.net", "A")
	if err != nil || len(addrs(raw)) != 1 {
		t.Fatalf("magicdns: %v %v", addrs(raw), err)
	}
	// A MagicDNS name the tailnet does not know: never asked publicly.
	d.QueryDNS(ctx, "work", "nope.work.ts.net", "A")
	if res.asked("nope.work.ts.net") {
		t.Fatal("a MagicDNS name was looked up in public DNS")
	}
	// A domain of the person's own the tailnet cannot answer: public DNS,
	// as the proxy path does.
	res.set("app.example.com", "100.74.194.18")
	raw, err = d.QueryDNS(ctx, "work", "app.example.com", "A")
	if err != nil || len(addrs(raw)) != 1 || addrs(raw)[0] != netip.MustParseAddr("100.74.194.18") {
		t.Fatalf("custom domain: %v %v", addrs(raw), err)
	}
	// A paused tailnet: an error, and nothing is looked up anywhere.
	res.set("nas.home.example", "192.168.1.5")
	if _, err := d.QueryDNS(ctx, "home", "nas.home.example", "A"); err == nil {
		t.Fatal("paused tailnet answered")
	}
	if res.asked("nas.home.example") {
		t.Fatal("a paused tailnet's name was looked up in public DNS")
	}
	// A lookup that loops back (iOS: the extension's own query reaching
	// the responder) fails at once instead of recursing.
	fallbackMu.Lock()
	fallbackNow["loop.example.com"] = 1
	fallbackMu.Unlock()
	_, err = d.QueryDNS(ctx, "work", "loop.example.com", "A")
	fallbackMu.Lock()
	delete(fallbackNow, "loop.example.com")
	fallbackMu.Unlock()
	if err == nil || res.asked("loop.example.com") {
		t.Fatalf("loop not cut: %v", err)
	}
	// The rules adapter is the route table's.
	r := rulesAdapter{e.routes}
	if p, v := r.ClassifyName("x.work.ts.net"); p != "work" || v != packets.Owned {
		t.Fatalf("%s %v", p, v)
	}
	if _, v := r.ClassifyAddr(netip.MustParseAddr("100.99.0.1")); v != packets.TailnetSpace {
		t.Fatalf("unclaimed tailnet address: %v", v)
	}
	if _, v := r.ClassifyName("example.org"); v != packets.Unowned {
		t.Fatalf("unowned: %v", v)
	}
}

func TestRemoveProfileForgetsSyntheticAddresses(t *testing.T) {
	e, _, _ := allAppsEngine(t, false)
	pool := e.aa.mux.Pool()
	a, err := pool.Get("work", netip.MustParseAddr("100.64.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, e, "remove_profile", map[string]any{"name": "work"})
	if _, _, ok := pool.Lookup(a); ok {
		t.Fatal("removed tailnet still has synthetic addresses")
	}
	data, _ := os.ReadFile(filepath.Join(e.opts.StateDir, PoolFile))
	if strings.Contains(string(data), `"work"`) {
		t.Fatalf("pool file still has work: %s", data)
	}
}

func TestSetDNSServersParses(t *testing.T) {
	e := New(Options{})
	e.SetDNSServers("1.1.1.1\n[2001:db8::1]:5353\n 9.9.9.9:53 \nnonsense\n100.64.0.53\n198.18.0.2")
	got := e.resolvers.Servers()
	want := []string{"1.1.1.1:53", "[2001:db8::1]:5353", "9.9.9.9:53"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("%v", got)
		}
	}
	// Protected sockets may use a carrier resolver inside 100.64.0.0/10.
	e.SetProtect(func(int) bool { return true })
	if n := len(e.resolvers.Servers()); n != 4 {
		t.Fatalf("protected: %v", e.resolvers.Servers())
	}
}

// proxyGet fetches url through the engine's router.
func proxyGet(t *testing.T, e *Engine, target string) string {
	t.Helper()
	proxy, _ := url.Parse("http://" + e.router.Addr())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy),
		DialContext: (&net.Dialer{}).DialContext}}
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
