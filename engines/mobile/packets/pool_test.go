package packets

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func TestPoolStableAndPersisted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p := NewPool(PoolOptions{File: file})
	real := netip.MustParseAddr("100.64.0.2")
	a1, err := p.Get("work", real)
	if err != nil {
		t.Fatal(err)
	}
	if !SyntheticV4.Contains(a1) || a1 == TunnelV4 || a1 == DNSV4 || !p.Contains(a1) {
		t.Fatalf("v4 synthetic %v", a1)
	}
	b1, _ := p.Get("home", real)
	if b1 == a1 {
		t.Fatal("two tailnets got the same synthetic address")
	}
	if again, _ := p.Get("work", real); again != a1 {
		t.Fatalf("not stable: %v then %v", a1, again)
	}
	v6, err := p.Get("work", netip.MustParseAddr("fd7a:115c:a1e0::2"))
	if err != nil || !SyntheticV6.Contains(v6) || v6 == TunnelV6 || v6 == DNSV6 {
		t.Fatalf("v6 synthetic %v %v", v6, err)
	}
	if tn, r, ok := p.Lookup(a1); !ok || tn != "work" || r != real {
		t.Fatalf("lookup %v: %v %v %v", a1, tn, r, ok)
	}
	if _, _, ok := p.Lookup(netip.MustParseAddr("198.18.200.1")); ok {
		t.Fatal("lookup of an unused address")
	}

	// A new run keeps the same addresses.
	p2 := NewPool(PoolOptions{File: file})
	if p2.Len() != 3 {
		t.Fatalf("reloaded %d entries", p2.Len())
	}
	if again, _ := p2.Get("work", real); again != a1 {
		t.Fatalf("not stable across runs: %v then %v", a1, again)
	}
	if again, _ := p2.Get("home", real); again != b1 {
		t.Fatalf("not stable across runs: %v then %v", b1, again)
	}
	// and does not hand a kept address to someone else
	c, _ := p2.Get("other", real)
	if c == a1 || c == b1 {
		t.Fatalf("reused %v", c)
	}

	p2.Forget("work")
	if _, _, ok := p2.Lookup(a1); ok {
		t.Fatal("forgotten tailnet still mapped")
	}
	if p3 := NewPool(PoolOptions{File: file}); p3.Len() != 2 {
		t.Fatalf("after forget: %d entries on disk", p3.Len())
	}
}

func TestPoolExhaustionAndReclaim(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	p := NewPool(PoolOptions{
		V4Base: netip.MustParseAddr("198.18.1.0"), V4Size: 3,
		ReclaimAfter: time.Hour, Now: clock,
	})
	var got []netip.Addr
	for i := 1; i <= 3; i++ {
		a, err := p.Get("t", netip.AddrFrom4([4]byte{100, 64, 0, byte(i)}))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
		now = now.Add(time.Minute)
	}
	if got[0] != netip.MustParseAddr("198.18.1.0") || got[2] != netip.MustParseAddr("198.18.1.2") {
		t.Fatalf("allocation order %v", got)
	}
	if _, err := p.Get("t", netip.MustParseAddr("100.64.0.4")); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("want ErrPoolFull, got %v", err)
	}
	// The first entry has been unused long enough: it is reclaimed, the
	// others (recently used) are kept.
	p.Lookup(got[1])
	p.Lookup(got[2])
	now = now.Add(2 * time.Hour)
	p.Lookup(got[1])
	p.Lookup(got[2])
	a, err := p.Get("t", netip.MustParseAddr("100.64.0.4"))
	if err != nil || a != got[0] {
		t.Fatalf("reclaim: %v %v", a, err)
	}
	if _, r, _ := p.Lookup(got[1]); r != netip.MustParseAddr("100.64.0.2") {
		t.Fatal("a recently used entry was reclaimed")
	}
	// MaxEntries caps it too.
	q := NewPool(PoolOptions{MaxEntries: 1})
	q.Get("t", netip.MustParseAddr("100.64.0.1"))
	if _, err := q.Get("t", netip.MustParseAddr("100.64.0.2")); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("MaxEntries: %v", err)
	}
}
