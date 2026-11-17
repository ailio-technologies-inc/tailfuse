package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/tailscale/wireguard-go/tun"
)

// Node is one tailnet's embedded Tailscale node. The real one is a
// tsnet.Server (tsnode.go); tests use fakes.
type Node interface {
	// Start brings the node up. It returns once the node is running locally;
	// signing in and connecting continue in the background.
	Start() error
	// Close shuts the node down.
	Close() error
	// Dial connects to address on the tailnet.
	Dial(ctx context.Context, network, address string) (net.Conn, error)
	// Status reports the node's state.
	Status(ctx context.Context) (NodeStatus, error)
	// Login starts a fresh interactive sign-in; its URL appears in Status.
	Login(ctx context.Context) error
	// Devices lists the devices on the tailnet, this one included (Self).
	Devices(ctx context.Context) ([]Device, error)
	// Learn reports what the tailnet serves: the Tailscale addresses of its
	// online devices (not this one) and the domains its DNS answers.
	Learn(ctx context.Context) (ips []netip.Addr, domains []string, err error)
}

// Prefs are the Tailscale settings a person can change on a node.
type Prefs struct {
	// ExitNode is a device's name or Tailscale address to send all traffic
	// through; "" for none.
	ExitNode string `json:"exit_node,omitempty"`
	// AcceptRoutes uses the subnet routes other devices advertise.
	AcceptRoutes bool `json:"accept_routes,omitempty"`
	// AdvertiseRoutes are subnets (CIDR) this device offers the tailnet.
	AdvertiseRoutes []string `json:"advertise_routes,omitempty"`
	// AdvertiseExitNode offers this device as an exit node.
	AdvertiseExitNode bool `json:"advertise_exit_node,omitempty"`
	// ShieldsUp blocks incoming connections from the tailnet.
	ShieldsUp bool `json:"shields_up,omitempty"`
}

// IsZero reports whether nothing is set, so there is nothing to apply.
func (p Prefs) IsZero() bool {
	return p.ExitNode == "" && !p.AcceptRoutes && len(p.AdvertiseRoutes) == 0 && !p.AdvertiseExitNode && !p.ShieldsUp
}

// Validate checks the values before they are saved.
func (p Prefs) Validate() error {
	for _, r := range p.AdvertiseRoutes {
		if _, err := netip.ParsePrefix(strings.TrimSpace(r)); err != nil {
			return fmt.Errorf("%q is not a subnet like 10.0.0.0/24", r)
		}
	}
	return nil
}

// PrefsNode is a Node whose Tailscale settings can be changed (the real node
// can; some test fakes cannot). SetPrefs returns an error while the exit node
// is not yet among the tailnet's devices, so callers retry.
type PrefsNode interface {
	SetPrefs(ctx context.Context, p Prefs) error
}

// NodeOptions configures a new Node.
type NodeOptions struct {
	Name     string // profile name
	Dir      string // state folder, <stateDir>/nodes/<name>
	Hostname string // this device's name on the tailnet
	AuthKey  string // optional
	// Logs: Tailscale's log upload is allowed for this process (a tailnet
	// requires it). When false, nothing is uploaded.
	Logs bool
	Logf func(format string, args ...any)
	// Tun, in all-apps mode, is the device the node's WireGuard engine
	// reads apps' packets from and writes the tailnet's to (tsnet.Server.Tun,
	// from packets.Mux.Link). nil in proxy mode: the node is exactly as
	// before, its traffic only through Dial.
	Tun tun.Device
}

// DNSQuerier is a Node that can ask its tailnet's own resolver (MagicDNS and
// split DNS) directly; the all-apps DNS responder needs it. raw is the DNS
// response.
type DNSQuerier interface {
	QueryDNS(ctx context.Context, name, qtype string) (raw []byte, err error)
}

// NodeFactory makes a Node; it must not start it.
type NodeFactory func(NodeOptions) (Node, error)

// Backend states, as Tailscale names them.
const (
	StateNoState          = "NoState"
	StateNeedsLogin       = "NeedsLogin"
	StateNeedsMachineAuth = "NeedsMachineAuth"
	StateStopped          = "Stopped"
	StateStarting         = "Starting"
	StateRunning          = "Running"
)

// NodeStatus is what a node reports about itself.
type NodeStatus struct {
	State          string
	AuthURL        string
	Hostname       string
	IPs            []string
	Account        string
	Tailnet        string
	MagicDNSSuffix string // without leading or trailing dot
	Health         []string
}

// Device is another device on a tailnet (the devices command).
type Device struct {
	Name    string   `json:"name"`
	DNSName string   `json:"dns_name"`
	IPs     []string `json:"ips"`
	OS      string   `json:"os"`
	Online  bool     `json:"online"`
	Self    bool     `json:"self,omitempty"` // this device
}

// loggingRequired is what Tailscale reports when a tailnet's policy needs its
// logging left on (same check as the desktop core).
const loggingRequired = "requires logging"
