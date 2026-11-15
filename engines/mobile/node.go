package engine

import (
	"context"
	"net"
	"net/netip"
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
	// Devices lists the other devices on the tailnet.
	Devices(ctx context.Context) ([]Device, error)
	// Learn reports what the tailnet serves: the Tailscale addresses of its
	// online devices (not this one) and the domains its DNS answers.
	Learn(ctx context.Context) (ips []netip.Addr, domains []string, err error)
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
}

// loggingRequired is what Tailscale reports when a tailnet's policy needs its
// logging left on (same check as the desktop core).
const loggingRequired = "requires logging"
