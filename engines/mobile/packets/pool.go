package packets

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"
)

// Tailfuse's own address space inside the tunnel.
//
// IPv4: 198.18.0.0/15 is reserved for network benchmarking (RFC 2544): never
// a real destination, never handed out by carriers (they use 100.64.0.0/10
// and 10.0.0.0/8) or home routers (192.168.0.0/16), never a Tailscale
// address. The iOS shell already uses 198.18.0.1 as the tunnel's own address.
//
// IPv6: a unique local /48 (RFC 4193) with a random global ID, so it cannot
// clash with Tailscale's fd7a:115c:a1e0::/48 or another ULA by accident.
var (
	SyntheticV4 = netip.MustParsePrefix("198.18.0.0/15")
	SyntheticV6 = netip.MustParsePrefix("fd5e:9b07:4a1c::/48")

	// TunnelV4 and TunnelV6 are the tunnel interface's own addresses: the
	// source of every packet an app sends into the tunnel.
	TunnelV4 = netip.MustParseAddr("198.18.0.1")
	TunnelV6 = netip.MustParseAddr("fd5e:9b07:4a1c::1")
	// DNSV4 and DNSV6 are where the responder listens (UDP and TCP port 53).
	DNSV4 = netip.MustParseAddr("198.18.0.2")
	DNSV6 = netip.MustParseAddr("fd5e:9b07:4a1c::2")

	// Synthetic addresses handed out to devices start here; everything below
	// (198.18.0.0/24, fd5e:9b07:4a1c::/64) is reserved for the tunnel itself.
	poolV4Base = netip.MustParseAddr("198.18.1.0")
	poolV4Size = uint64(1<<17 - 256 - 1) // up to 198.19.255.254
	poolV6Base = netip.MustParseAddr("fd5e:9b07:4a1c:1::")
	poolV6Size = uint64(1 << 32)
)

// ErrPoolFull: every synthetic address is in recent use.
var ErrPoolFull = errors.New("packets: synthetic address pool is full")

// PoolOptions configures a Pool. Zero values give the defaults.
type PoolOptions struct {
	// File keeps the mapping between runs ("" keeps it in memory only).
	File string
	// MaxEntries caps the mapping per address family (default 4096).
	MaxEntries int
	// ReclaimAfter: an entry unused this long may be given to another device
	// when the pool is full (default 1 hour). An answer handed out by DNS
	// lives at most a few minutes, so a reclaimed address is never one an app
	// still holds from a fresh answer.
	ReclaimAfter time.Duration
	// V4Base/V4Size, V6Base/V6Size override the ranges (tests).
	V4Base, V6Base netip.Addr
	V4Size, V6Size uint64
	Now            func() time.Time
}

// Pool gives a stable synthetic address to a (tailnet, real address) pair,
// for devices whose real address another tailnet also uses. The same pair
// gets the same address for as long as the entry lives (and across runs,
// when a File is set). Safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	opts    PoolOptions
	bySynth map[netip.Addr]*poolEntry
	byKey   map[poolKey]*poolEntry
	cursor  [2]uint64 // next offset to try, per family (0: v4, 1: v6)
	count   [2]int
}

type poolKey struct {
	tailnet string
	real    netip.Addr
}

type poolEntry struct {
	Tailnet string     `json:"tailnet"`
	Real    netip.Addr `json:"real"`
	Addr    netip.Addr `json:"addr"`
	used    time.Time
}

// NewPool returns a pool, loading File if it exists. A malformed or foreign
// entry in the file is skipped.
func NewPool(opts PoolOptions) *Pool {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 4096
	}
	if opts.ReclaimAfter <= 0 {
		opts.ReclaimAfter = time.Hour
	}
	if !opts.V4Base.IsValid() {
		opts.V4Base, opts.V4Size = poolV4Base, poolV4Size
	}
	if !opts.V6Base.IsValid() {
		opts.V6Base, opts.V6Size = poolV6Base, poolV6Size
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Pool{opts: opts, bySynth: map[netip.Addr]*poolEntry{}, byKey: map[poolKey]*poolEntry{}}
	if opts.File != "" {
		if data, err := os.ReadFile(opts.File); err == nil {
			var list []*poolEntry
			if json.Unmarshal(data, &list) == nil {
				now := opts.Now()
				for _, e := range list {
					if e == nil || e.Tailnet == "" || !e.Real.IsValid() || !p.inRange(e.Addr) ||
						e.Real.Is4() != e.Addr.Is4() || p.bySynth[e.Addr] != nil {
						continue
					}
					f := fam(e.Addr)
					if p.count[f] >= opts.MaxEntries {
						continue
					}
					e.Real = e.Real.Unmap()
					e.used = now // a fresh grace period after a restart
					k := poolKey{e.Tailnet, e.Real}
					if p.byKey[k] != nil {
						continue
					}
					p.bySynth[e.Addr], p.byKey[k] = e, e
					p.count[f]++
				}
			}
		}
	}
	return p
}

func fam(a netip.Addr) int {
	if a.Is4() {
		return 0
	}
	return 1
}

func (p *Pool) base(f int) (netip.Addr, uint64) {
	if f == 0 {
		return p.opts.V4Base, p.opts.V4Size
	}
	return p.opts.V6Base, p.opts.V6Size
}

func (p *Pool) inRange(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	base, size := p.base(fam(a))
	off, ok := offset(base, a)
	return ok && off < size
}

// Contains reports whether a is a synthetic address this pool can hand out.
func (p *Pool) Contains(a netip.Addr) bool { return p.inRange(a.Unmap()) }

// Get returns the synthetic address for real on tailnet, allocating one if
// needed. The address has real's family.
func (p *Pool) Get(tailnet string, real netip.Addr) (netip.Addr, error) {
	real = real.Unmap()
	if !real.IsValid() || tailnet == "" {
		return netip.Addr{}, errors.New("packets: bad pool key")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	k := poolKey{tailnet, real}
	if e := p.byKey[k]; e != nil {
		e.used = now
		return e.Addr, nil
	}
	f := fam(real)
	base, size := p.base(f)
	if (p.count[f] >= p.opts.MaxEntries || uint64(p.count[f]) >= size) && !p.reclaimLocked(f, now) {
		return netip.Addr{}, ErrPoolFull
	}
	for tries := uint64(0); tries < size; tries++ {
		off := p.cursor[f] % size
		p.cursor[f] = off + 1
		a := add(base, off)
		if p.bySynth[a] != nil {
			continue
		}
		e := &poolEntry{Tailnet: tailnet, Real: real, Addr: a, used: now}
		p.bySynth[a], p.byKey[k] = e, e
		p.count[f]++
		p.saveLocked()
		return a, nil
	}
	return netip.Addr{}, ErrPoolFull
}

// reclaimLocked frees the least recently used entry of family f if it has
// been unused for ReclaimAfter.
func (p *Pool) reclaimLocked(f int, now time.Time) bool {
	var oldest *poolEntry
	for _, e := range p.bySynth {
		if fam(e.Addr) == f && (oldest == nil || e.used.Before(oldest.used)) {
			oldest = e
		}
	}
	if oldest == nil || now.Sub(oldest.used) < p.opts.ReclaimAfter {
		return false
	}
	p.removeLocked(oldest)
	return true
}

func (p *Pool) removeLocked(e *poolEntry) {
	delete(p.bySynth, e.Addr)
	delete(p.byKey, poolKey{e.Tailnet, e.Real})
	p.count[fam(e.Addr)]--
}

// Lookup maps a synthetic address back to its tailnet and real address, and
// marks it used.
func (p *Pool) Lookup(synth netip.Addr) (tailnet string, real netip.Addr, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.bySynth[synth.Unmap()]
	if e == nil {
		return "", netip.Addr{}, false
	}
	e.used = p.opts.Now()
	return e.Tailnet, e.Real, true
}

// Forget drops every entry of a tailnet (removed from the config).
func (p *Pool) Forget(tailnet string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for _, e := range p.bySynth {
		if e.Tailnet == tailnet {
			p.removeLocked(e)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Len is the number of entries.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bySynth)
}

func (p *Pool) saveLocked() {
	if p.opts.File == "" {
		return
	}
	list := make([]*poolEntry, 0, len(p.bySynth))
	for _, e := range p.bySynth {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Addr.Less(list[j].Addr) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := p.opts.File + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p.opts.File)
	}
}

// add returns base+off. For IPv6 off is added to the low 64 bits.
func add(base netip.Addr, off uint64) netip.Addr {
	if base.Is4() {
		b := base.As4()
		v := binary.BigEndian.Uint32(b[:]) + uint32(off)
		binary.BigEndian.PutUint32(b[:], v)
		return netip.AddrFrom4(b)
	}
	b := base.As16()
	v := binary.BigEndian.Uint64(b[8:]) + off
	binary.BigEndian.PutUint64(b[8:], v)
	return netip.AddrFrom16(b)
}

// offset is a-base when a is at or above base in the same family (and, for
// IPv6, in the same /64).
func offset(base, a netip.Addr) (uint64, bool) {
	if base.Is4() != a.Is4() {
		return 0, false
	}
	if base.Is4() {
		x, y := base.As4(), a.As4()
		bv, av := binary.BigEndian.Uint32(x[:]), binary.BigEndian.Uint32(y[:])
		if av < bv {
			return 0, false
		}
		return uint64(av - bv), true
	}
	x, y := base.As16(), a.As16()
	if [8]byte(x[:8]) != [8]byte(y[:8]) {
		return 0, false
	}
	bv, av := binary.BigEndian.Uint64(x[8:]), binary.BigEndian.Uint64(y[8:])
	if av < bv {
		return 0, false
	}
	return av - bv, true
}
