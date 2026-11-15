package engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

func fakeDevices(n int) []Device {
	oses := []string{"linux", "iOS", "macOS", "windows", "android"}
	out := make([]Device, n)
	for i := range out {
		name := fmt.Sprintf("Host-%05d", (i*7919)%n) // shuffled, mixed case below
		if i%3 == 0 {
			name = "host-" + name[5:]
		}
		out[i] = Device{Name: name, DNSName: name + ".tail1234.ts.net", IPs: []string{fmt.Sprintf("100.64.%d.%d", i/250, i%250+1), fmt.Sprintf("fd7a:115c:a1e0::%x", i)}, OS: oses[i%5], Online: i%4 == 0}
	}
	out[17].Self, out[17].Online = true, true
	return out
}

func devicesEngine(t *testing.T, devs []Device) *Engine {
	t.Helper()
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, dir)
	cfg := NewConfig()
	cfg.Profiles["example"] = &Profile{Suffixes: []string{".tail1234.ts.net"}}
	if err := cfg.Save(dir + "/" + ConfigFile); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	n := fn.node("example")
	n.signIn(fakeState{Suffix: "tail1234.ts.net", Account: "ada@example.com", Tailnet: "example.com"})
	n.mu.Lock()
	n.devices = devs
	n.mu.Unlock()
	eventually(t, "running", func() bool { p := profile(getState(t, e), "example"); return p != nil && p.Online })
	return e
}

func page(t *testing.T, e *Engine, args map[string]any) DevicePage {
	t.Helper()
	args["name"] = "example"
	var p DevicePage
	if err := json.Unmarshal(mustOK(t, e, "devices", args), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDevicesPagingFilterSort(t *testing.T) {
	e := devicesEngine(t, fakeDevices(10000))
	p := page(t, e, map[string]any{})
	if p.Total != 10000 || len(p.Devices) != 50 || p.Matched != 10000 {
		t.Fatalf("first page: total %d len %d matched %d", p.Total, len(p.Devices), p.Matched)
	}
	if !p.Devices[0].Self {
		t.Fatal("this device is not first")
	}
	wantOnline := 2501
	if p.Online != wantOnline {
		t.Fatalf("online %d want %d", p.Online, wantOnline)
	}
	// online first, then by name case-insensitively; walk every page.
	seen := map[string]bool{}
	var prev *Device
	for off := 0; off < 10000; off += 200 {
		pg := page(t, e, map[string]any{"limit": 500, "offset": off}) // 500 is clamped to 200
		if len(pg.Devices) != 200 {
			t.Fatalf("offset %d: %d devices", off, len(pg.Devices))
		}
		for i := range pg.Devices {
			d := &pg.Devices[i]
			if seen[d.DNSName] {
				t.Fatalf("duplicate %s", d.DNSName)
			}
			seen[d.DNSName] = true
			if prev != nil && !d.Self {
				if !prev.Online && d.Online {
					t.Fatalf("offline %s before online %s", prev.Name, d.Name)
				}
				if prev.Online == d.Online && !prev.Self && foldCompare(prev.Name, d.Name) > 0 {
					t.Fatalf("%s before %s", prev.Name, d.Name)
				}
			}
			prev = d
		}
	}
	if len(seen) != 10000 {
		t.Fatalf("saw %d devices", len(seen))
	}
	if pg := page(t, e, map[string]any{"offset": 9990, "limit": 50}); len(pg.Devices) != 10 {
		t.Fatalf("last page %d", len(pg.Devices))
	}
	if pg := page(t, e, map[string]any{"offset": 20000}); len(pg.Devices) != 0 || pg.Matched != 10000 {
		t.Fatalf("past the end %+v", pg)
	}
	// filters
	if pg := page(t, e, map[string]any{"online": true, "limit": 200}); pg.Matched != 2501 || pg.Total != 10000 {
		t.Fatalf("online only: matched %d", pg.Matched)
	}
	if pg := page(t, e, map[string]any{"query": "HOST-00042"}); pg.Matched != 1 || pg.Devices[0].Name == "" {
		t.Fatalf("name query: %+v", pg.Matched)
	}
	if pg := page(t, e, map[string]any{"query": "100.64.3.", "limit": 200}); pg.Matched != 250 {
		t.Fatalf("ip query: %d", pg.Matched)
	}
	if pg := page(t, e, map[string]any{"query": "ANDROID", "online": true}); pg.Matched != 500 {
		t.Fatalf("os+online: %d", pg.Matched) // i%5==4 and i%4==0 -> i%20==16: 500
	}
	if pg := page(t, e, map[string]any{"query": "nothing-like-this"}); pg.Matched != 0 || pg.Devices == nil || len(pg.Devices) != 0 {
		t.Fatalf("no match: %+v", pg)
	}
}

func TestDevicesCacheAndErrors(t *testing.T) {
	e := devicesEngine(t, fakeDevices(100))
	page(t, e, map[string]any{})
	if len(e.devCaches) != 1 {
		t.Fatal("not cached")
	}
	e.stopNodeLocked("example")
	if len(e.devCaches) != 0 {
		t.Fatal("cache kept after the node stopped")
	}
	mustFail(t, e, "devices", map[string]any{"name": "example"}, "not connected")
}

func benchEngine(devs []Device) *Engine {
	e := New(Options{})
	e.up = true
	e.cfg.Profiles["example"] = &Profile{}
	e.nodes["example"] = &nodeEntry{node: &fakeNode{devices: devs}, status: NodeStatus{State: StateRunning}}
	return e
}

// Cached: a keystroke or a next page (filter and slice over the reduced list).
func BenchmarkDevices10000Cached(b *testing.B) {
	e := benchEngine(fakeDevices(10000))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := e.Devices(DevicesArgs{Name: "example", Query: "host-0", Offset: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

// Refresh: refetch (the fake hands over a shuffled copy), sort, then page.
func BenchmarkDevices10000Refresh(b *testing.B) {
	src := fakeDevices(10000)
	e := benchEngine(nil)
	fnode := e.nodes["example"].node.(*fakeNode)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fnode.devices = append([]Device(nil), src...)
		if _, err := e.Devices(DevicesArgs{Name: "example", Refresh: true}); err != nil {
			b.Fatal(err)
		}
	}
}
