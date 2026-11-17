package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeNet stands in for Tailscale: each profile's "tailnet" is a real
// loopback HTTP server that answers "<profile> <host> <path>", so a test can
// see which tailnet a request landed on.
type fakeNet struct {
	t        *testing.T
	mu       sync.Mutex
	upstream map[string]string // profile -> address of its fake tailnet server
	nodes    []*fakeNode
	dials    []string // "profile address"
	servers  []*http.Server
}

func newFakeNet(t *testing.T) *fakeNet {
	f := &fakeNet{t: t, upstream: map[string]string{}}
	t.Cleanup(func() {
		for _, s := range f.servers {
			s.Close()
		}
	})
	return f
}

// serve gives profile a fake tailnet server.
func (f *fakeNet) serve(profile string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %s", profile, r.Host, r.URL.Path)
	})}
	go srv.Serve(ln)
	f.mu.Lock()
	f.upstream[profile] = ln.Addr().String()
	f.servers = append(f.servers, srv)
	f.mu.Unlock()
}

func (f *fakeNet) dial(ctx context.Context, profile, network, address string) (net.Conn, error) {
	f.mu.Lock()
	up, ok := f.upstream[profile]
	f.dials = append(f.dials, profile+" "+address)
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tailnet %s is not connected", profile)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, up)
}

func (f *fakeNet) lastDial() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dials) == 0 {
		return ""
	}
	return f.dials[len(f.dials)-1]
}

func (f *fakeNet) factory(opts NodeOptions) (Node, error) {
	n := &fakeNode{net: f, opts: opts}
	f.mu.Lock()
	f.nodes = append(f.nodes, n)
	f.mu.Unlock()
	return n, nil
}

// node returns the newest node made for profile name.
func (f *fakeNet) node(name string) *fakeNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.nodes) - 1; i >= 0; i-- {
		if f.nodes[i].opts.Name == name {
			return f.nodes[i]
		}
	}
	return nil
}

func (f *fakeNet) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.nodes {
		if x.opts.Name == name {
			n++
		}
	}
	return n
}

// fakeState is what a fake node keeps in its state folder ("its keys").
type fakeState struct {
	Suffix       string `json:"suffix"`
	Account      string `json:"account"`
	Tailnet      string `json:"tailnet"`
	RequiresLogs bool   `json:"requires_logs"`
}

type fakeNode struct {
	net  *fakeNet
	opts NodeOptions

	mu       sync.Mutex
	started  bool
	closed   bool
	logins   int
	devices  []Device
	learnIPs []netip.Addr
	learnDom []string
	prefs    []Prefs // every SetPrefs call, newest last
	prefsErr error   // returned by SetPrefs while set (e.g. exit node not known yet)
	ff       *fakeFiles
}

func (n *fakeNode) SetPrefs(ctx context.Context, p Prefs) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.prefsErr != nil {
		return n.prefsErr
	}
	n.prefs = append(n.prefs, p)
	return nil
}

// Taildrop on the fake node: devices it can send to, what it was sent, and
// files waiting for it.
type fakeFiles struct {
	targets []FileTarget
	sent    map[string][]byte // target+"/"+name -> bytes
	waiting map[string][]byte
}

func (n *fakeNode) files() *fakeFiles {
	if n.ff == nil {
		n.ff = &fakeFiles{sent: map[string][]byte{}, waiting: map[string][]byte{}}
	}
	return n.ff
}

func (n *fakeNode) FileTargets(ctx context.Context) ([]FileTarget, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.files().targets, nil
}

func (n *fakeNode) SendFile(ctx context.Context, target, name string, size int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("sent %d bytes, expected %d", len(b), size)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.files().sent[target+"/"+name] = b
	return nil
}

func (n *fakeNode) WaitingFiles(ctx context.Context) ([]WaitingFile, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []WaitingFile
	for name, b := range n.files().waiting {
		out = append(out, WaitingFile{Name: name, Size: int64(len(b))})
	}
	return out, nil
}

func (n *fakeNode) ReceiveFile(ctx context.Context, name string, w io.Writer) error {
	n.mu.Lock()
	b, ok := n.files().waiting[name]
	n.mu.Unlock()
	if !ok {
		return fmt.Errorf("no waiting file %q", name)
	}
	_, err := w.Write(b)
	return err
}

func (n *fakeNode) DeleteWaitingFile(ctx context.Context, name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.files().waiting, name)
	return nil
}

func (n *fakeNode) lastPrefs() (Prefs, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.prefs) == 0 {
		return Prefs{}, 0
	}
	return n.prefs[len(n.prefs)-1], len(n.prefs)
}

func (n *fakeNode) stateFile() string { return filepath.Join(n.opts.Dir, "fake.json") }

func (n *fakeNode) load() (fakeState, bool) {
	var s fakeState
	data, err := os.ReadFile(n.stateFile())
	if err != nil {
		return s, false
	}
	return s, json.Unmarshal(data, &s) == nil
}

// signIn simulates the person finishing the browser sign-in.
func (n *fakeNode) signIn(s fakeState) {
	data, _ := json.Marshal(s)
	if err := os.WriteFile(n.stateFile(), data, 0o600); err != nil {
		n.net.t.Fatal(err)
	}
}

func (n *fakeNode) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.started = true
	return nil
}

func (n *fakeNode) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

func (n *fakeNode) isClosed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closed
}

func (n *fakeNode) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return n.net.dial(ctx, n.opts.Name, network, address)
}

func (n *fakeNode) Status(ctx context.Context) (NodeStatus, error) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return NodeStatus{}, errors.New("closed")
	}
	s, ok := n.load()
	if !ok {
		return NodeStatus{State: StateNeedsLogin, AuthURL: "https://login.example/a/" + n.opts.Name}, nil
	}
	st := NodeStatus{
		Hostname: n.opts.Hostname, IPs: []string{"100.64.0.7"},
		Account: s.Account, Tailnet: s.Tailnet, MagicDNSSuffix: s.Suffix,
	}
	if s.RequiresLogs && !n.opts.Logs {
		st.State = StateStopped
		st.Health = []string{"tailnet requires logging to be enabled. Remove --no-logs-no-support from tailscaled command line."}
		return st, nil
	}
	st.State = StateRunning
	return st, nil
}

func (n *fakeNode) Login(ctx context.Context) error {
	n.mu.Lock()
	n.logins++
	n.mu.Unlock()
	return nil
}

func (n *fakeNode) Devices(ctx context.Context) ([]Device, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.devices, nil
}

func (n *fakeNode) Learn(ctx context.Context) ([]netip.Addr, []string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.learnIPs, n.learnDom, nil
}

// fakeResolver answers from a fixed table.
type fakeResolver struct {
	mu      sync.Mutex
	m       map[string][]netip.Addr
	lookups []string // every name asked for, to prove a name never reached DNS
}

func (r *fakeResolver) asked(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.lookups {
		if h == host {
			return true
		}
	}
	return false
}

func (r *fakeResolver) set(host string, ips ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string][]netip.Addr{}
	}
	var out []netip.Addr
	for _, s := range ips {
		out = append(out, netip.MustParseAddr(s))
	}
	r.m[host] = out
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, host)
	if a, ok := r.m[host]; ok && len(a) > 0 {
		return a, nil
	}
	return nil, errors.New("no such host")
}
