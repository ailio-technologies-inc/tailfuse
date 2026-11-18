package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestEngine(t *testing.T, fn *fakeNet, dir string) (*Engine, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	e := New(Options{
		StateDir: dir, Listen: "127.0.0.1:0", NodeFactory: fn.factory, Resolver: &fakeResolver{},
		PollInterval: 10 * time.Millisecond, LearnInterval: 30 * time.Millisecond, LearnFirst: 10 * time.Millisecond,
		Now: clock.Now, Logf: func(f string, a ...any) { t.Logf(f, a...) },
	})
	t.Cleanup(e.Stop)
	return e, clock
}

type callResult struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func call(t *testing.T, e *Engine, cmd string, args any) callResult {
	t.Helper()
	req := map[string]any{"cmd": cmd}
	if args != nil {
		req["args"] = args
	}
	b, _ := json.Marshal(req)
	out := e.Call(string(b))
	if out == "" {
		t.Fatalf("%s: empty response", cmd)
	}
	var r callResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%s: response is not json: %q", cmd, out)
	}
	if !r.OK && r.Error == "" {
		t.Fatalf("%s: failure without error: %q", cmd, out)
	}
	return r
}

func mustOK(t *testing.T, e *Engine, cmd string, args any) json.RawMessage {
	t.Helper()
	r := call(t, e, cmd, args)
	if !r.OK {
		t.Fatalf("%s %v: %s", cmd, args, r.Error)
	}
	return r.Result
}

func mustFail(t *testing.T, e *Engine, cmd string, args any, want string) {
	t.Helper()
	r := call(t, e, cmd, args)
	if r.OK {
		t.Fatalf("%s %v succeeded, want %q", cmd, args, want)
	}
	if !strings.Contains(r.Error, want) {
		t.Fatalf("%s %v: error %q, want %q", cmd, args, r.Error, want)
	}
	if r.Error != strings.ToLower(r.Error[:1])+r.Error[1:] {
		t.Fatalf("error not lower-case: %q", r.Error)
	}
}

func getState(t *testing.T, e *Engine) State {
	t.Helper()
	var st State
	if err := json.Unmarshal(mustOK(t, e, "state", nil), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func profile(st State, name string) *ProfileState {
	for i := range st.Profiles {
		if st.Profiles[i].Name == name {
			return &st.Profiles[i]
		}
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeConfig(t *testing.T, dir string, c *Config) {
	t.Helper()
	if err := c.Save(filepath.Join(dir, ConfigFile)); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, dir string) *Config {
	t.Helper()
	c, err := LoadConfig(filepath.Join(dir, ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallBasics(t *testing.T) {
	e := New(Options{})
	for _, req := range []string{"", "nope", "{}", `{"cmd":"bogus"}`, `{"cmd":"set_unlocked","args":[1]}`} {
		out := e.Call(req)
		var r callResult
		if out == "" || json.Unmarshal([]byte(out), &r) != nil || r.OK || r.Error == "" {
			t.Errorf("Call(%q) = %q", req, out)
		}
	}
	// No state folder yet: config commands say so.
	mustFail(t, e, "set_unlocked", map[string]any{"on": true}, "state folder")
	// state still answers.
	st := getState(t, e)
	if st.Up || st.Version != Version || st.FreeProfiles != 2 || st.Profiles == nil || st.MatchDomains == nil || st.Tunnels == nil {
		t.Fatalf("state: %+v", st)
	}
}

// The "works off" column of API.md: with the engine never started.
func TestWorksOffColumn(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, "")
	e.SetStateDir(dir)
	c := NewConfig()
	c.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net"}, Account: "ada@example.com"}
	c.Profiles["home"] = &Profile{Suffixes: []string{".home-lab.ts.net"}}
	writeConfig(t, dir, c)
	os.MkdirAll(filepath.Join(dir, "nodes", "home"), 0o700)
	os.WriteFile(filepath.Join(dir, "nodes", "home", "keys"), []byte("x"), 0o600)

	st := getState(t, e)
	if st.Up || len(st.Profiles) != 2 || st.Proxy != "127.0.0.1:0" || !slices.Equal(st.MatchDomains, []string{"home-lab.ts.net", "work.ts.net"}) {
		t.Fatalf("state off: %+v", st)
	}
	if p := profile(st, "work"); p.Account != "ada@example.com" || p.Online || p.AuthURL != "" {
		t.Fatalf("work off: %+v", p)
	}

	// Not off-capable.
	mustFail(t, e, "add_profile", nil, "tailfuse is off")
	mustFail(t, e, "login", map[string]any{"name": "work"}, "tailfuse is off")
	mustFail(t, e, "devices", map[string]any{"name": "work"}, "tailfuse is off")

	// Off-capable.
	mustOK(t, e, "set_connected", map[string]any{"name": "work", "on": false})
	if !readConfig(t, dir).Profiles["work"].Paused {
		t.Fatal("paused not saved")
	}
	mustFail(t, e, "set_connected", map[string]any{"name": "nope", "on": false}, "there is no tailnet called")
	mustFail(t, e, "set_connected", map[string]any{"name": "work"}, "on is required")

	var th struct {
		Host  string  `json:"host"`
		Owner *string `json:"owner"`
	}
	json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "https://Wiki.work.ts.net/x"}), &th)
	if th.Host != "wiki.work.ts.net" || th.Owner == nil || *th.Owner != "work" {
		t.Fatalf("test_host: %+v", th)
	}
	json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "example.com"}), &th)
	if th.Owner != nil {
		t.Fatalf("unowned test_host: %+v", th)
	}
	mustFail(t, e, "test_host", map[string]any{"host": " "}, "no host")

	mustOK(t, e, "add_tunnel", map[string]any{"name": "db", "listen": "5432", "target": "db.work.ts.net:5432", "profile": "work"})
	if got := readConfig(t, dir).Tunnels; len(got) != 1 || got[0].Listen != "127.0.0.1:5432" {
		t.Fatalf("tunnel saved: %+v", got)
	}
	mustFail(t, e, "add_tunnel", map[string]any{"name": "db", "listen": "5433", "target": "db.work.ts.net:5432", "profile": "work"}, "duplicate local port name")
	mustFail(t, e, "add_tunnel", map[string]any{"name": "x", "listen": "0.0.0.0:5433", "target": "db.work.ts.net:5432", "profile": "work"}, "loopback")
	mustFail(t, e, "add_tunnel", map[string]any{"name": "x", "listen": "5433", "target": "db.work.ts.net:5432", "profile": "nope"}, "unknown profile")
	mustOK(t, e, "add_tunnel", map[string]any{"name": "nas", "listen": "8080", "target": "nas.home-lab.ts.net:80", "profile": "home"})
	mustOK(t, e, "remove_tunnel", map[string]any{"name": "db"})
	mustFail(t, e, "remove_tunnel", map[string]any{"name": "db"}, "there is no local port called")

	mustOK(t, e, "set_unlocked", map[string]any{"on": true})
	if !readConfig(t, dir).Unlocked || !getState(t, e).Unlocked {
		t.Fatal("unlocked not saved")
	}

	mustOK(t, e, "remove_profile", map[string]any{"name": "home"})
	if _, err := os.Stat(filepath.Join(dir, "nodes", "home")); !os.IsNotExist(err) {
		t.Fatal("keys left behind")
	}
	cfg := readConfig(t, dir)
	if cfg.Profiles["home"] != nil || len(cfg.Tunnels) != 0 {
		t.Fatalf("remove_profile left %+v", cfg)
	}
	mustFail(t, e, "remove_profile", map[string]any{"name": "home"}, "there is no tailnet called")
	if len(fn.nodes) != 0 {
		t.Fatal("a node was started while off")
	}

	// Another process (the iOS extension) changes the config: an off Call sees it.
	c = readConfig(t, dir)
	c.Profiles["third"] = &Profile{Suffixes: []string{".third.ts.net"}}
	writeConfig(t, dir, c)
	mustOK(t, e, "set_connected", map[string]any{"name": "third", "on": false})
	if p := profile(getState(t, e), "third"); p == nil || !p.Paused {
		t.Fatal("off Call did not reload the config")
	}
}

func TestSignInRenamesAndRoutes(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, dir)
	var evMu sync.Mutex
	var events []string
	e.SetListener(func(k string) { evMu.Lock(); events = append(events, k); evMu.Unlock() })
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(dir); err != nil {
		t.Fatal("second Start:", err)
	}

	var added struct{ Name string }
	json.Unmarshal(mustOK(t, e, "add_profile", nil), &added)
	if added.Name != "new-tailnet" {
		t.Fatalf("provisional name %q", added.Name)
	}
	eventually(t, "auth_url", func() bool {
		p := profile(getState(t, e), "new-tailnet")
		return p != nil && p.AuthURL == "https://login.example/a/new-tailnet" && !p.Online && p.Error == ""
	})
	if !readConfig(t, dir).Profiles["new-tailnet"].Pending {
		t.Fatal("provisional profile not saved as pending")
	}
	evMu.Lock()
	events = nil
	evMu.Unlock()

	fn.node("new-tailnet").signIn(fakeState{Suffix: "tail1234.ts.net", Account: "ada@example.com", Tailnet: "example.com"})
	eventually(t, "rename", func() bool {
		st := getState(t, e)
		p := profile(st, "example")
		return p != nil && p.Online && profile(st, "new-tailnet") == nil
	})
	st := getState(t, e)
	p := profile(st, "example")
	if !slices.Equal(p.Suffixes, []string{".tail1234.ts.net"}) || p.Account != "ada@example.com" || p.Tailnet != "example.com" || p.AuthURL != "" || !slices.Equal(p.IPs, []string{"100.64.0.7"}) {
		t.Fatalf("signed-in profile: %+v", p)
	}
	if !slices.Contains(st.MatchDomains, "tail1234.ts.net") {
		t.Fatalf("match_domains %v", st.MatchDomains)
	}
	cfg := readConfig(t, dir)
	if cp := cfg.Profiles["example"]; cp == nil || cp.Pending || cp.AuthKey != "" {
		t.Fatalf("saved: %+v", cfg.Profiles)
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", "example", "fake.json")); err != nil {
		t.Fatal("keys not moved to the new name:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", "new-tailnet")); !os.IsNotExist(err) {
		t.Fatal("provisional folder left behind")
	}
	eventually(t, "domains event", func() bool {
		evMu.Lock()
		defer evMu.Unlock()
		return slices.Contains(events, "domains") && slices.Contains(events, "state")
	})

	// Traffic through the engine's router lands on that tailnet.
	fn.serve("example")
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: st.Proxy})}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://nas.tail1234.ts.net/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "example nas.tail1234.ts.net /x" {
		t.Fatalf("proxied to %q", b)
	}
	if st.PACURL != "http://"+st.Proxy+"/tailfuse.pac" {
		t.Fatalf("pac_url %q", st.PACURL)
	}

	// test_host while on uses the router.
	var th struct {
		Owner *string `json:"owner"`
	}
	json.Unmarshal(mustOK(t, e, "test_host", map[string]any{"host": "nas.tail1234.ts.net"}), &th)
	if th.Owner == nil || *th.Owner != "example" {
		t.Fatalf("test_host on: %+v", th)
	}

	// devices
	n := fn.node("example")
	n.mu.Lock()
	n.devices = []Device{{Name: "nas", DNSName: "nas.tail1234.ts.net", IPs: []string{"100.64.0.9"}, OS: "linux", Online: true}}
	n.mu.Unlock()
	var devs DevicePage
	json.Unmarshal(mustOK(t, e, "devices", map[string]any{"name": "example"}), &devs)
	if len(devs.Devices) != 1 || devs.Devices[0].DNSName != "nas.tail1234.ts.net" || devs.Total != 1 || devs.Online != 1 || devs.Matched != 1 {
		t.Fatalf("devices %+v", devs)
	}
	mustFail(t, e, "devices", map[string]any{"name": "nope"}, "there is no tailnet called")

	// login starts a fresh sign-in on that node.
	mustOK(t, e, "login", map[string]any{"name": "example"})
	n.mu.Lock()
	logins := n.logins
	n.mu.Unlock()
	if logins != 1 {
		t.Fatal("login not passed to the node")
	}

	// Learning: a split-DNS domain the tailnet serves joins match_domains.
	n.mu.Lock()
	n.learnDom = []string{"corp.example"}
	n.learnIPs = []netip.Addr{netip.MustParseAddr("100.64.0.9")}
	n.mu.Unlock()
	eventually(t, "learned domain", func() bool { return slices.Contains(getState(t, e).MatchDomains, "corp.example") })
	resp, err = client.Get("http://wiki.corp.example/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(b), "example ") {
		t.Fatalf("learned domain proxied to %q", b)
	}

	// Local port: started at once while on, pinned to that tailnet.
	port := freePort(t)
	mustOK(t, e, "add_tunnel", map[string]any{"name": "nas", "listen": port, "target": "nas.tail1234.ts.net:80", "profile": "example"})
	mustFail(t, e, "add_tunnel", map[string]any{"name": "dup", "listen": port, "target": "nas.tail1234.ts.net:80", "profile": "example"}, "both listen on")
	mustFail(t, e, "add_tunnel", map[string]any{"name": "self", "listen": st.Proxy, "target": "nas.tail1234.ts.net:80", "profile": "example"}, "router's own port")
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET /via-port HTTP/1.1\r\nHost: nas\r\nConnection: close\r\n\r\n")
	pr, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(pr.Body)
	c.Close()
	if string(b) != "example nas /via-port" || fn.lastDial() != "example nas.tail1234.ts.net:80" {
		t.Fatalf("local port: %q via %q", b, fn.lastDial())
	}
	mustOK(t, e, "remove_tunnel", map[string]any{"name": "nas"})
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Fatal("removed local port still listening")
	}

	// Switch off: node closed, traffic refused (502), state paused.
	mustOK(t, e, "set_connected", map[string]any{"name": "example", "on": false})
	if !n.isClosed() {
		t.Fatal("node not stopped")
	}
	if p := profile(getState(t, e), "example"); !p.Paused || p.Online {
		t.Fatalf("paused state: %+v", p)
	}
	resp, err = client.Get("http://nas.tail1234.ts.net/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("paused tailnet: %d", resp.StatusCode)
	}
	mustOK(t, e, "set_connected", map[string]any{"name": "example", "on": true})
	eventually(t, "back online", func() bool { return profile(getState(t, e), "example").Online })
	if fn.count("example") != 2 {
		t.Fatalf("node starts: %d", fn.count("example"))
	}

	// Remove: node stopped, keys deleted, gone from state.
	port2 := freePort(t)
	mustOK(t, e, "add_tunnel", map[string]any{"name": "p2", "listen": port2, "target": "nas.tail1234.ts.net:80", "profile": "example"})
	mustOK(t, e, "remove_profile", map[string]any{"name": "example"})
	st = getState(t, e)
	if len(st.Profiles) != 0 || len(st.Tunnels) != 0 || slices.Contains(st.MatchDomains, "tail1234.ts.net") {
		t.Fatalf("after remove: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", "example")); !os.IsNotExist(err) {
		t.Fatal("keys left behind")
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port2, time.Second); err == nil {
		c.Close()
		t.Fatal("removed profile's local port still listening")
	}

	e.Stop()
	e.Stop()
	if e.Running() || getState(t, e).Up {
		t.Fatal("still up after Stop")
	}
	mustFail(t, e, "add_profile", nil, "tailfuse is off")
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func TestFreeLimit(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, dir)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	st := getState(t, e)
	if st.Unlocked || st.FreeProfiles != 2 {
		t.Fatalf("state %+v", st)
	}
	mustOK(t, e, "add_profile", nil)
	mustOK(t, e, "add_profile", nil)
	mustFail(t, e, "add_profile", nil, "that needs the unlimited upgrade")
	if got := len(getState(t, e).Profiles); got != 2 {
		t.Fatalf("refused add left %d profiles", got)
	}
	mustOK(t, e, "set_unlocked", map[string]any{"on": true})
	if !getState(t, e).Unlocked {
		t.Fatal("unlocked not reported")
	}
	mustOK(t, e, "add_profile", nil)
	// Sign the three in, then clear the flag: all three keep working.
	for i, name := range []string{"new-tailnet", "new-tailnet-2", "new-tailnet-3"} {
		fn.node(name).signIn(fakeState{Suffix: fmt.Sprintf("tail%d.ts.net", i), Account: fmt.Sprintf("u%d@gmail.com", i)})
	}
	eventually(t, "three signed in", func() bool {
		st := getState(t, e)
		on := 0
		for _, p := range st.Profiles {
			if p.Online {
				on++
			}
		}
		return on == 3 && profile(st, "u0") != nil
	})
	mustOK(t, e, "set_unlocked", map[string]any{"on": false})
	time.Sleep(50 * time.Millisecond)
	st = getState(t, e)
	if len(st.Profiles) != 3 || st.Unlocked {
		t.Fatalf("clearing the flag changed tailnets: %+v", st)
	}
	for _, p := range st.Profiles {
		if !p.Online || p.Paused {
			t.Fatalf("tailnet stopped working after the flag was cleared: %+v", p)
		}
	}
	mustFail(t, e, "add_profile", nil, "that needs the unlimited upgrade")
	// Removing one brings it back under the limit... only to 2, still refused.
	mustOK(t, e, "remove_profile", map[string]any{"name": "u2"})
	mustFail(t, e, "add_profile", nil, "that needs the unlimited upgrade")
	mustOK(t, e, "remove_profile", map[string]any{"name": "u1"})
	mustOK(t, e, "add_profile", nil)
}

// A sign-in begun under the limit is never blocked afterwards: not when the
// flag is cleared meanwhile, and not when it turns out to be a tailnet
// Tailfuse already has.
func TestLimitNotAppliedAfterTheFact(t *testing.T) {
	dir := t.TempDir()
	c := NewConfig()
	c.Unlocked = true
	c.Profiles["example"] = &Profile{Suffixes: []string{".tail1.ts.net"}, Account: "ada@example.com"}
	c.Profiles["other"] = &Profile{Suffixes: []string{".tail2.ts.net"}}
	writeConfig(t, dir, c)
	fn := newFakeNet(t)
	os.MkdirAll(filepath.Join(dir, "nodes", "example"), 0o700)
	(&fakeNode{net: fn, opts: NodeOptions{Dir: filepath.Join(dir, "nodes", "example")}}).signIn(fakeState{Suffix: "tail1.ts.net", Account: "ada@example.com"})
	os.MkdirAll(filepath.Join(dir, "nodes", "other"), 0o700)
	(&fakeNode{net: fn, opts: NodeOptions{Dir: filepath.Join(dir, "nodes", "other")}}).signIn(fakeState{Suffix: "tail2.ts.net", Account: "x@other.org"})
	e, _ := newTestEngine(t, fn, dir)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	mustOK(t, e, "add_profile", nil) // new-tailnet
	mustOK(t, e, "add_profile", nil) // new-tailnet-2
	mustOK(t, e, "set_unlocked", map[string]any{"on": false})

	// The first is a tailnet already listed: merged into it, keys replaced.
	fn.node("new-tailnet").signIn(fakeState{Suffix: "tail1.ts.net", Account: "bob@example.com", Tailnet: "example.com"})
	eventually(t, "merge", func() bool {
		st := getState(t, e)
		p := profile(st, "example")
		return profile(st, "new-tailnet") == nil && p != nil && p.Account == "bob@example.com" && p.Online
	})
	data, _ := os.ReadFile(filepath.Join(dir, "nodes", "example", "fake.json"))
	if !strings.Contains(string(data), "bob@example.com") {
		t.Fatalf("existing tailnet not refreshed with the new sign-in: %s", data)
	}
	// The second is new: added although the flag is now off.
	fn.node("new-tailnet-2").signIn(fakeState{Suffix: "tail3.ts.net", Account: "carol@third.net"})
	eventually(t, "third tailnet", func() bool {
		p := profile(getState(t, e), "third")
		return p != nil && p.Online
	})
	st := getState(t, e)
	if len(st.Profiles) != 3 {
		t.Fatalf("profiles %+v", st.Profiles)
	}
	mustFail(t, e, "add_profile", nil, "that needs the unlimited upgrade")
}

func TestPendingExpires(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, clock := newTestEngine(t, fn, dir)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	mustOK(t, e, "add_profile", map[string]any{"auth_key": "tskey-auth-x"})
	if got := fn.node("new-tailnet").opts.AuthKey; got != "tskey-auth-x" {
		t.Fatalf("auth key not passed: %q", got)
	}
	clock.add(9 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if profile(getState(t, e), "new-tailnet") == nil {
		t.Fatal("expired early")
	}
	clock.add(2 * time.Minute)
	eventually(t, "expiry", func() bool { return profile(getState(t, e), "new-tailnet") == nil })
	if !fn.node("new-tailnet").isClosed() {
		t.Fatal("expired node not stopped")
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", "new-tailnet")); !os.IsNotExist(err) {
		t.Fatal("expired keys left behind")
	}
	if len(readConfig(t, dir).Profiles) != 0 {
		t.Fatal("expired profile still saved")
	}
}

func TestTailnetRequiringLogs(t *testing.T) {
	dir := t.TempDir()
	c := NewConfig()
	c.Profiles["plain"] = &Profile{Suffixes: []string{".plain.ts.net"}}
	writeConfig(t, dir, c)
	fn := newFakeNet(t)
	os.MkdirAll(filepath.Join(dir, "nodes", "plain"), 0o700)
	(&fakeNode{net: fn, opts: NodeOptions{Dir: filepath.Join(dir, "nodes", "plain")}}).signIn(fakeState{Suffix: "plain.ts.net", Account: "p@plain.org"})
	e, _ := newTestEngine(t, fn, dir)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	if fn.node("plain").opts.Logs || getState(t, e).TailscaleLogs {
		t.Fatal("logs on by default")
	}
	mustOK(t, e, "add_profile", nil)
	fn.node("new-tailnet").signIn(fakeState{Suffix: "audit.ts.net", Account: "a@audited.com", RequiresLogs: true})
	eventually(t, "audited tailnet online", func() bool {
		p := profile(getState(t, e), "audited")
		return p != nil && p.Online
	})
	st := getState(t, e)
	if !st.TailscaleLogs || !readConfig(t, dir).Profiles["audited"].TailscaleLogs {
		t.Fatal("logging not recorded")
	}
	// The switch is process-wide: every node was restarted with it.
	if !fn.node("plain").opts.Logs || !fn.node("audited").opts.Logs {
		t.Fatal("nodes not restarted in logging mode")
	}
	// Remove it: back to no logs for everyone.
	mustOK(t, e, "remove_profile", map[string]any{"name": "audited"})
	if fn.node("plain").opts.Logs || getState(t, e).TailscaleLogs {
		t.Fatal("logging still on after the tailnet needing it was removed")
	}
	eventually(t, "plain online", func() bool { return profile(getState(t, e), "plain").Online })

	// An existing tailnet that starts requiring logs shows an error instead
	// of turning logging on by itself.
	(&fakeNode{net: fn, opts: NodeOptions{Dir: filepath.Join(dir, "nodes", "plain")}}).signIn(fakeState{Suffix: "plain.ts.net", RequiresLogs: true})
	eventually(t, "logging error", func() bool {
		return strings.Contains(profile(getState(t, e), "plain").Error, "requires Tailscale's logging")
	})
	if getState(t, e).TailscaleLogs {
		t.Fatal("logging turned on without a sign-in")
	}
}

func TestStartErrors(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	e := New(Options{Listen: busy.Addr().String(), NodeFactory: fn.factory})
	if err := e.Start(dir); err == nil {
		e.Stop()
		t.Fatal("started on a busy port")
	}
	if e.Running() {
		t.Fatal("running after a failed start")
	}
	bad := NewConfig()
	bad.Profiles["a"] = &Profile{Suffixes: []string{".x.ts.net"}}
	bad.Profiles["b"] = &Profile{Suffixes: []string{".x.ts.net"}}
	writeConfig(t, dir, bad)
	e2 := New(Options{Listen: "127.0.0.1:0", NodeFactory: fn.factory})
	if err := e2.Start(dir); err == nil || !strings.Contains(err.Error(), "owned by both") {
		e2.Stop()
		t.Fatalf("invalid config started: %v", err)
	}
	e3 := New(Options{Listen: "0.0.0.0:0", NodeFactory: fn.factory})
	if err := e3.Start(t.TempDir()); err == nil {
		e3.Stop()
		t.Fatal("router on 0.0.0.0")
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state folder mode %v", fi.Mode().Perm())
	}
}

func TestEventsAreCoalesced(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, "")
	e.SetStateDir(dir)
	var mu sync.Mutex
	counts := map[string]int{}
	e.SetListener(func(k string) { mu.Lock(); counts[k]++; mu.Unlock() })
	c := NewConfig()
	c.Profiles["w"] = &Profile{Suffixes: []string{".w.ts.net"}}
	writeConfig(t, dir, c)
	start := time.Now()
	for i := 0; i < 40; i++ {
		mustOK(t, e, "set_connected", map[string]any{"name": "w", "on": i%2 == 0})
	}
	time.Sleep(400 * time.Millisecond)
	elapsed := time.Since(start)
	mu.Lock()
	n := counts["state"]
	mu.Unlock()
	// At most one per 250 ms (plus the first).
	if max := int(elapsed/minEventGap) + 2; n == 0 || n > max {
		t.Fatalf("%d state events in %v", n, elapsed)
	}
	e.SetListener(nil)
	mustOK(t, e, "remove_profile", map[string]any{"name": "w"}) // no listener: no crash
	time.Sleep(300 * time.Millisecond)
}

func TestSetInterfacesFeedsNetmon(t *testing.T) {
	if err := SetInterfaces("bad"); err == nil {
		t.Fatal("bad list accepted")
	}
	if err := SetInterfaces("wlan0 30 1500 true true false false true | 10.1.10.131/24\n"); err != nil {
		t.Fatal(err)
	}
	ifs, err := netmonInterfaces()
	if err != nil || len(ifs) != 1 || ifs[0].Name != "wlan0" {
		t.Fatalf("netmon sees %v %v", ifs, err)
	}
	SetInterfaces("rmnet0 5 1400 true false false false false | 100.100.1.2/30\n")
	ifs, _ = netmonInterfaces()
	if len(ifs) != 1 || ifs[0].Name != "rmnet0" {
		t.Fatalf("update not seen: %v", ifs)
	}
}

func TestDeviceNameIsHostname(t *testing.T) {
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, t.TempDir())
	e.SetDeviceName("Ada's iPhone")
	if err := e.Start(e.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	var added struct{ Name string }
	if err := json.Unmarshal(mustOK(t, e, "add_profile", nil), &added); err != nil {
		t.Fatal(err)
	}
	if n := fn.node(added.Name); n == nil || n.opts.Hostname != "adas-iphone" {
		t.Fatalf("node hostname = %+v, want adas-iphone", n)
	}

	// A name with nothing usable in it leaves the fallback in place.
	e.SetDeviceName("???")
	if e.device != "" {
		t.Fatalf("device = %q after an unusable name", e.device)
	}
}

// A node restarting with saved keys reports NeedsLogin until control answers:
// that reads as connecting for loginSettle, and as a sign-in after it.
func TestBareNeedsLoginSettlesBeforeSignIn(t *testing.T) {
	fn := newFakeNet(t)
	dir := t.TempDir()
	c := NewConfig()
	c.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net"}}
	writeConfig(t, dir, c)
	os.MkdirAll(filepath.Join(dir, "nodes", "work"), 0o700)
	data, _ := json.Marshal(fakeState{Suffix: "work.ts.net", Account: "ada@example.com", Reconnecting: true})
	os.WriteFile(filepath.Join(dir, "nodes", "work", "fake.json"), data, 0o600)
	e, clock := newTestEngine(t, fn, dir)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	e.pollOnce()
	if p := profile(getState(t, e), "work"); p.NeedsLogin || !p.Connecting {
		t.Fatalf("just started: %+v", p)
	}
	clock.add(loginSettle)
	if p := profile(getState(t, e), "work"); !p.NeedsLogin || p.Connecting {
		t.Fatalf("after settling: %+v", p)
	}
}
