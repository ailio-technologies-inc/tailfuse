package packets

import (
	"context"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tailscale/wireguard-go/tun"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// The memory harness. It runs on a computer (no tun without root), so the
// numbers are this Mac's, not an iPhone's; they compare designs and show
// how the cost grows with flows. Run with:
//
//	PACKETS_MEM=1 go test -run Memory -v ./packets/
//
// It applies the iOS memory settings (engine.IOSMemorySettings: 32 MiB soft
// limit, GC 50) so the heap behaves as in the extension.

func memGate(t *testing.T) {
	if os.Getenv("PACKETS_MEM") == "" {
		t.Skip("set PACKETS_MEM=1 to run the memory harness")
	}
	debug.SetMemoryLimit(32 << 20)
	debug.SetGCPercent(50)
}

type memSample struct{ heap, sys, rss uint64 }

func sample() memSample {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := memSample{heap: ms.HeapInuse + ms.StackInuse, sys: ms.Sys}
	if out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output(); err == nil {
		kb, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		s.rss = kb << 10
	}
	return s
}

func (a memSample) delta(b memSample) string {
	d := func(x, y uint64) string {
		return strconv.FormatFloat((float64(int64(x)-int64(y)))/(1<<20), 'f', 2, 64) + " MiB"
	}
	return "heap+stacks " + d(a.heap, b.heap) + ", rss " + d(a.rss, b.rss)
}

// TestMemoryMux: the recommended design. The Mux with N idle flows, then N
// flows moving packets both ways at once.
func TestMemoryMux(t *testing.T) {
	memGate(t)
	base := sample()
	r := newRig(t, rigOpts{noPhone: true, limits: Limits{MaxFlows: 20000}})
	r.rules.addr("100.64.0.2", "work")
	l := r.m.Link("work")
	r.m.SetAddrs("work", []netip.Addr{netip.MustParseAddr("100.64.0.1")})
	// The "tailnet": reflects every packet back as if the peer answered.
	var fromApps atomic.Uint64
	go func() {
		slab := make([]byte, 4096)
		pk := make([]tun.ReadPacket, 1)
		for {
			n, err := l.Read(slab, pk)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				p := slab[pk[i].Offset : pk[i].Offset+pk[i].Size]
				fromApps.Add(1)
				// swap src/dst and ports: the peer's reply
				src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[16:20])), uint16(p[22])<<8|uint16(p[23]))
				dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[12:16])), uint16(p[20])<<8|uint16(p[21]))
				l.Write([][]byte{buildUDP(src, dst, p[28:])}, 0)
			}
		}
	}()
	var toApps atomic.Uint64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-r.dev.Out():
				toApps.Add(1)
			case <-ctx.Done():
				return
			}
		}
	}()
	idle := sample()
	t.Logf("mux, 1 tailnet, no flows: %s", idle.delta(base))
	payload := make([]byte, 1200)
	for _, n := range []int{1000, 4000, 16000} {
		for i := r.m.Flows(); i < n; i++ {
			for !r.dev.Inject(udpPacket("198.18.0.1:"+strconv.Itoa(1024+i), "100.64.0.2:7", "x")) {
				time.Sleep(time.Millisecond)
			}
		}
		waitFor(t, func() bool { return r.m.Flows() >= n })
		s := sample()
		t.Logf("%5d idle flows: %s (%.0f bytes/flow heap)", n, s.delta(idle), float64(int64(s.heap)-int64(idle.heap))/float64(n))
	}
	// Active: 4000 flows, full-size packets, as fast as the path goes for
	// 2 s (8 senders, prebuilt packets, blocking on the tun queue like a
	// kernel would).
	pkts := make([][]byte, 4000)
	for i := range pkts {
		pkts[i] = udpPacket("198.18.0.1:"+strconv.Itoa(1024+i), "100.64.0.2:7", string(payload))
	}
	var wg sync.WaitGroup
	stop := time.Now().Add(2 * time.Second)
	in0, out0 := fromApps.Load(), toApps.Load()
	var peak atomic.Uint64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; time.Now().Before(stop); i += 8 {
				select {
				case r.dev.in <- pkts[i%len(pkts)]:
				case <-ctx.Done():
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if v := ms.HeapInuse + ms.StackInuse; v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	s := sample()
	t.Logf("4000 flows active (%.0f pkts/s = %.0f Mbit/s each way, 1228 B packets): peak heap+stacks %.2f MiB over idle; after: %s",
		float64(fromApps.Load()-in0)/2, float64(toApps.Load()-out0)/2*1228*8/1e6,
		float64(int64(peak.Load())-int64(idle.heap))/(1<<20), s.delta(idle))
}

// TestMemoryTermination: what terminating flows in a gVisor stack would
// cost per connection (the design this package does not use, and what each
// tsnet node's own netstack pays per proxied connection). Two stacks wired
// back to back with N connections, idle and then with 64 KiB queued each.
func TestMemoryTermination(t *testing.T) {
	memGate(t)
	for _, c := range []struct {
		name         string
		rxDef, rxMax int
		txDef, txMax int
	}{
		{"tailscale iOS/gVisor defaults (1 MiB default, 4 MiB max)", tcp.DefaultReceiveBufferSize, tcp.MaxBufferSize, tcp.DefaultSendBufferSize, tcp.MaxBufferSize},
		{"small (64 KiB default, 256 KiB max)", 64 << 10, 256 << 10, 64 << 10, 256 << 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := sample()
			a := newTestStack(t, netip.MustParseAddr("10.0.0.1"))
			b := newTestStack(t, netip.MustParseAddr("10.0.0.2"))
			for _, s := range []*testStack{a, b} {
				rx := tcpip.TCPReceiveBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: c.rxDef, Max: c.rxMax}
				tx := tcpip.TCPSendBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: c.txDef, Max: c.txMax}
				s.s.SetTransportProtocolOption(tcp.ProtocolNumber, &rx)
				s.s.SetTransportProtocolOption(tcp.ProtocolNumber, &tx)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.out(ctx, b.inject)
			b.out(ctx, a.inject)
			ln := b.listenTCP(t, netip.MustParseAddrPort("10.0.0.2:80"))
			var mu sync.Mutex
			var server []io.ReadWriteCloser
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					mu.Lock()
					server = append(server, c)
					mu.Unlock()
				}
			}()
			two := sample()
			t.Logf("two empty stacks: %s", two.delta(base))
			const n = 500
			var clients []io.ReadWriteCloser
			for i := 0; i < n; i++ {
				cc, err := a.dialTCP(ctx, netip.MustParseAddrPort("10.0.0.2:80"))
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, cc)
			}
			waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(server) == n })
			s := sample()
			t.Logf("%d idle connections: %s (%.1f KiB/conn)", n, s.delta(two), float64(int64(s.heap)-int64(two.heap))/n/1024)
			chunk := make([]byte, 64<<10)
			var wg sync.WaitGroup
			for _, cc := range clients {
				wg.Add(1)
				go func(cc io.ReadWriteCloser) { defer wg.Done(); cc.Write(chunk) }(cc)
			}
			wg.Wait()
			time.Sleep(200 * time.Millisecond)
			s2 := sample()
			t.Logf("%d connections with 64 KiB unread each: %s (%.1f KiB/conn)", n, s2.delta(two), float64(int64(s2.heap)-int64(two.heap))/n/1024)
			for _, cc := range clients {
				cc.Close()
			}
			mu.Lock()
			for _, sc := range server {
				sc.Close()
			}
			mu.Unlock()
		})
	}
}

// nullDevice drops writes without copying.
type nullDevice struct{ n atomic.Uint64 }

func (d *nullDevice) ReadPacket(b []byte) (int, error) { select {} }
func (d *nullDevice) WritePacket(pkt []byte) error     { d.n.Add(1); return nil }
func (d *nullDevice) Close() error                     { return nil }

// BenchmarkRoundTrip: one packet from an app to the tailnet and its reply
// back, through the Mux only (no harness copies). allocs/op is what the
// packet path allocates per round trip.
func BenchmarkRoundTrip(b *testing.B) {
	m, err := New(Config{Rules: newRules(), DNS: newDNS()})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	m.cfg.Rules.(*fakeRules).addr("100.64.0.2", "work")
	dev := &nullDevice{}
	m.mu.Lock()
	m.dev = dev
	m.mu.Unlock()
	lt := m.Link("work")
	m.SetAddrs("work", []netip.Addr{netip.MustParseAddr("100.64.0.1")})
	l := lt.(*link)
	out := udpPacket("198.18.0.1:5000", "100.64.0.2:7", string(make([]byte, 1200)))
	pristine := udpPacket("100.64.0.2:7", "100.64.0.1:5000", string(make([]byte, 1200)))
	back := append([]byte(nil), pristine...)
	slab := make([]byte, 4096)
	pk := make([]tun.ReadPacket, 1)
	bufs := [][]byte{nil}
	b.ReportAllocs()
	b.SetBytes(int64(len(out) + len(back)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pb := m.getBuf()
		pb.n = copy(pb.b, out)
		m.fromDevice(pb)
		if _, err := l.Read(slab, pk); err != nil {
			b.Fatal(err)
		}
		copy(back, pristine) // undo the last translation
		bufs[0] = back
		l.Write(bufs, 0)
	}
	if dev.n.Load() == 0 {
		b.Fatal("nothing written")
	}
}
