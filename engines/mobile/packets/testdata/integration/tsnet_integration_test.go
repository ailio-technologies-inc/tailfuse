//go:build tsintegration

// Real nodes, real WireGuard: two tailnets, each with its own control
// server and DERP (Tailscale's testcontrol), a peer device on each, and the
// phone's two nodes running with tsnet.Server.Tun set to the Mux's links.
//
//	go test -tags tsintegration -run Tsnet -v ./packets/
package packets

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/local"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

type realDNS map[string]*local.Client

func (d realDNS) QueryDNS(ctx context.Context, tailnet, name, qtype string) ([]byte, error) {
	lc := d[tailnet]
	if lc == nil {
		return nil, fmt.Errorf("tailnet %s is not running", tailnet)
	}
	raw, _, err := lc.QueryDNS(ctx, strings.TrimSuffix(name, ".")+".", qtype)
	return raw, err
}

func startTailnet(t *testing.T, domain string) string {
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap:        derpMap,
		DNSConfig:      &tailcfg.DNSConfig{Proxied: true},
		MagicDNSDomain: domain,
		Logf:           logger.Discard,
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control.HTTPTestServer.URL
}

func startNode(t *testing.T, ctx context.Context, controlURL, host string, s *tsnet.Server) (*tsnet.Server, netip.Addr) {
	dir := filepath.Join(t.TempDir(), host)
	os.MkdirAll(dir, 0o755)
	if s == nil {
		s = &tsnet.Server{}
	}
	s.Dir, s.ControlURL, s.Hostname, s.Store, s.Ephemeral = dir, controlURL, host, new(mem.Store), true
	s.Logf = logger.Discard
	t.Cleanup(func() { s.Close() })
	st, err := s.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, st.TailscaleIPs[0]
}

func TestTsnetIntegration(t *testing.T) {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	r := newRig(t, rigOpts{})
	dns := realDNS{}
	r.m.dns.tdns = dns

	type net1 struct {
		name, domain string
		peerIP       netip.Addr
	}
	nets := []*net1{{name: "work", domain: "work.ts.example"}, {name: "home", domain: "home.ts.example"}}
	for _, n := range nets {
		url := startTailnet(t, n.domain)
		peer, ip := startNode(t, ctx, url, "server", nil)
		n.peerIP = ip
		ln, err := peer.Listen("tcp", ":22")
		if err != nil {
			t.Fatal(err)
		}
		greeting := n.name + " "
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); c.Write([]byte(greeting)); io.Copy(c, c) }()
			}
		}()
		phone, _ := startNode(t, ctx, url, "phone", &tsnet.Server{Tun: r.m.Link(n.name)})
		v4, v6 := phone.TailscaleIPs()
		r.m.SetAddrs(n.name, []netip.Addr{v4, v6})
		lc, _ := phone.LocalClient()
		dns[n.name] = lc
		r.rules.name(n.domain, n.name, Owned)
		t.Logf("%s: peer %v, phone %v %v", n.name, ip, v4, v6)

		// The proxy path keeps working on a node with a Tun.
		c, err := phone.Dial(ctx, "tcp", net.JoinHostPort(ip.String(), "22"))
		if err != nil {
			t.Fatalf("%s: tsnet Dial with Tun set: %v", n.name, err)
		}
		buf := make([]byte, len(greeting))
		c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != greeting {
			t.Fatalf("%s: proxy path %q %v", n.name, buf, err)
		}
		c.Close()
	}
	shared := nets[0].peerIP == nets[1].peerIP
	if shared {
		r.rules.addr(nets[0].peerIP.String(), "work", "home")
	} else {
		r.rules.addr(nets[0].peerIP.String(), "work")
		r.rules.addr(nets[1].peerIP.String(), "home")
	}
	t.Logf("peers share an address: %v", shared)

	for _, n := range nets {
		m := r.query("server."+n.domain, dnsmessage.TypeA)
		as := answerAddrs(m)
		if m.RCode != dnsmessage.RCodeSuccess || len(as) != 1 {
			t.Fatalf("%s: dns %v %v", n.name, m.RCode, as)
		}
		if shared != r.m.pool.Contains(as[0]) {
			t.Fatalf("%s: answered %v (shared=%v)", n.name, as[0], shared)
		}
		t.Logf("server.%s -> %v", n.domain, as[0])
		var c net.Conn
		var err error
		for i := 0; i < 20; i++ { // the first handshake goes through DERP
			dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
			c, err = r.phone.dialTCP(dctx, netip.AddrPortFrom(as[0], 22))
			dcancel()
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: dial through the tun: %v", n.name, err)
		}
		c.SetDeadline(time.Now().Add(20 * time.Second))
		buf := make([]byte, len(n.name)+1)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != n.name+" " {
			t.Fatalf("%s: got %q %v", n.name, buf, err)
		}
		payload := strings.Repeat("x", 256<<10)
		go io.WriteString(c, payload)
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != payload {
			t.Fatalf("%s: echo %v", n.name, err)
		}
		c.Close()
	}
	st := r.m.Stats()
	t.Logf("packets in %d out %d refused %d dropped %d unsolicited %d", st.PacketsIn.Load(), st.PacketsOut.Load(),
		st.Refused.Load(), st.Dropped.Load(), st.Unsolicited.Load())
}
