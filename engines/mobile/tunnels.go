package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// tunnelSet runs the local ports: each listens on 127.0.0.1:PORT and pins
// every connection to one host:port through one tailnet (desktop tunnels.rs).
type tunnelSet struct {
	dial DialFunc
	logf func(string, ...any)

	mu      sync.Mutex
	running map[string]*tunnelRun
}

type tunnelRun struct {
	t      Tunnel
	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

func newTunnelSet(dial DialFunc, logf func(string, ...any)) *tunnelSet {
	return &tunnelSet{dial: dial, logf: logf, running: map[string]*tunnelRun{}}
}

// start binds and serves one local port.
func (ts *tunnelSet) start(t Tunnel) error {
	listen, err := normalizeTunnelListen(t.Listen)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("local port %s: %w", listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &tunnelRun{t: t, ln: ln, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	ts.mu.Lock()
	old := ts.running[t.Name]
	ts.running[t.Name] = run
	ts.mu.Unlock()
	if old != nil {
		old.close()
	}
	run.wg.Add(1)
	go ts.serve(run)
	return nil
}

func (ts *tunnelSet) serve(run *tunnelRun) {
	defer run.wg.Done()
	for {
		c, err := run.ln.Accept()
		if err != nil {
			if run.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		run.mu.Lock()
		if run.ctx.Err() != nil {
			run.mu.Unlock()
			c.Close()
			return
		}
		run.conns[c] = struct{}{}
		run.wg.Add(1)
		run.mu.Unlock()
		go func() {
			defer run.wg.Done()
			defer func() {
				run.mu.Lock()
				delete(run.conns, c)
				run.mu.Unlock()
				c.Close()
			}()
			ctx, cancel := context.WithTimeout(run.ctx, dialTimeout)
			up, err := ts.dial(ctx, run.t.Profile, "tcp", run.t.Target)
			cancel()
			if err != nil {
				ts.logf("local port %s: %v", run.t.Name, err)
				return
			}
			defer up.Close()
			pipe(c, c, up, up)
		}()
	}
}

func (run *tunnelRun) close() {
	run.cancel()
	run.ln.Close()
	run.mu.Lock()
	for c := range run.conns {
		c.Close()
	}
	run.mu.Unlock()
	run.wg.Wait()
}

// has reports whether ap is the address of a running local port.
func (ts *tunnelSet) has(ap netip.AddrPort) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, run := range ts.running {
		if a, err := netip.ParseAddrPort(run.ln.Addr().String()); err == nil && a == ap {
			return true
		}
	}
	return false
}

// stop closes one local port, if running.
func (ts *tunnelSet) stop(name string) {
	ts.mu.Lock()
	run := ts.running[name]
	delete(ts.running, name)
	ts.mu.Unlock()
	if run != nil {
		run.close()
	}
}

// sync makes the running set match tunnels: stops what is gone or changed,
// starts what is missing. Returns the ports that could not start.
func (ts *tunnelSet) sync(tunnels []Tunnel) map[string]error {
	want := map[string]Tunnel{}
	for _, t := range tunnels {
		want[t.Name] = t
	}
	ts.mu.Lock()
	var gone []string
	for name, run := range ts.running {
		if t, ok := want[name]; !ok || t != run.t {
			gone = append(gone, name)
		}
	}
	ts.mu.Unlock()
	for _, name := range gone {
		ts.stop(name)
	}
	errs := map[string]error{}
	for _, t := range tunnels {
		ts.mu.Lock()
		_, ok := ts.running[t.Name]
		ts.mu.Unlock()
		if ok {
			continue
		}
		if err := ts.start(t); err != nil {
			ts.logf("%v", err)
			errs[t.Name] = err
		}
	}
	return errs
}

func (ts *tunnelSet) closeAll() {
	ts.mu.Lock()
	names := make([]string, 0, len(ts.running))
	for n := range ts.running {
		names = append(names, n)
	}
	ts.mu.Unlock()
	for _, n := range names {
		ts.stop(n)
	}
}
