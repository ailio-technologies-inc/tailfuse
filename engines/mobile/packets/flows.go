package packets

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/types/ipproto"
)

// The flow table: one small entry per TCP connection, UDP association or
// ICMP echo id an app has open to a tailnet. It is all the per-connection
// state this process keeps.

// appKey is a flow as the app sees it: its own address and port, and the
// address and port it sent to (real or synthetic). For ICMP echo both ports
// are the echo id.
type appKey struct {
	proto    ipproto.Proto
	src, dst netip.AddrPort
}

// nodeKey is the same flow as the tailnet sees it: the peer's real address
// and port, and the port on this node's own address (the app's port).
type nodeKey struct {
	tailnet string
	proto   ipproto.Proto
	peer    netip.AddrPort
	local   uint16
}

type flow struct {
	app  appKey
	node nodeKey
	link *link
	real netip.Addr // the peer's real address
	self netip.Addr // the node's own address, same family

	// Guarded by flowTable.mu.
	last     time.Time
	replied  bool // a packet came back from the tailnet
	finApp   bool
	finNode  bool
	reset    bool
	packets  uint64
	refusing bool // removed; late packets are refused
}

var (
	errTableFull = errors.New("flow table full")
	errClash     = errors.New("flow clashes with another on the same tailnet")
)

type flowTable struct {
	mu     sync.Mutex
	lim    *Limits
	now    func() time.Time
	byApp  map[appKey]*flow
	byNode map[nodeKey]*flow
}

func newFlowTable(lim *Limits, now func() time.Time) *flowTable {
	return &flowTable{lim: lim, now: now, byApp: map[appKey]*flow{}, byNode: map[nodeKey]*flow{}}
}

// timeoutLocked is how long f may stay idle.
func (t *flowTable) timeoutLocked(f *flow) time.Duration {
	switch f.app.proto {
	case ipproto.TCP:
		switch {
		case f.reset || (f.finApp && f.finNode):
			return t.lim.TCPClosing
		case !f.replied:
			return t.lim.TCPOpening
		}
		return t.lim.TCPIdle
	case ipproto.UDP:
		return t.lim.UDPIdle
	}
	return t.lim.ICMPIdle
}

func (t *flowTable) expiredLocked(f *flow, now time.Time) bool {
	return now.Sub(f.last) >= t.timeoutLocked(f)
}

// fromApp finds the flow for an app packet and records its TCP flags. nil
// when there is none (or it expired).
func (t *flowTable) fromApp(k appKey, fin, rst bool) *flow {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.byApp[k]
	if f == nil {
		return nil
	}
	now := t.now()
	if t.expiredLocked(f, now) {
		t.removeLocked(f)
		return nil
	}
	f.last = now
	f.packets++
	f.finApp = f.finApp || fin
	f.reset = f.reset || rst
	return f
}

// fromNode is fromApp for a packet coming back from the tailnet.
func (t *flowTable) fromNode(k nodeKey, fin, rst bool) *flow {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.byNode[k]
	if f == nil {
		return nil
	}
	now := t.now()
	if t.expiredLocked(f, now) {
		t.removeLocked(f)
		return nil
	}
	f.last = now
	f.packets++
	f.replied = true
	f.finNode = f.finNode || fin
	f.reset = f.reset || rst
	return f
}

// insert adds f, first dropping expired flows when the table is full.
func (t *flowTable) insert(f *flow) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if old := t.byNode[f.node]; old != nil {
		if !t.expiredLocked(old, now) {
			return errClash
		}
		t.removeLocked(old)
	}
	if old := t.byApp[f.app]; old != nil {
		t.removeLocked(old)
	}
	if len(t.byApp) >= t.lim.MaxFlows {
		t.sweepLocked(now)
		if len(t.byApp) >= t.lim.MaxFlows {
			return errTableFull
		}
	}
	f.last = now
	t.byApp[f.app] = f
	t.byNode[f.node] = f
	return nil
}

func (t *flowTable) removeLocked(f *flow) {
	if t.byApp[f.app] == f {
		delete(t.byApp, f.app)
	}
	if t.byNode[f.node] == f {
		delete(t.byNode, f.node)
	}
	f.refusing = true
}

func (t *flowTable) sweepLocked(now time.Time) int {
	n := 0
	for _, f := range t.byApp {
		if t.expiredLocked(f, now) {
			t.removeLocked(f)
			n++
		}
	}
	return n
}

// sweep drops expired flows; returns how many.
func (t *flowTable) sweep() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sweepLocked(t.now())
}

// dropLink removes every flow through l (its node stopped).
func (t *flowTable) dropLink(l *link) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range t.byApp {
		if f.link == l {
			t.removeLocked(f)
		}
	}
}

func (t *flowTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byApp)
}
