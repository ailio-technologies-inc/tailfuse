package engine

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ptRouter is a router with two live tailnets ("work", "personal"), one
// owned but down ("gone"), a conflict between work and personal, and a
// domain "gone" served when it last ran. Names in res resolve as given.
type ptEnv struct {
	r   *Router
	fn  *fakeNet
	res *fakeResolver
}

func ptRouter(t *testing.T, passthrough bool) *ptEnv {
	t.Helper()
	fn := newFakeNet(t)
	fn.serve("work")
	fn.serve("personal")
	cfg := NewConfig()
	cfg.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net"}}
	cfg.Profiles["personal"] = &Profile{Suffixes: []string{".home-lab.ts.net"}}
	cfg.Profiles["gone"] = &Profile{Suffixes: []string{".gone.ts.net"}, Paused: true}
	res := &fakeResolver{}
	rt := NewRouteTable("", res)
	rt.SetConfig(cfg)
	ip := netip.MustParseAddr
	rt.SetLearned("work", []netip.Addr{ip("100.74.194.18"), ip("100.70.0.9")}, []string{"corp.example", "shared.example"})
	rt.SetLearned("personal", []netip.Addr{ip("100.70.0.9")}, []string{"shared.example"})
	rt.SetLearned("gone", nil, []string{"old.example"})
	rt.Forget("gone") // it stopped: its domain is only a claim now
	r := NewRouter(rt, fn.dial, t.Logf)
	r.SetPassthrough(passthrough)
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &ptEnv{r: r, fn: fn, res: res}
}

// remote makes 127.0.0.1 count as "somewhere else", so loopback test servers
// can play internet sites. Only unspecified addresses stay "this device".
func (p *ptEnv) remote() { p.r.localAddr = func(a netip.Addr) bool { return a.IsUnspecified() } }

func rawExchange(t *testing.T, addr, send string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, send)
	b, _ := io.ReadAll(c)
	return string(b)
}

func connectStatus(t *testing.T, addr, target string) (int, string, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	body := ""
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		c.Close()
		return resp.StatusCode, body, nil, nil
	}
	t.Cleanup(func() { c.Close() })
	return 200, "", c, br
}

// Passthrough off must answer exactly as before, byte for byte.
func TestPassthroughOffIsUnchanged(t *testing.T) {
	p := ptRouter(t, false)
	p.res.set("evil.example.com", "93.184.216.34")
	want403 := func(host string) string {
		msg := "no tailfuse profile owns hostname " + host
		return "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: " +
			strconv.Itoa(len(msg)) + "\r\nConnection: close\r\n\r\n" + msg
	}
	for _, host := range []string{"evil.example.com", "x.old.example", "100.99.0.1", "localhost"} {
		if got := rawExchange(t, p.r.Addr(), "CONNECT "+net.JoinHostPort(host, "443")+" HTTP/1.1\r\n\r\n"); got != want403(host) {
			t.Errorf("CONNECT %s:\n got %q\nwant %q", host, got, want403(host))
		}
		if got := rawExchange(t, p.r.Addr(), "GET http://"+net.JoinHostPort(host, "80")+"/ HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); got != want403(host) {
			t.Errorf("GET %s:\n got %q\nwant %q", host, got, want403(host))
		}
	}
	c, code := socks5(t, p.r.Addr(), 0x03, "evil.example.com", 443)
	rest, _ := io.ReadAll(c)
	c.Close()
	if code != socksNotAllowed || len(rest) != 0 {
		t.Fatalf("socks: code %d, extra %q", code, rest)
	}
	// Down tailnet: the same 502 text as before.
	got := rawExchange(t, p.r.Addr(), "CONNECT a.gone.ts.net:443 HTTP/1.1\r\n\r\n")
	msg := "tailnet gone could not reach a.gone.ts.net:443: tailnet gone is not connected"
	if want := "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: " + strconv.Itoa(len(msg)) + "\r\nConnection: close\r\n\r\n" + msg; got != want {
		t.Fatalf("down tailnet:\n got %q\nwant %q", got, want)
	}
}

func TestPassthroughDirectForUnownedNames(t *testing.T) {
	p := ptRouter(t, true)
	p.remote()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "direct %s %s", r.Host, r.URL.Path)
	}))
	defer site.Close()
	_, port, _ := net.SplitHostPort(site.Listener.Addr().String())
	p.res.set("site.example", "127.0.0.1")
	before := len(p.fn.dials)

	// CONNECT
	code, body, c, br := connectStatus(t, p.r.Addr(), "site.example:"+port)
	if code != 200 {
		t.Fatalf("CONNECT: %d %s", code, body)
	}
	fmt.Fprintf(c, "GET /c HTTP/1.1\r\nHost: site.example\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "direct site.example /c" {
		t.Fatalf("CONNECT landed on %q", b)
	}

	// SOCKS5, by name and by address
	for _, tc := range []struct {
		atyp byte
		host string
	}{{0x03, "site.example"}, {0x01, "127.0.0.1"}} {
		n, _ := strconv.Atoi(port)
		sc, code := socks5(t, p.r.Addr(), tc.atyp, tc.host, uint16(n))
		if code != 0 {
			t.Fatalf("socks %s: %d", tc.host, code)
		}
		if got := getThrough(t, sc, "site.example"); got != "direct site.example /hello" {
			t.Fatalf("socks %s landed on %q", tc.host, got)
		}
		sc.Close()
	}

	// Plain HTTP through Go's proxy client.
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: p.r.Addr()})}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	r2, err := cl.Get("http://site.example:" + port + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r2.Body)
	r2.Body.Close()
	if string(b) != "direct site.example:"+port+" /plain" {
		t.Fatalf("plain landed on %q", b)
	}
	if len(p.fn.dials) != before {
		t.Fatalf("an unowned name went through a node: %v", p.fn.dials[before:])
	}

	// Owned names still go to their tailnet, never direct.
	r3, err := cl.Get("http://nas.home-lab.ts.net/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r3.Body)
	r3.Body.Close()
	if string(b) != "personal nas.home-lab.ts.net /x" {
		t.Fatalf("owned name landed on %q", b)
	}
	// A custom domain resolving to exactly one tailnet's device goes there.
	p.res.set("custom.example", "100.74.194.18")
	r4, err := cl.Get("http://custom.example/y")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r4.Body)
	r4.Body.Close()
	if !strings.HasPrefix(string(b), "work ") {
		t.Fatalf("custom domain landed on %q", b)
	}
}

func TestPassthroughStillRefuses(t *testing.T) {
	p := ptRouter(t, true)
	p.res.set("split.example", "100.74.194.18", "100.70.0.9") // work + conflict
	p.res.set("cgnat.example", "100.99.0.1")                  // tailnet space, nobody's
	p.res.set("mixed.example", "93.184.216.34", "100.99.0.1")
	p.res.set("localhost", "127.0.0.1", "::1")
	p.res.set("multi.example", "224.0.0.1")
	_, routerPort, _ := net.SplitHostPort(p.r.Addr())
	cases := []struct {
		target string
		code   int
		want   string
	}{
		{"x.shared.example:443", 403, "claimed by more than one tailnet"},
		{"100.70.0.9:443", 403, "claimed by more than one tailnet"},
		{"100.99.0.1:443", 403, "tailscale address no running tailnet has"},
		{"[fd7a:115c:a1e0::99]:443", 403, "tailscale address no running tailnet has"},
		{"cgnat.example:443", 403, "resolves to a tailscale address"},
		{"split.example:443", 403, "resolves to a tailscale address"},
		{"mixed.example:443", 403, "resolves to a tailscale address"},
		{"x.old.example:443", 403, "belongs to tailnet gone, which is not connected"},
		{"a.gone.ts.net:443", 502, "tailnet gone could not reach"},
		{"127.0.0.1:" + routerPort, 403, "is this device"},
		{"localhost:" + routerPort, 403, "is this device"},
		{"localhost:22", 403, "is this device"},
		{"[::1]:80", 403, "is this device"},
		{"0.0.0.0:80", 403, "is this device"},
		{"[::]:80", 403, "is this device"},
		{"multi.example:80", 403, "no address that can be connected to"},
		{"site.example:0", 400, "bad port"},
	}
	for _, tc := range cases {
		code, body, _, _ := connectStatus(t, p.r.Addr(), tc.target)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("CONNECT %s: %d %q, want %d %q", tc.target, code, body, tc.code, tc.want)
		}
	}
	// The off tailnet's names never reached public DNS.
	for _, h := range []string{"a.gone.ts.net", "x.old.example", "x.shared.example"} {
		if p.res.asked(h) {
			t.Errorf("%s was looked up in public DNS", h)
		}
	}
	// SOCKS5 and plain HTTP refuse the same way.
	sc, code := socks5(t, p.r.Addr(), 0x03, "x.old.example", 443)
	sc.Close()
	if code != socksNotAllowed {
		t.Fatalf("socks claimed domain: %d", code)
	}
	sc, code = socks5(t, p.r.Addr(), 0x01, "127.0.0.1", 22)
	sc.Close()
	if code != socksNotAllowed {
		t.Fatalf("socks loopback: %d", code)
	}
	if got := rawExchange(t, p.r.Addr(), "GET http://cgnat.example/ HTTP/1.1\r\nHost: cgnat.example\r\n\r\n"); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Fatalf("plain cgnat: %q", got)
	}
}

func TestPassthroughLANAndLocalPorts(t *testing.T) {
	p := ptRouter(t, true)
	// Private LAN and link-local addresses are allowed (not dialed here).
	for _, h := range []string{"192.168.1.10", "10.0.0.1", "172.16.5.5", "169.254.10.1", "fd00::1", "fe80::1"} {
		if _, oe := p.r.directTargets(h, "80"); oe != nil {
			t.Errorf("%s refused: %s", h, oe.msg)
		}
	}
	// A registered local port on loopback is reachable; others are not.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "local-port")
			c.Close()
		}
	}()
	lp := netip.MustParseAddrPort(ln.Addr().String())
	p.r.SetLocalPorts(func(ap netip.AddrPort) bool { return ap == lp })
	code, body, c, br := connectStatus(t, p.r.Addr(), ln.Addr().String())
	if code != 200 {
		t.Fatalf("local port: %d %s", code, body)
	}
	b, _ := io.ReadAll(br)
	c.Close()
	if string(b) != "local-port" {
		t.Fatalf("local port said %q", b)
	}
	// Even if the router's own address were listed, it is refused.
	self := netip.MustParseAddrPort(p.r.Addr())
	p.r.SetLocalPorts(func(ap netip.AddrPort) bool { return ap == lp || ap == self })
	if code, _, _, _ := connectStatus(t, p.r.Addr(), p.r.Addr()); code != 403 {
		t.Fatalf("router itself: %d", code)
	}
}

// startRaw serves each connection with h on loopback; returns the port.
func startRaw(t *testing.T, h func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				h(c)
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func directEnv(t *testing.T) (*ptEnv, *http.Client) {
	p := ptRouter(t, true)
	p.remote()
	p.res.set("site.example", "127.0.0.1")
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: p.r.Addr()}), ExpectContinueTimeout: 10 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	return p, &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func TestForwardHopHeadersAndKeepAlive(t *testing.T) {
	p, _ := directEnv(t)
	var seen http.Header
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("X-Kept", "yes")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Proxy-Authenticate", "Basic")
		io.WriteString(w, "ok")
	}))
	defer site.Close()
	_, port := splitPort(site.Listener.Addr().String())
	c, err := net.Dial("tcp", p.r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	for i := 0; i < 2; i++ { // two requests on one client connection
		fmt.Fprintf(c, "GET http://site.example:%s/h HTTP/1.1\r\nHost: site.example\r\nConnection: X-Secret, keep-alive\r\nX-Secret: 1\r\nX-Kept: 1\r\nProxy-Authorization: Basic eA==\r\nProxy-Connection: keep-alive\r\nKeep-Alive: 300\r\nTe: trailers\r\n\r\n", port)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "ok" || resp.Header.Get("X-Kept") != "yes" || resp.Header.Get("Keep-Alive") != "" || resp.Header.Get("Proxy-Authenticate") != "" || resp.Close {
			t.Fatalf("response %d: %q %v close=%v", i, b, resp.Header, resp.Close)
		}
		for _, h := range []string{"X-Secret", "Proxy-Authorization", "Proxy-Connection", "Keep-Alive", "Te"} {
			if seen.Get(h) != "" {
				t.Errorf("upstream got hop header %s", h)
			}
		}
		if seen.Get("X-Kept") != "1" {
			t.Fatal("end-to-end header lost")
		}
	}
}

func splitPort(addr string) (string, string) {
	h, p, _ := net.SplitHostPort(addr)
	return h, p
}

func TestForwardStreamsWithoutBuffering(t *testing.T) {
	p, cl := directEnv(t)
	_ = p
	release := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "-rest")
	}))
	defer site.Close()
	defer close(release)
	_, port := splitPort(site.Listener.Addr().String())
	resp, err := cl.Get("http://site.example:" + port + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.TransferEncoding == nil || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("not re-chunked: %v", resp.TransferEncoding)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first" {
		t.Fatalf("first bytes did not stream through while upstream waits: %q %v", buf, err)
	}
}

func TestForwardLargeBodiesBothWays(t *testing.T) {
	_, cl := directEnv(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 512*1024) // 8 MiB
	sum := sha256.Sum256(big)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h := sha256.New()
			n, _ := io.Copy(h, r.Body)
			fmt.Fprintf(w, "%d %x %s", n, h.Sum(nil), r.Header.Get("Expect"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.Write(big)
	}))
	defer site.Close()
	_, port := splitPort(site.Listener.Addr().String())
	resp, err := cl.Get("http://site.example:" + port + "/big")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	resp.Body.Close()
	if err != nil || n != int64(len(big)) || resp.ContentLength != int64(len(big)) || !bytes.Equal(h.Sum(nil), sum[:]) {
		t.Fatalf("download: %d bytes, cl %d, err %v", n, resp.ContentLength, err)
	}
	// Chunked upload of unknown length, with Expect: 100-continue.
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < len(big); i += 100_000 {
			end := min(i+100_000, len(big))
			pw.Write(big[i:end])
		}
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, "http://site.example:"+port+"/up", pr)
	req.Header.Set("Expect", "100-continue")
	start := time.Now()
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := fmt.Sprintf("%d %x ", len(big), sum); string(b) != want {
		t.Fatalf("upload: %q want %q", b, want)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("upload waited out the Expect timeout")
	}
	// Fixed-length upload.
	resp, err = cl.Post("http://site.example:"+port+"/up2", "application/octet-stream", bytes.NewReader(big[:12345]))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(b), "12345 ") {
		t.Fatalf("fixed upload: %q", b)
	}
}

func TestForwardHeadNoBodyAndCloseDelimited(t *testing.T) {
	p, _ := directEnv(t)
	port := startRaw(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		switch req.URL.Path {
		case "/head":
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1234\r\n\r\n")
			// one more on the same connection
			if req2, err := http.ReadRequest(br); err == nil && req2.URL.Path == "/204" {
				io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
			}
		case "/old":
			// HTTP/1.0-style: no length, ends at close.
			io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nuntil-close")
		}
	})
	c, err := net.Dial("tcp", p.r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "HEAD http://site.example:%s/head HTTP/1.1\r\nHost: site.example\r\n\r\n", port)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodHead})
	if err != nil || resp.ContentLength != 1234 {
		t.Fatalf("HEAD: %v %v", resp, err)
	}
	fmt.Fprintf(c, "GET http://site.example:%s/204 HTTP/1.1\r\nHost: site.example\r\n\r\n", port)
	resp, err = http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("204: %v %v", resp, err)
	}
	// Close-delimited upstream: re-chunked, and the client connection lives on.
	fmt.Fprintf(c, "GET http://site.example:%s/old HTTP/1.1\r\nHost: site.example\r\n\r\n", port)
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "until-close" || resp.Close {
		t.Fatalf("close-delimited: %q close=%v", b, resp.Close)
	}
	fmt.Fprintf(c, "GET http://site.example:%s/old HTTP/1.1\r\nHost: site.example\r\nConnection: close\r\n\r\n", port)
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal("client connection did not survive an upstream close:", err)
	}
	b, _ = io.ReadAll(resp.Body)
	if string(b) != "until-close" || !resp.Close {
		t.Fatalf("second: %q close=%v", b, resp.Close)
	}
}

func TestForwardUpgrade(t *testing.T) {
	p, _ := directEnv(t)
	var gotConn, gotUp string
	port := startRaw(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		gotConn, gotUp = req.Header.Get("Connection"), req.Header.Get("Upgrade")
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buf := make([]byte, 4)
		for {
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			c.Write(bytes.ToUpper(buf))
		}
	})
	c, err := net.Dial("tcp", p.r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET http://site.example:%s/ws HTTP/1.1\r\nHost: site.example\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: x\r\n\r\n", port)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 || resp.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	if gotConn != "Upgrade" || gotUp != "websocket" {
		t.Fatalf("upstream saw Connection %q Upgrade %q", gotConn, gotUp)
	}
	for _, msg := range []string{"ping", "pong"} {
		io.WriteString(c, msg)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != strings.ToUpper(msg) {
			t.Fatalf("frames: %q %v", buf, err)
		}
	}
}

func TestForwardIdleTimeoutNotTotal(t *testing.T) {
	p, cl := directEnv(t)
	_ = p
	old := bodyIdleTimeout
	bodyIdleTimeout = 300 * time.Millisecond
	defer func() { bodyIdleTimeout = old }()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow": // 1.2 s in all, never idle for long
			for i := 0; i < 12; i++ {
				io.WriteString(w, "x")
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
			}
		case "/stall":
			io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Second)
			io.WriteString(w, "late")
		}
	}))
	defer site.Close()
	_, port := splitPort(site.Listener.Addr().String())
	resp, err := cl.Get("http://site.example:" + port + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != strings.Repeat("x", 12) {
		t.Fatalf("slow transfer cut: %q %v", b, err)
	}
	start := time.Now()
	resp, err = cl.Get("http://site.example:" + port + "/stall")
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || strings.Contains(string(b), "late") || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("stalled transfer not ended: %q %v after %v", b, err, time.Since(start))
	}
}

func TestClaimsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := NewConfig()
	cfg.Profiles["gone"] = &Profile{Suffixes: []string{".gone.ts.net"}}
	hosts := filepath.Join(dir, "learned-hosts.json")
	rt := NewRouteTable(hosts, nil)
	rt.SetConfig(cfg)
	rt.SetLearned("gone", nil, []string{"Old.Example."})
	rt2 := NewRouteTable(hosts, nil)
	rt2.SetConfig(cfg)
	if p, v := rt2.Classify("wiki.old.example"); v != Claimed || p != "gone" {
		t.Fatalf("after restart: %q %v", p, v)
	}
	if rt2.Owner("wiki.old.example") != "" {
		t.Fatal("a claim routes traffic")
	}
	// It stops serving the domain: the claim goes.
	rt2.SetLearned("gone", nil, nil)
	if _, v := rt2.Classify("wiki.old.example"); v != Unowned {
		t.Fatalf("stale claim kept: %v", v)
	}
	// A removed profile's claims go too.
	rt2.SetLearned("gone", nil, []string{"old.example"})
	delete(cfg.Profiles, "gone")
	rt2.SetConfig(cfg)
	if _, v := NewRouteTable(hosts, nil).Classify("old.example"); v != Unowned {
		t.Fatalf("removed profile's claim kept: %v", v)
	}
}

func TestEngineSetPassthrough(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet(t)
	e, _ := newTestEngine(t, fn, dir)
	e.SetPassthrough(true) // before Start
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	if !getState(t, e).Passthrough || !e.router.passthrough.Load() {
		t.Fatal("passthrough not applied at Start")
	}
	// A local port is the one loopback target passthrough may reach.
	c := readConfig(t, dir)
	c.Profiles["w"] = &Profile{Suffixes: []string{".w.ts.net"}}
	writeConfig(t, dir, c)
	e.Stop()
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	mustOK(t, e, "add_tunnel", map[string]any{"name": "db", "listen": port, "target": "db.w.ts.net:5432", "profile": "w"})
	if !e.tunnels.has(netip.MustParseAddrPort("127.0.0.1:" + port)) {
		t.Fatal("local port not known")
	}
	if code, _, _, _ := connectStatus(t, e.router.Addr(), "127.0.0.1:"+port); code != 200 {
		t.Fatalf("local port via passthrough: %d", code)
	}
	if code, _, _, _ := connectStatus(t, e.router.Addr(), "127.0.0.1:1"); code != 403 {
		t.Fatalf("other loopback port: %d", code)
	}
	e.SetPassthrough(false)
	if getState(t, e).Passthrough {
		t.Fatal("still on")
	}
	if code, _, _, _ := connectStatus(t, e.router.Addr(), "127.0.0.1:"+port); code != 403 {
		t.Fatalf("passthrough off: %d", code)
	}
}
