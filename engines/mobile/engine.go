package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ailio-technologies-inc/tailfuse/engines/mobile/packets"
	"github.com/tailscale/wireguard-go/tun"
)

// Version is the engine version reported in State.
const Version = "0.1.26"

// errOff is returned by commands that need the nodes while the engine is off.
var errOff = errors.New("tailfuse is off")

// Options configures an Engine.
type Options struct {
	StateDir    string
	Listen      string      // router address; DefaultListen when ""
	NodeFactory NodeFactory // required to Start
	Resolver    Resolver    // DNS for custom-domain discovery; net.DefaultResolver when nil
	Logf        func(string, ...any)

	PollInterval   time.Duration // node status refresh; 2s
	LearnInterval  time.Duration // what tailnets serve; 30s
	LearnFirst     time.Duration // first learning round after Start; 3s
	PendingTimeout time.Duration // unfinished sign-in removed after; 10m
	Now            func() time.Time

	// Packets: the shell can hand over the tun, so all-apps mode applies
	// (allapps.go). tfmobile sets it; tfdev and the desktop-parity tests
	// leave it off and run proxy mode only.
	Packets bool
	// ForwardDNS: names no tailnet owns are forwarded to SetDNSServers'
	// resolvers (always on Android; tests).
	ForwardDNS bool
}

// Engine runs one node per tailnet and the router. Every method is safe for
// concurrent use.
type Engine struct {
	opts Options

	// opMu serialises everything that changes the config or the nodes.
	opMu sync.Mutex

	// mu guards the fields below, for quick reads (state, dialing).
	mu        sync.RWMutex
	stateDir  string
	cfg       *Config
	up        bool
	stopping  bool
	routes    *RouteTable
	router    *Router
	tunnels   *tunnelSet
	nodes     map[string]*nodeEntry
	devMu     sync.Mutex // guards devCaches; held while fetching
	devCaches map[string]*devCache
	logsMode  bool
	device    string // hostname from SetDeviceName, "" when not given

	passthrough atomic.Bool // SetPassthrough (Android)
	aa          allApps     // all-apps mode; guarded by mu
	resolvers   *packets.Resolvers
	dnsMu       sync.Mutex
	dnsList     []netip.AddrPort
	stopCh      chan struct{}
	loops       sync.WaitGroup

	// change events
	nmu         sync.Mutex
	listener    func(kind string)
	lastState   []byte
	lastDomains string
	pendKinds   map[string]bool
	armed       bool
	lastFire    time.Time
}

type nodeEntry struct {
	node   Node
	tun    tun.Device // all-apps mode: the node's link in the Mux
	status NodeStatus
	err    string
}

// New returns an engine that is off.
func New(opts Options) *Engine {
	if opts.Listen == "" {
		opts.Listen = DefaultListen
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.LearnInterval == 0 {
		opts.LearnInterval = 30 * time.Second
	}
	if opts.LearnFirst == 0 {
		opts.LearnFirst = 3 * time.Second
	}
	if opts.PendingTimeout == 0 {
		opts.PendingTimeout = 10 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{opts: opts, stateDir: opts.StateDir, cfg: NewConfig(), nodes: map[string]*nodeEntry{}, pendKinds: map[string]bool{},
		resolvers: &packets.Resolvers{}}
}

// SetStateDir sets where the config lives while the engine is off.
func (e *Engine) SetStateDir(dir string) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.up {
		e.stateDir = dir
	}
}

// SetDeviceName sets the name this device takes on each tailnet from the
// phone's own name (e.g. "Ada's iPhone" becomes "adas-iphone"). A name with
// nothing usable in it is ignored. Takes effect for nodes started after it;
// a hostname saved in the config still wins.
func (e *Engine) SetDeviceName(name string) {
	h := HostnameFromDeviceName(name)
	e.mu.Lock()
	e.device = h
	e.mu.Unlock()
}

// SetPassthrough switches passthrough on or off (Android only; see
// passthrough.go). It may be called before Start and takes effect at once.
// While all-apps packets run it is held off (no system proxy then).
func (e *Engine) SetPassthrough(on bool) {
	e.passthrough.Store(on)
	e.applyPassthrough()
	e.changed()
}

// SetListener registers the one change listener (nil clears it).
func (e *Engine) SetListener(l func(kind string)) {
	e.nmu.Lock()
	e.listener = l
	e.nmu.Unlock()
}

// Running reports whether the engine is started.
func (e *Engine) Running() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.up
}

func (e *Engine) configPath() string { return filepath.Join(e.stateDir, ConfigFile) }
func (e *Engine) nodeDir(name string) string {
	return filepath.Join(e.stateDir, "nodes", name)
}

// Start loads the config from stateDir (created if missing), starts the
// router and brings up every tailnet that is not paused. Safe to call twice.
func (e *Engine) Start(stateDir string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.Running() {
		return nil
	}
	if e.opts.NodeFactory == nil {
		return errors.New("no node factory")
	}
	if stateDir == "" {
		stateDir = e.stateDir
	}
	if stateDir == "" {
		return errors.New("no state folder given")
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "nodes"), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(stateDir, 0o700)
	cfg, err := LoadConfig(filepath.Join(stateDir, ConfigFile))
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	routes := NewRouteTable(filepath.Join(stateDir, "learned-hosts.json"), e.opts.Resolver)
	routes.SetConfig(cfg)
	router := NewRouter(routes, e.dialProfile, e.opts.Logf)
	tunnels := newTunnelSet(e.dialProfile, e.opts.Logf)
	router.SetPassthrough(e.passthrough.Load()) // packets are not running yet
	router.SetLocalPorts(tunnels.has)
	if err := router.Listen(e.opts.Listen); err != nil {
		return err
	}
	e.mu.Lock()
	e.stateDir = stateDir
	e.cfg = cfg
	e.routes = routes
	e.router = router
	e.tunnels = tunnels
	e.nodes = map[string]*nodeEntry{}
	e.dropAllDevices()
	e.logsMode = wantLogs(cfg)
	e.up = true
	e.stopping = false
	e.stopCh = make(chan struct{})
	e.aa.err = ""
	e.mu.Unlock()

	if e.allAppsWanted(cfg) {
		if err := e.newMuxLocked(stateDir, routes); err != nil {
			e.setAllAppsError("could not start all-apps mode: " + err.Error())
		}
	}
	e.expirePendingLocked()
	for _, name := range e.cfg.Names() {
		if shouldRun(e.cfg.Profiles[name]) {
			e.startNodeLocked(name)
		}
	}
	e.tunnels.sync(e.cfg.Tunnels)
	e.loops.Add(2)
	go e.pollLoop(e.stopCh)
	go e.learnLoop(e.stopCh)
	e.changed()
	return nil
}

// Stop shuts every node and the router down. Safe when not started.
func (e *Engine) Stop() {
	e.opMu.Lock()
	e.mu.Lock()
	if !e.up || e.stopping {
		e.mu.Unlock()
		e.opMu.Unlock()
		return
	}
	e.stopping = true
	close(e.stopCh)
	e.mu.Unlock()
	e.opMu.Unlock()
	e.loops.Wait()

	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.router.Close()
	e.tunnels.closeAll()
	e.mu.Lock()
	nodes := e.nodes
	e.nodes = map[string]*nodeEntry{}
	e.dropAllDevices()
	e.up = false
	e.stopping = false
	e.mu.Unlock()
	closeNodes(nodes)
	e.closeMux()
	e.changed()
}

func closeNodes(nodes map[string]*nodeEntry) {
	var wg sync.WaitGroup
	for _, ne := range nodes {
		wg.Add(1)
		go func(ne *nodeEntry) {
			defer wg.Done()
			if ne.node != nil {
				_ = ne.node.Close()
			}
			if ne.tun != nil {
				_ = ne.tun.Close() // tsnet closes it too; closing twice is fine
			}
		}(ne)
	}
	wg.Wait()
}

// shouldRun: the profile's node is started with the engine.
func shouldRun(p *Profile) bool { return !p.Paused && !p.SignedOut }

// wantLogs: some tailnet that will run requires Tailscale's logging.
func wantLogs(c *Config) bool {
	for _, p := range c.Profiles {
		if p.TailscaleLogs && shouldRun(p) {
			return true
		}
	}
	return false
}

func defaultHostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	if h == "" || h == "localhost" {
		h = "tailfuse-" + runtime.GOOS
	}
	return h
}

// startNodeLocked makes and starts the node for a profile. Errors are kept
// beside the profile. Caller holds opMu.
func (e *Engine) startNodeLocked(name string) {
	p := e.cfg.Profiles[name]
	if p == nil {
		return
	}
	ne := &nodeEntry{}
	hostname := e.cfg.Hostname
	if hostname == "" {
		e.mu.RLock()
		hostname = e.device
		e.mu.RUnlock()
	}
	if hostname == "" {
		hostname = defaultHostname()
	}
	dir := e.nodeDir(name)
	err := os.MkdirAll(dir, 0o700)
	var n Node
	if err == nil {
		ne.tun = e.linkFor(name)
		n, err = e.opts.NodeFactory(NodeOptions{
			Name: name, Dir: dir, Hostname: hostname, AuthKey: p.AuthKey,
			Logs: e.logsMode, Logf: e.opts.Logf, Tun: ne.tun,
		})
	}
	if err == nil {
		ne.node = n
		if err = n.Start(); err != nil {
			_ = n.Close()
			ne.node = nil
		}
	}
	if err != nil {
		ne.err = "could not start: " + err.Error()
		e.opts.Logf("tailnet %s: %v", name, err)
	}
	e.mu.Lock()
	e.nodes[name] = ne
	e.mu.Unlock()
	if ne.node != nil && !p.Prefs.IsZero() {
		e.applyPrefs(name, ne.node, p.Prefs)
	}
}

// applyPrefs sets a node's Tailscale settings in the background. The exit node
// can only be found once the tailnet's devices are known, so it retries for a
// minute; the node stops being worth trying once it is closed.
func (e *Engine) applyPrefs(name string, n Node, p Prefs) {
	pn, ok := n.(PrefsNode)
	if !ok {
		return
	}
	go func() {
		var err error
		for i := 0; i < 30; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = pn.SetPrefs(ctx, p)
			cancel()
			if err == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
		e.opts.Logf("tailnet %s: could not apply settings: %v", name, err)
	}()
}

// stopNodeLocked closes a profile's node. Caller holds opMu.
func (e *Engine) stopNodeLocked(name string) {
	e.mu.Lock()
	ne := e.nodes[name]
	delete(e.nodes, name)
	e.mu.Unlock()
	e.dropDevices(name)
	if ne != nil && ne.node != nil {
		_ = ne.node.Close()
	}
	if ne != nil && ne.tun != nil {
		_ = ne.tun.Close()
	}
	if e.routes != nil {
		e.routes.Forget(name)
	}
}

// reconcileLogsLocked restarts every node when the process's logging mode
// must change. Tailscale's no-logs switch is process-wide, so all tailnets in
// this process share it. Caller holds opMu.
func (e *Engine) reconcileLogsLocked() {
	if !e.up {
		return
	}
	want := wantLogs(e.cfg)
	if want == e.logsMode {
		return
	}
	e.opts.Logf("tailscale logging %v for this process; restarting nodes", want)
	e.mu.Lock()
	nodes := e.nodes
	e.nodes = map[string]*nodeEntry{}
	e.dropAllDevices()
	e.logsMode = want
	e.mu.Unlock()
	closeNodes(nodes)
	for _, name := range e.cfg.Names() {
		if shouldRun(e.cfg.Profiles[name]) {
			e.startNodeLocked(name)
		}
	}
}

// commitLocked validates and saves cfg, then makes it current.
func (e *Engine) commitLocked(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if e.stateDir == "" {
		return errors.New("no state folder given")
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		return err
	}
	if err := cfg.Save(e.configPath()); err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg = cfg
	if e.routes != nil {
		e.routes.SetConfig(cfg)
	}
	e.mu.Unlock()
	return nil
}

// reloadLocked re-reads the config while off (another process may have
// written it while it was on).
func (e *Engine) reloadLocked() error {
	if e.Running() {
		return nil
	}
	if e.stateDir == "" {
		return errors.New("no state folder given: call SetStateDir first")
	}
	cfg, err := LoadConfig(e.configPath())
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
	return nil
}

// dialProfile dials through a tailnet's node; used by the router and ports.
func (e *Engine) dialProfile(ctx context.Context, profile, network, address string) (net.Conn, error) {
	e.mu.RLock()
	var node Node
	state := ""
	if ne := e.nodes[profile]; ne != nil {
		node, state = ne.node, ne.status.State
	}
	e.mu.RUnlock()
	if node == nil {
		return nil, fmt.Errorf("tailnet %s is not connected", profile)
	}
	if state != StateRunning {
		return nil, fmt.Errorf("tailnet %s is not connected (%s)", profile, strings.ToLower(state))
	}
	return node.Dial(ctx, network, address)
}

func (e *Engine) pollLoop(stop chan struct{}) {
	defer e.loops.Done()
	t := time.NewTicker(e.opts.PollInterval)
	defer t.Stop()
	e.pollOnce()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.pollOnce()
		}
	}
}

// pollOnce refreshes every node's status and acts on it: finishes sign-ins,
// drops expired ones, notices tailnets that require logging.
func (e *Engine) pollOnce() {
	e.opMu.Lock()
	defer e.changed()
	defer e.opMu.Unlock()
	e.mu.RLock()
	if !e.up || e.stopping {
		e.mu.RUnlock()
		return
	}
	entries := make(map[string]*nodeEntry, len(e.nodes))
	for n, ne := range e.nodes {
		entries[n] = ne
	}
	e.mu.RUnlock()

	for name, ne := range entries {
		if ne.node == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := ne.node.Status(ctx)
		cancel()
		if err != nil {
			continue
		}
		e.mu.Lock()
		ne.status = st
		m := e.aa.mux
		e.mu.Unlock()
		if m != nil && st.State == StateRunning {
			var addrs []netip.Addr
			for _, s := range st.IPs {
				if a, err := netip.ParseAddr(s); err == nil {
					addrs = append(addrs, a)
				}
			}
			m.SetAddrs(name, addrs)
		}
	}
	e.expirePendingLocked()

	cfg := e.cfg.Clone()
	dirty := false
	logsChanged := false
	for _, name := range cfg.Names() {
		p := cfg.Profiles[name]
		e.mu.RLock()
		ne := e.nodes[name]
		e.mu.RUnlock()
		if ne == nil || ne.node == nil {
			continue
		}
		e.mu.RLock()
		st := ne.status
		e.mu.RUnlock()
		needsLogs := slices.ContainsFunc(st.Health, func(h string) bool { return strings.Contains(h, loggingRequired) })
		if needsLogs && !p.TailscaleLogs {
			if p.Pending {
				// Some tailnets refuse a node whose logging is off. The
				// person is adding this one now: allow it, as on desktop.
				p.TailscaleLogs = true
				dirty, logsChanged = true, true
			} else {
				e.setNodeErr(ne, "This tailnet requires Tailscale's logging. Remove it here and add it again to allow that.")
			}
			continue
		}
		if needsLogs && p.TailscaleLogs && !e.logsMode {
			logsChanged = true
			continue
		}
		e.setNodeErr(ne, "")
		if st.State != StateRunning {
			continue
		}
		if p.Pending {
			if st.MagicDNSSuffix != "" {
				if dirty {
					if err := e.commitLocked(cfg); err != nil {
						e.opts.Logf("saving config: %v", err)
					}
					dirty = false
				}
				e.finishSignInLocked(name, st)
				cfg = e.cfg.Clone()
			}
			continue
		}
		if p.AuthKey != "" {
			p.AuthKey = "" // signed in: the key is no longer needed
			dirty = true
		}
		if st.Account != "" && st.Account != p.Account {
			p.Account = st.Account
			dirty = true
		}
		if st.Tailnet != "" && st.Tailnet != p.Tailnet {
			p.Tailnet = st.Tailnet
			dirty = true
		}
	}
	if dirty {
		if err := e.commitLocked(cfg); err != nil {
			e.opts.Logf("saving config: %v", err)
		}
	}
	if logsChanged {
		e.reconcileLogsLocked()
	}
}

func (e *Engine) setNodeErr(ne *nodeEntry, msg string) {
	e.mu.Lock()
	if !strings.HasPrefix(ne.err, "could not start") {
		ne.err = msg
	}
	e.mu.Unlock()
}

// expirePendingLocked removes sign-ins nobody finished in time.
func (e *Engine) expirePendingLocked() {
	now := e.opts.Now()
	for _, name := range e.cfg.Names() {
		p := e.cfg.Profiles[name]
		if p.Pending && !p.Created.IsZero() && now.Sub(p.Created) > e.opts.PendingTimeout {
			e.opts.Logf("sign-in %s not finished in time; removing it", name)
			_ = e.removeProfileLocked(name)
		}
	}
}

// finishSignInLocked turns a provisional profile into a real one once its
// node has signed in: renamed after the tailnet, or, when Tailfuse already has
// that tailnet, merged into the existing profile. The free limit is not
// checked here: it applied when the sign-in began.
func (e *Engine) finishSignInLocked(name string, st NodeStatus) {
	found := SignedIn{Suffix: strings.ToLower(strings.Trim(st.MagicDNSSuffix, ".")), Tailnet: st.Tailnet, Account: st.Account}
	cfg := e.cfg.Clone()
	pending := cfg.Profiles[name]
	existing := cfg.Owner("host." + found.Suffix)
	e.stopNodeLocked(name)

	if existing != "" && existing != name {
		ex := cfg.Profiles[existing]
		ex.Account = found.Account
		if found.Tailnet != "" {
			ex.Tailnet = found.Tailnet
		}
		ex.TailscaleLogs = ex.TailscaleLogs || pending.TailscaleLogs
		ex.AuthKey = ""
		delete(cfg.Profiles, name)
		e.stopNodeLocked(existing)
		if err := os.RemoveAll(e.nodeDir(existing)); err == nil {
			_ = os.Rename(e.nodeDir(name), e.nodeDir(existing))
		}
		_ = os.RemoveAll(e.nodeDir(name))
		if err := e.commitLocked(cfg); err != nil {
			e.opts.Logf("saving config: %v", err)
		}
		if !ex.Paused {
			e.startNodeLocked(existing)
		}
		e.opts.Logf("sign-in %s is tailnet %s, already listed as %s", name, found.Suffix, existing)
		e.reconcileLogsLocked()
		return
	}

	final := NameForTailnet(func(n string) bool { _, ok := cfg.Profiles[n]; return ok && n != name }, found)
	p := *pending
	p.Pending = false
	p.Created = time.Time{}
	p.Suffixes = []string{"." + found.Suffix}
	p.Account = found.Account
	p.Tailnet = found.Tailnet
	p.AuthKey = ""
	delete(cfg.Profiles, name)
	cfg.Profiles[final] = &p
	if err := cfg.Validate(); err != nil {
		e.opts.Logf("sign-in %s: %v", name, err)
		_ = e.removeProfileLocked(name)
		return
	}
	if final != name {
		_ = os.RemoveAll(e.nodeDir(final))
		if err := os.Rename(e.nodeDir(name), e.nodeDir(final)); err != nil {
			e.opts.Logf("sign-in %s: %v", name, err)
		}
	}
	if err := e.commitLocked(cfg); err != nil {
		e.opts.Logf("saving config: %v", err)
		return
	}
	e.startNodeLocked(final)
	e.opts.Logf("sign-in %s finished: tailnet %s as %s", name, found.Suffix, final)
}

// removeProfileLocked stops the node, deletes its keys and its local ports.
func (e *Engine) removeProfileLocked(name string) error {
	if _, ok := e.cfg.Profiles[name]; !ok {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	if e.Running() {
		e.stopNodeLocked(name)
	}
	cfg := e.cfg.Clone()
	delete(cfg.Profiles, name)
	kept := cfg.Tunnels[:0]
	for _, t := range cfg.Tunnels {
		if t.Profile != name {
			kept = append(kept, t)
		}
	}
	cfg.Tunnels = kept
	if err := e.commitLocked(cfg); err != nil {
		return err
	}
	if err := os.RemoveAll(e.nodeDir(name)); err != nil {
		return err
	}
	e.forgetPoolLocked(name)
	if e.Running() {
		e.tunnels.sync(cfg.Tunnels)
		e.reconcileLogsLocked()
	}
	return nil
}

func (e *Engine) learnLoop(stop chan struct{}) {
	defer e.loops.Done()
	wait := e.opts.LearnFirst
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		wait = e.opts.LearnInterval
		e.learnOnce(stop)
	}
}

// learnOnce asks each running tailnet what it serves and re-checks the
// remembered hosts.
func (e *Engine) learnOnce(stop chan struct{}) {
	e.mu.RLock()
	routes := e.routes
	type item struct {
		name string
		node Node
	}
	var items []item
	for n, ne := range e.nodes {
		if ne.node != nil && ne.status.State == StateRunning {
			items = append(items, item{n, ne.node})
		}
	}
	e.mu.RUnlock()
	if routes == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for _, it := range items {
		lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
		ips, domains, err := it.node.Learn(lctx)
		lcancel()
		if err == nil {
			routes.SetLearned(it.name, ips, domains)
		}
	}
	routes.RecheckHosts(ctx)
	e.changed()
}

// State is the state command's result.
type State struct {
	Up            bool     `json:"up"`
	Version       string   `json:"version"`
	Proxy         string   `json:"proxy"`
	PACURL        string   `json:"pac_url"`
	Unlocked      bool     `json:"unlocked"`
	FreeProfiles  int      `json:"free_profiles"`
	TailscaleLogs bool     `json:"tailscale_logs"`
	Passthrough   bool     `json:"passthrough"`
	MatchDomains  []string `json:"match_domains"`
	// All-apps mode (allapps.go). AllApps is the saved setting. The shell
	// uses all-apps network settings when AllApps is true and AllAppsError
	// is "", else proxy settings. AllAppsActive: the tun is attached.
	AllApps       bool           `json:"all_apps"`
	AllAppsActive bool           `json:"all_apps_active"`
	AllAppsError  string         `json:"all_apps_error"`
	Tunnel        TunnelSettings `json:"tunnel"`
	Profiles      []ProfileState `json:"profiles"`
	Tunnels       []Tunnel       `json:"tunnels"`
}

// ProfileState is one tailnet in State.
type ProfileState struct {
	Name     string   `json:"name"`
	Account  string   `json:"account"`
	Tailnet  string   `json:"tailnet"`
	Suffixes []string `json:"suffixes"`
	Hostname string   `json:"hostname"`
	IPs      []string `json:"ips"`
	Online   bool     `json:"online"`
	Paused   bool     `json:"paused"`
	Error    string   `json:"error"`
	AuthURL  string   `json:"auth_url"`
	// NeedsLogin is true only when a sign-in is known to be required.
	NeedsLogin bool `json:"needs_login"`
	// Connecting: the node is starting and needs no sign-in.
	Connecting bool `json:"connecting"`
	// Prefs are the saved Tailscale settings (set_options).
	Prefs Prefs `json:"prefs"`
}

// nodeStateFile is where a node keeps its keys (tsnet's own file name).
const nodeStateFile = "tailscaled.state"

// State reports the engine's state. While off it reads the config from disk.
func (e *Engine) State() State {
	e.mu.RLock()
	up := e.up
	cfg := e.cfg
	stateDir := e.stateDir
	e.mu.RUnlock()
	if !up && stateDir != "" {
		if c, err := LoadConfig(filepath.Join(stateDir, ConfigFile)); err == nil {
			cfg = c
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	st := State{
		Up: up, Version: Version, Proxy: e.opts.Listen, Unlocked: cfg.Unlocked,
		FreeProfiles: FreeProfiles, Profiles: []ProfileState{}, Tunnels: append([]Tunnel{}, cfg.Tunnels...),
		AllApps: !cfg.ProxyOnly, Tunnel: tunnelSettings(),
	}
	if up {
		st.Proxy = e.router.Addr()
		st.TailscaleLogs = e.logsMode
		st.Passthrough = e.passthrough.Load() && !e.aa.active
		st.AllAppsActive = e.aa.active
		st.AllAppsError = e.aa.err
		if st.AllApps && e.aa.mux == nil && st.AllAppsError == "" {
			st.AllAppsError = "all-apps mode is not available here"
		}
		st.MatchDomains = e.routes.MatchDomains()
	} else {
		rt := NewRouteTable("", nil)
		rt.SetConfig(cfg)
		st.MatchDomains = rt.MatchDomains()
	}
	if st.MatchDomains == nil {
		st.MatchDomains = []string{}
	}
	st.PACURL = "http://" + st.Proxy + "/tailfuse.pac"
	for _, name := range cfg.Names() {
		p := cfg.Profiles[name]
		ps := ProfileState{
			Name: name, Account: p.Account, Tailnet: p.Tailnet, Paused: p.Paused,
			Suffixes: append([]string{}, p.Suffixes...), IPs: []string{}, Prefs: p.Prefs,
		}
		ne := e.nodes[name]
		if !up {
			ne = nil
		}
		ps.NeedsLogin = p.SignedOut
		if ne == nil && !p.SignedOut && !p.Pending && !(up && shouldRun(p)) {
			// Nothing running to ask: a tailnet that has signed in before has node state on disk.
			if _, err := os.Stat(filepath.Join(e.nodeDir(name), nodeStateFile)); err != nil && os.IsNotExist(err) {
				ps.NeedsLogin = true
			}
		}
		if ne != nil {
			ps.Error = ne.err
			s := ne.status
			ps.Hostname = s.Hostname
			ps.IPs = append(ps.IPs, s.IPs...)
			ps.Online = s.State == StateRunning
			if !ps.Online && ps.Error == "" {
				ps.AuthURL = s.AuthURL
			}
			if ps.Online {
				ps.NeedsLogin = false
			} else if s.State == StateNeedsLogin || s.AuthURL != "" {
				ps.NeedsLogin = true
			}
			if ps.Account == "" {
				ps.Account = s.Account
			}
			if ps.Tailnet == "" {
				ps.Tailnet = s.Tailnet
			}
		}
		ps.Connecting = up && shouldRun(p) && !ps.Online && !ps.NeedsLogin && ps.Error == ""
		st.Profiles = append(st.Profiles, ps)
	}
	return st
}

// changed fires change events when the state or match_domains differ from
// what was last reported.
func (e *Engine) changed() {
	st := e.State()
	b, _ := json.Marshal(st)
	// "domains" means: re-apply the network settings. That is also needed
	// when the mode the shell should use changes.
	dom := strings.Join(st.MatchDomains, ",") + "|all_apps=" + strconv.FormatBool(st.AllApps && st.AllAppsError == "")
	e.nmu.Lock()
	first := e.lastState == nil
	stateChanged := !bytes.Equal(b, e.lastState)
	domChanged := dom != e.lastDomains || first
	e.lastState, e.lastDomains = b, dom
	e.nmu.Unlock()
	if domChanged {
		e.emit("domains")
	}
	if stateChanged {
		e.emit("state")
	}
}

// minEventGap coalesces events to at most a few a second.
const minEventGap = 250 * time.Millisecond

func (e *Engine) emit(kind string) {
	e.nmu.Lock()
	defer e.nmu.Unlock()
	e.pendKinds[kind] = true
	if e.armed {
		return
	}
	e.armed = true
	delay := minEventGap - time.Since(e.lastFire)
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, e.fire)
}

func (e *Engine) fire() {
	e.nmu.Lock()
	var kinds []string
	for k := range e.pendKinds {
		kinds = append(kinds, k)
	}
	e.pendKinds = map[string]bool{}
	e.armed = false
	e.lastFire = time.Now()
	l := e.listener
	e.nmu.Unlock()
	sort.Strings(kinds) // "domains" before "state"
	if l == nil {
		return
	}
	for _, k := range kinds {
		func() {
			defer func() { _ = recover() }()
			l(k)
		}()
	}
}

// Request is one Call request.
type Request struct {
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args,omitempty"`
}

type response struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result"`
	Error  string `json:"error,omitempty"`
}

type failResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Call runs one JSON request and returns one JSON response. It never panics
// and never returns an empty string.
func (e *Engine) Call(request string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fail(fmt.Errorf("internal error: %v", r))
		}
	}()
	var req Request
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		return fail(errors.New("bad request: not json"))
	}
	if len(req.Args) == 0 || string(req.Args) == "null" {
		req.Args = json.RawMessage("{}")
	}
	result, err := e.dispatch(req)
	if err != nil {
		return fail(err)
	}
	b, err := json.Marshal(response{OK: true, Result: result})
	if err != nil {
		return fail(err)
	}
	e.changed()
	return string(b)
}

func fail(err error) string {
	msg := err.Error()
	if msg != "" {
		msg = strings.ToLower(msg[:1]) + msg[1:]
	}
	b, _ := json.Marshal(failResponse{OK: false, Error: msg})
	return string(b)
}

func decodeArgs(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return errors.New("bad arguments: " + err.Error())
	}
	return nil
}

func (e *Engine) dispatch(req Request) (any, error) {
	switch req.Cmd {
	case "state":
		return e.State(), nil
	case "add_profile":
		var a struct {
			AuthKey string `json:"auth_key"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		name, err := e.AddProfile(a.AuthKey)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": name}, nil
	case "login":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return nil, e.Login(a.Name)
	case "set_connected":
		var a struct {
			Name string `json:"name"`
			On   *bool  `json:"on"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		if a.On == nil {
			return nil, errors.New("bad arguments: on is required")
		}
		return nil, e.SetConnected(a.Name, *a.On)
	case "remove_profile":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return nil, e.RemoveProfile(a.Name)
	case "test_host":
		var a struct {
			Host string `json:"host"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		host, owner, err := e.TestHost(a.Host)
		if err != nil {
			return nil, err
		}
		var o any
		if owner != "" {
			o = owner
		}
		out := map[string]any{"host": host, "owner": o}
		e.mu.RLock()
		routes, up := e.routes, e.up
		e.mu.RUnlock()
		if up && owner != "" && routes.ViaAddress(host) {
			if d := SuggestDomain(host); d != "" {
				out["via"] = "address"
				out["suggest_domain"] = d
			}
		}
		return out, nil
	case "add_tunnel":
		var t Tunnel
		if err := decodeArgs(req.Args, &t); err != nil {
			return nil, err
		}
		return nil, e.AddTunnel(t)
	case "remove_tunnel":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return nil, e.RemoveTunnel(a.Name)
	case "set_unlocked":
		var a struct {
			On *bool `json:"on"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		if a.On == nil {
			return nil, errors.New("bad arguments: on is required")
		}
		return nil, e.SetUnlocked(*a.On)
	case "devices":
		var a DevicesArgs
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return e.Devices(a)
	case "import_setup":
		var a struct {
			Link   string `json:"link"`
			DryRun bool   `json:"dry_run"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return e.ImportSetup(a.Link, a.DryRun)
	case "set_all_apps":
		var a struct {
			On *bool `json:"on"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		if a.On == nil {
			return nil, errors.New("bad arguments: on is required")
		}
		return nil, e.SetAllApps(*a.On)
	case "add_domain", "remove_domain":
		var a struct {
			Name   string `json:"name"`
			Domain string `json:"domain"`
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		if req.Cmd == "add_domain" {
			return nil, e.AddDomain(a.Name, a.Domain)
		}
		return nil, e.RemoveDomain(a.Name, a.Domain)
	case "set_options":
		var a struct {
			Name string `json:"name"`
			Prefs
		}
		if err := decodeArgs(req.Args, &a); err != nil {
			return nil, err
		}
		return nil, e.SetOptions(a.Name, a.Prefs)
	case "":
		return nil, errors.New("bad request: no cmd")
	default:
		return nil, fmt.Errorf("unknown command %q", req.Cmd)
	}
}

// ErrNeedsUpgrade is returned by AddProfile past the free limit.
var ErrNeedsUpgrade = errors.New("that needs the unlimited upgrade")

// AddProfile starts signing in to a new tailnet under a provisional name.
func (e *Engine) AddProfile(authKey string) (string, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if !e.Running() {
		return "", errOff
	}
	cfg := e.cfg.Clone()
	if len(cfg.Profiles) >= FreeProfiles && !cfg.Unlocked {
		return "", ErrNeedsUpgrade
	}
	name := "new-tailnet"
	for n := 2; cfg.Profiles[name] != nil; n++ {
		name = fmt.Sprintf("new-tailnet-%d", n)
	}
	_ = os.RemoveAll(e.nodeDir(name)) // keys left by an earlier, removed sign-in
	cfg.Profiles[name] = &Profile{Suffixes: []string{}, Pending: true, Created: e.opts.Now().UTC(), AuthKey: strings.TrimSpace(authKey)}
	if err := e.commitLocked(cfg); err != nil {
		return "", err
	}
	e.startNodeLocked(name)
	return name, nil
}

// Login starts a fresh sign-in for an existing tailnet.
func (e *Engine) Login(name string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if !e.Running() {
		return errOff
	}
	p := e.cfg.Profiles[name]
	if p == nil {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	if p.Paused || p.SignedOut {
		cfg := e.cfg.Clone()
		cfg.Profiles[name].Paused = false
		cfg.Profiles[name].SignedOut = false
		if err := e.commitLocked(cfg); err != nil {
			return err
		}
		e.reconcileLogsLocked()
	}
	e.mu.RLock()
	ne := e.nodes[name]
	e.mu.RUnlock()
	if ne == nil || ne.node == nil {
		e.stopNodeLocked(name)
		e.startNodeLocked(name)
		e.mu.RLock()
		ne = e.nodes[name]
		e.mu.RUnlock()
		if ne == nil || ne.node == nil {
			return fmt.Errorf("tailnet %q could not start", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return ne.node.Login(ctx)
}

// SetConnected switches one tailnet on or off.
func (e *Engine) SetConnected(name string, on bool) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	if e.cfg.Profiles[name] == nil {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	cfg := e.cfg.Clone()
	cfg.Profiles[name].Paused = !on
	if on {
		cfg.Profiles[name].SignedOut = false
	}
	if err := e.commitLocked(cfg); err != nil {
		return err
	}
	if !e.Running() {
		return nil
	}
	e.mu.RLock()
	_, running := e.nodes[name]
	e.mu.RUnlock()
	if on && !running {
		if wantLogs(e.cfg) != e.logsMode {
			e.reconcileLogsLocked() // starts it too
		} else {
			e.startNodeLocked(name)
		}
	} else if !on {
		e.stopNodeLocked(name)
		e.reconcileLogsLocked()
	}
	return nil
}

// SetOptions saves a tailnet's Tailscale settings and applies them to its node
// when it is running (otherwise they apply when it starts).
func (e *Engine) SetOptions(name string, prefs Prefs) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	if e.cfg.Profiles[name] == nil {
		return fmt.Errorf("there is no tailnet called %q", name)
	}
	cfg := e.cfg.Clone()
	cfg.Profiles[name].Prefs = prefs
	if err := e.commitLocked(cfg); err != nil {
		return err
	}
	if !e.Running() {
		return nil
	}
	e.mu.RLock()
	ne := e.nodes[name]
	e.mu.RUnlock()
	if ne != nil && ne.node != nil {
		e.applyPrefs(name, ne.node, prefs)
	}
	return nil
}

// RemoveProfile removes a tailnet: its node, keys and local ports.
func (e *Engine) RemoveProfile(name string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	return e.removeProfileLocked(name)
}

// TestHost reports which tailnet a hostname or URL goes to. While on it asks
// the router (which also knows what tailnets serve); off, suffix rules only.
func (e *Engine) TestHost(input string) (string, string, error) {
	host := hostFromInput(input)
	if host == "" {
		return "", "", errors.New("no host given")
	}
	e.mu.RLock()
	up, routes := e.up, e.routes
	e.mu.RUnlock()
	if up {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return host, routes.ResolveOwner(ctx, host), nil
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return "", "", err
	}
	return host, e.cfg.Owner(host), nil
}

// hostFromInput takes a hostname, host:port or URL and returns the host.
func hostFromInput(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			return normalizeHost(u.Hostname())
		}
		return ""
	}
	s, _, _ = strings.Cut(s, "/")
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	return normalizeHost(s)
}

// AddTunnel adds a local port, and starts it while on.
func (e *Engine) AddTunnel(t Tunnel) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	t.Name = strings.TrimSpace(t.Name)
	t.Target = strings.TrimSpace(t.Target)
	listen, err := normalizeTunnelListen(t.Listen)
	if err != nil {
		return fmt.Errorf("local port %q: %w", t.Name, err)
	}
	t.Listen = listen
	if p := e.cfg.Profiles[t.Profile]; p != nil && p.Pending {
		return fmt.Errorf("tailnet %q has not finished signing in", t.Profile)
	}
	if listen == e.opts.Listen || (e.router != nil && e.Running() && listen == e.router.Addr()) {
		return fmt.Errorf("local port %q: %s is the router's own port", t.Name, listen)
	}
	cfg := e.cfg.Clone()
	cfg.Tunnels = append(cfg.Tunnels, t)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if e.Running() {
		if err := e.tunnels.start(t); err != nil {
			return err
		}
	}
	if err := e.commitLocked(cfg); err != nil {
		if e.Running() {
			e.tunnels.stop(t.Name)
		}
		return err
	}
	return nil
}

// RemoveTunnel removes a local port.
func (e *Engine) RemoveTunnel(name string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	cfg := e.cfg.Clone()
	kept := cfg.Tunnels[:0]
	for _, t := range cfg.Tunnels {
		if t.Name != name {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(cfg.Tunnels) {
		return fmt.Errorf("there is no local port called %q", name)
	}
	cfg.Tunnels = kept
	if err := e.commitLocked(cfg); err != nil {
		return err
	}
	if e.Running() {
		e.tunnels.stop(name)
	}
	return nil
}

// SetUnlocked records whether the unlimited-tailnets purchase is held.
func (e *Engine) SetUnlocked(on bool) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return err
	}
	cfg := e.cfg.Clone()
	cfg.Unlocked = on
	return e.commitLocked(cfg)
}

// Device paging limits (the devices command).
const (
	devicesDefaultLimit = 50
	devicesMaxLimit     = 200
	devicesCacheTTL     = 5 * time.Second
)

// DevicesArgs are the devices command's arguments.
type DevicesArgs struct {
	Name    string `json:"name"`
	Query   string `json:"query"`
	Online  bool   `json:"online"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	Refresh bool   `json:"refresh"`
}

// DevicePage is one page of a tailnet's devices.
type DevicePage struct {
	Devices []Device `json:"devices"`
	Total   int      `json:"total"`
	Online  int      `json:"online"`
	Matched int      `json:"matched"`
}

// devCache is a tailnet's device list, reduced and sorted, kept for a few
// seconds so paging and search-as-you-type do not fetch the status each time.
type devCache struct {
	node   Node
	at     time.Time
	list   []Device
	online int
}

func (e *Engine) dropDevices(name string) {
	e.devMu.Lock()
	delete(e.devCaches, name)
	e.devMu.Unlock()
}

func (e *Engine) dropAllDevices() {
	e.devMu.Lock()
	e.devCaches = nil
	e.devMu.Unlock()
}

// sortDevices orders this device first, then online before offline, then by
// name (case-insensitive).
func sortDevices(ds []Device) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := &ds[i], &ds[j]
		if a.Self != b.Self {
			return a.Self
		}
		if a.Online != b.Online {
			return a.Online
		}
		if c := foldCompare(a.Name, b.Name); c != 0 {
			return c < 0
		}
		return a.DNSName < b.DNSName
	})
}

// foldCompare compares case-insensitively without allocating.
func foldCompare(a, b string) int {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			ra, rb = unicode.ToLower(ra), unicode.ToLower(rb)
			if ra != rb {
				if ra < rb {
					return -1
				}
				return 1
			}
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	}
	return 1
}

// containsFold reports whether s contains q (already lower-cased),
// ignoring case. ASCII queries match without allocating.
func containsFold(s, q string) bool {
	if q == "" {
		return true
	}
	ascii := true
	for i := 0; i < len(q); i++ {
		if q[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if !ascii {
		return strings.Contains(strings.ToLower(s), q)
	}
	for i := 0; i+len(q) <= len(s); i++ {
		j := 0
		for ; j < len(q); j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != q[j] {
				break
			}
		}
		if j == len(q) {
			return true
		}
	}
	return false
}

func (d *Device) matches(q string) bool {
	if containsFold(d.Name, q) || containsFold(d.DNSName, q) || containsFold(d.OS, q) {
		return true
	}
	for _, ip := range d.IPs {
		if containsFold(ip, q) {
			return true
		}
	}
	return false
}

// Devices returns one page of a tailnet's devices: this device first, then
// online ones, then by name; filtered by Query (name, DNS name, any address
// or OS) and Online.
func (e *Engine) Devices(a DevicesArgs) (*DevicePage, error) {
	name := a.Name
	e.mu.RLock()
	up := e.up
	_, exists := e.cfg.Profiles[name]
	var node Node
	running := false
	if ne := e.nodes[name]; ne != nil {
		node, running = ne.node, ne.status.State == StateRunning
	}
	e.mu.RUnlock()
	if !up {
		return nil, errOff
	}
	if !exists {
		return nil, fmt.Errorf("there is no tailnet called %q", name)
	}
	if node == nil || !running {
		return nil, fmt.Errorf("tailnet %q is not connected", name)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = devicesDefaultLimit
	}
	limit = min(limit, devicesMaxLimit)
	offset := max(a.Offset, 0)

	e.devMu.Lock()
	defer e.devMu.Unlock()
	c := e.devCaches[name]
	if c == nil || c.node != node || a.Refresh || e.opts.Now().Sub(c.at) > devicesCacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		devs, err := node.Devices(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
		sortDevices(devs)
		c = &devCache{node: node, at: e.opts.Now(), list: devs}
		for i := range devs {
			if devs[i].Online {
				c.online++
			}
		}
		if e.devCaches == nil {
			e.devCaches = map[string]*devCache{}
		}
		e.devCaches[name] = c
	}
	page := &DevicePage{Devices: []Device{}, Total: len(c.list), Online: c.online}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	for i := range c.list {
		d := &c.list[i]
		if (a.Online && !d.Online) || !d.matches(q) {
			continue
		}
		if page.Matched >= offset && len(page.Devices) < limit {
			page.Devices = append(page.Devices, *d)
		}
		page.Matched++
	}
	return page, nil
}
