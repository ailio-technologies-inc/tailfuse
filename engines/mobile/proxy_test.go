package engine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testRouter starts a router over real loopback sockets with two fake
// tailnets ("work" and "personal") and a learned address for "work".
func testRouter(t *testing.T) (*Router, *fakeNet) {
	t.Helper()
	fn := newFakeNet(t)
	fn.serve("work")
	fn.serve("personal")
	cfg := NewConfig()
	cfg.Profiles["work"] = &Profile{Suffixes: []string{".work.ts.net"}}
	cfg.Profiles["personal"] = &Profile{Suffixes: []string{".home-lab.ts.net"}}
	cfg.Profiles["gone"] = &Profile{Suffixes: []string{".gone.ts.net"}} // owned, but its node is down
	rt := NewRouteTable("", &fakeResolver{})
	rt.SetConfig(cfg)
	rt.SetLearned("work", []netip.Addr{netip.MustParseAddr("100.74.194.18"), netip.MustParseAddr("fd7a:115c:a1e0::5")}, []string{"corp.example"})
	r := NewRouter(rt, fn.dial, t.Logf)
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, fn
}

// socks5 connects through the router with SOCKS5 and returns the reply code.
func socks5(t *testing.T, addr string, atyp byte, host string, port uint16) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{0x05, 0x01, 0x00})
	var greet [2]byte
	if _, err := io.ReadFull(c, greet[:]); err != nil || greet != [2]byte{0x05, 0x00} {
		t.Fatalf("greeting: %v %v", greet, err)
	}
	req := []byte{0x05, 0x01, 0x00, atyp}
	switch atyp {
	case 0x01:
		a := netip.MustParseAddr(host).As4()
		req = append(req, a[:]...)
	case 0x04:
		a := netip.MustParseAddr(host).As16()
		req = append(req, a[:]...)
	case 0x03:
		req = append(req, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	c.Write(req)
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return c, rep[1]
}

// getThrough sends a GET over an established tunnel and returns the body.
func getThrough(t *testing.T, c net.Conn, host string) string {
	t.Helper()
	fmt.Fprintf(c, "GET /hello HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestSOCKS5RoutesBySuffix(t *testing.T) {
	r, fn := testRouter(t)
	c, code := socks5(t, r.Addr(), 0x03, "db.work.ts.net", 5432)
	if code != 0 {
		t.Fatalf("reply %d", code)
	}
	if got := getThrough(t, c, "db.work.ts.net"); got != "work db.work.ts.net /hello" {
		t.Fatalf("landed on %q", got)
	}
	if got := fn.lastDial(); got != "work db.work.ts.net:5432" {
		t.Fatalf("dialed %q", got)
	}
	c.Close()
	c, code = socks5(t, r.Addr(), 0x03, "NAS.home-lab.ts.net", 80)
	if code != 0 {
		t.Fatalf("reply %d", code)
	}
	if got := getThrough(t, c, "nas"); !strings.HasPrefix(got, "personal ") {
		t.Fatalf("landed on %q", got)
	}
	c.Close()
}

func TestSOCKS5IPAddressTypes(t *testing.T) {
	r, fn := testRouter(t)
	c, code := socks5(t, r.Addr(), 0x01, "100.74.194.18", 80)
	if code != 0 {
		t.Fatalf("ipv4 reply %d", code)
	}
	if got := getThrough(t, c, "x"); !strings.HasPrefix(got, "work ") {
		t.Fatalf("ipv4 landed on %q", got)
	}
	if got := fn.lastDial(); got != "work 100.74.194.18:80" {
		t.Fatalf("dialed %q", got)
	}
	c.Close()
	c, code = socks5(t, r.Addr(), 0x04, "fd7a:115c:a1e0::5", 443)
	if code != 0 {
		t.Fatalf("ipv6 reply %d", code)
	}
	if got := fn.lastDial(); got != "work [fd7a:115c:a1e0::5]:443" {
		t.Fatalf("dialed %q", got)
	}
	c.Close()
	// An address no tailnet has is refused.
	c, code = socks5(t, r.Addr(), 0x01, "1.1.1.1", 443)
	c.Close()
	if code != socksNotAllowed {
		t.Fatalf("unowned ip reply %d", code)
	}
}

func TestSOCKS5Refusals(t *testing.T) {
	r, fn := testRouter(t)
	c, code := socks5(t, r.Addr(), 0x03, "evil.example.com", 443)
	c.Close()
	if code != socksNotAllowed {
		t.Fatalf("unowned reply %d", code)
	}
	if fn.lastDial() != "" {
		t.Fatalf("an unowned name was dialed: %s", fn.lastDial())
	}
	// Owned but its tailnet is down: host unreachable, nothing elsewhere.
	c, code = socks5(t, r.Addr(), 0x03, "a.gone.ts.net", 443)
	c.Close()
	if code != socksHostUnreachable {
		t.Fatalf("down tailnet reply %d", code)
	}
	// BIND is not supported.
	c, err := net.Dial("tcp", r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(c, make([]byte, 2))
	c.Write([]byte{0x05, 0x02, 0x00, 0x03, 4, 'a', '.', 'b', 'c', 0, 80})
	rep := make([]byte, 10)
	io.ReadFull(c, rep)
	if rep[1] != socksCmdUnsupported {
		t.Fatalf("bind reply %d", rep[1])
	}
	// A client offering only username/password auth is turned away.
	c2, _ := net.Dial("tcp", r.Addr())
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	c2.Write([]byte{0x05, 0x01, 0x02})
	g := make([]byte, 2)
	io.ReadFull(c2, g)
	if g[1] != 0xff {
		t.Fatalf("auth reply %v", g)
	}
}

func TestHTTPConnect(t *testing.T) {
	r, fn := testRouter(t)
	c, err := net.Dial("tcp", r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// The tunnelled request is sent right behind CONNECT, before the reply,
	// as some clients do: it must not be lost.
	fmt.Fprintf(c, "CONNECT wiki.work.ts.net:8443 HTTP/1.1\r\nHost: wiki.work.ts.net:8443\r\n\r\n"+
		"GET /early HTTP/1.1\r\nHost: wiki.work.ts.net\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	inner, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(inner.Body)
	if string(b) != "work wiki.work.ts.net /early" {
		t.Fatalf("landed on %q", b)
	}
	if got := fn.lastDial(); got != "work wiki.work.ts.net:8443" {
		t.Fatalf("dialed %q", got)
	}

	// Refused: 403 with a short plain-text reason.
	c2, _ := net.Dial("tcp", r.Addr())
	defer c2.Close()
	fmt.Fprintf(c2, "CONNECT evil.example.com:443 HTTP/1.1\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(c2), &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("refusal: %v %v", resp, err)
	}
	b, _ = io.ReadAll(resp.Body)
	if string(b) != "no tailfuse profile owns hostname evil.example.com" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("refusal body %q", b)
	}

	// Learned split-DNS domain, through Go's own proxy client.
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: r.Addr()})}
	defer tr.CloseIdleConnections()
	_, err = (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://wiki.corp.example/")
	// The fake tailnet speaks plain HTTP, so TLS fails, but only after the
	// CONNECT went to the right tailnet.
	if err == nil || fn.lastDial() != "work wiki.corp.example:443" {
		t.Fatalf("learned domain: err=%v dialed %q", err, fn.lastDial())
	}
}

func TestPlainHTTPProxy(t *testing.T) {
	r, fn := testRouter(t)
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: r.Addr()})}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for _, tc := range []struct{ url, want string }{
		{"http://nas.home-lab.ts.net/a", "personal nas.home-lab.ts.net /a"},
		{"http://nas.home-lab.ts.net/b", "personal nas.home-lab.ts.net /b"}, // same connection, kept alive
		{"http://grafana.work.ts.net:3000/c", "work grafana.work.ts.net:3000 /c"},
	} {
		resp, err := client.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != tc.want {
			t.Fatalf("%s: %d %q", tc.url, resp.StatusCode, b)
		}
	}
	if got := fn.lastDial(); got != "work grafana.work.ts.net:3000" {
		t.Fatalf("dialed %q", got)
	}
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || string(b) != "no tailfuse profile owns hostname example.com" {
		t.Fatalf("refusal: %d %q", resp.StatusCode, b)
	}
	// Owned but down: 502, never sent elsewhere.
	resp, err = client.Get("http://a.gone.ts.net/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("down tailnet: %d", resp.StatusCode)
	}
	// POST bodies pass through.
	resp, err = client.Post("http://api.work.ts.net/post", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d", resp.StatusCode)
	}
}

func TestPACAndLocalRequests(t *testing.T) {
	r, _ := testRouter(t)
	get := func(path, host string) (*http.Response, string) {
		c, err := net.Dial("tcp", r.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, host)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/tailfuse.pac", r.Addr())
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ns-proxy-autoconfig" {
		t.Fatalf("pac: %d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{`".work.ts.net"`, `".home-lab.ts.net"`, `".corp.example"`, `"100.74.194.18"`, "PROXY " + r.Addr()} {
		if !strings.Contains(body, want) {
			t.Errorf("PAC lacks %s:\n%s", want, body)
		}
	}
	if resp, _ := get("/tailfuse.pac", "localhost"); resp.StatusCode != 200 {
		t.Fatalf("pac via localhost: %d", resp.StatusCode)
	}
	// A page that rebinds its own name to 127.0.0.1 gets no PAC.
	if resp, _ := get("/tailfuse.pac", "attacker.example"); resp.StatusCode != 403 {
		t.Fatalf("rebinding: %d", resp.StatusCode)
	}
	if resp, body := get("/healthz", r.Addr()); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get("/other", r.Addr()); resp.StatusCode != 404 {
		t.Fatalf("other: %d", resp.StatusCode)
	}
	// The PAC requested in absolute form through the proxy itself.
	if resp, body := get("http://"+r.Addr()+"/tailfuse.pac", r.Addr()); resp.StatusCode != 200 || !strings.Contains(body, "FindProxyForURL") {
		t.Fatalf("absolute pac: %d", resp.StatusCode)
	}
}

func TestRouterRefusesNonLoopbackAndCloses(t *testing.T) {
	r := NewRouter(NewRouteTable("", nil), nil, nil)
	if err := r.Listen("0.0.0.0:0"); err == nil {
		r.Close()
		t.Fatal("listened on all interfaces")
	}
	r2, _ := testRouter(t)
	c, code := socks5(t, r2.Addr(), 0x03, "db.work.ts.net", 1)
	if code != 0 {
		t.Fatal(code)
	}
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, c)
		close(done)
	}()
	r2.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("open connection survived Close")
	}
	if _, err := net.DialTimeout("tcp", r2.Addr(), time.Second); err == nil {
		t.Fatal("still listening")
	}
}
