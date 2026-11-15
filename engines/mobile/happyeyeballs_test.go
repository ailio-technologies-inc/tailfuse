package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// dialLog is a directDial that fails or hangs per address and connects the rest.
type dialLog struct {
	mu     sync.Mutex
	tried  []string
	fail   map[string]error // immediate failure
	hang   map[string]bool  // blocks until cancelled
	pipes  []net.Conn
	closed int
}

func (d *dialLog) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.tried = append(d.tried, addr)
	err, hang := d.fail[addr], d.hang[addr]
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a, b := net.Pipe()
	d.mu.Lock()
	d.pipes = append(d.pipes, b)
	d.mu.Unlock()
	return a, nil
}

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestDialDirectFallsBackToIPv4(t *testing.T) {
	unreachable := errors.New("connect: network is unreachable")
	v6, v4 := "[2600:1f13:37c::1]:80", "34.223.124.45:80"

	t.Run("v6 fails at once", func(t *testing.T) {
		d := &dialLog{fail: map[string]error{v6: unreachable}}
		r := &Router{ctx: context.Background(), logf: t.Logf, directDial: d.dial}
		start := time.Now()
		c, err := r.dialDirect([]netip.AddrPort{ap(v6), ap(v4)})
		if err != nil {
			t.Fatalf("dial: %v (tried %v)", err, d.tried)
		}
		c.Close()
		if el := time.Since(start); el > eyeballsDelay {
			t.Fatalf("fell back after %v, want well under %v", el, eyeballsDelay)
		}
	})

	t.Run("v6 hangs: v4 starts after the delay", func(t *testing.T) {
		d := &dialLog{hang: map[string]bool{v6: true}}
		r := &Router{ctx: context.Background(), logf: t.Logf, directDial: d.dial}
		start := time.Now()
		c, err := r.dialDirect([]netip.AddrPort{ap(v6), ap(v4)})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.Close()
		el := time.Since(start)
		if el < eyeballsDelay/2 || el > 3*time.Second {
			t.Fatalf("took %v", el)
		}
	})

	t.Run("v4 listed second but v6 first resolver order is interleaved", func(t *testing.T) {
		got := interleave([]netip.AddrPort{ap("[2001:db8::1]:80"), ap("[2001:db8::2]:80"), ap("192.0.2.1:80"), ap("192.0.2.2:80")})
		want := []string{"[2001:db8::1]:80", "192.0.2.1:80", "[2001:db8::2]:80", "192.0.2.2:80"}
		for i, w := range want {
			if got[i].String() != w {
				t.Fatalf("order %v, want %v", got, want)
			}
		}
	})

	t.Run("everything fails: the last error comes back", func(t *testing.T) {
		d := &dialLog{fail: map[string]error{v6: unreachable, v4: errors.New("refused")}}
		r := &Router{ctx: context.Background(), logf: t.Logf, directDial: d.dial}
		if _, err := r.dialDirect([]netip.AddrPort{ap(v6), ap(v4)}); err == nil {
			t.Fatal("want an error")
		}
		if len(d.tried) != 2 {
			t.Fatalf("tried %v", d.tried)
		}
	})
}

// familyResolver answers "ip" with only the AAAA record, as some resolvers do for a dual-stack name,
// and the A record only when asked for ip4.
type familyResolver struct{}

func (familyResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	switch network {
	case "ip4":
		return []netip.Addr{netip.MustParseAddr("34.223.124.45")}, nil
	default:
		return []netip.Addr{netip.MustParseAddr("2600:1f13:37c::1")}, nil
	}
}

func TestPassthroughAddsTheOtherFamily(t *testing.T) {
	rt := NewRouteTable("", familyResolver{})
	r := &Router{routes: rt, ctx: context.Background(), logf: t.Logf}
	targets, oe := r.directTargets("neverssl.example", "80")
	if oe != nil {
		t.Fatalf("refused: %v", oe.msg)
	}
	var has4, has6 bool
	for _, a := range targets {
		if a.Addr().Is4() {
			has4 = true
		} else {
			has6 = true
		}
	}
	if !has4 || !has6 {
		t.Fatalf("targets %v: want both families", targets)
	}
}
