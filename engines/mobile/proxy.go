package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DialFunc dials address on the named tailnet.
type DialFunc func(ctx context.Context, profile, network, address string) (net.Conn, error)

// Router is the loopback listener. One port speaks SOCKS5 (CONNECT), HTTP
// CONNECT, plain HTTP proxying of absolute-form requests, and serves
// GET /tailfuse.pac. Each connection goes to the tailnet that owns its
// destination; a name nothing owns is refused, never sent elsewhere, unless
// passthrough is on (passthrough.go).
type Router struct {
	routes *RouteTable
	dial   DialFunc
	logf   func(string, ...any)

	passthrough atomic.Bool
	localPort   func(netip.AddrPort) bool // Tailfuse's own local ports
	localAddr   func(netip.Addr) bool     // test hook: what counts as this device

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener
	addr   string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

const (
	handshakeTimeout = 30 * time.Second
	dialTimeout      = 20 * time.Second
	idleKeepAlive    = 2 * time.Minute
	copyBufSize      = 16 << 10
	headerBufSize    = 4 << 10
)

// bufPool holds the copy buffers: two per piped connection, shared, so an
// idle connection costs no copy buffer at all.
var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBufSize); return &b }}

// NewRouter returns a router; Listen starts it.
func NewRouter(routes *RouteTable, dial DialFunc, logf func(string, ...any)) *Router {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Router{routes: routes, dial: dial, logf: logf, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
}

// Listen binds addr (loopback only) and serves in the background.
func (r *Router) Listen(addr string) error {
	if err := checkLoopbackListen(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	r.ln = ln
	r.addr = ln.Addr().String()
	r.wg.Add(1)
	go r.serve()
	return nil
}

// Addr is the bound address.
func (r *Router) Addr() string { return r.addr }

// Close stops listening and ends every connection.
func (r *Router) Close() error {
	r.cancel()
	var err error
	if r.ln != nil {
		err = r.ln.Close()
	}
	r.mu.Lock()
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return err
}

func (r *Router) serve() {
	defer r.wg.Done()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		r.mu.Lock()
		if r.ctx.Err() != nil {
			r.mu.Unlock()
			c.Close()
			return
		}
		r.conns[c] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.conns, c)
				r.mu.Unlock()
				c.Close()
			}()
			r.handle(c)
		}()
	}
}

func (r *Router) handle(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReaderSize(c, headerBufSize)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x05 {
		r.handleSOCKS5(c, br)
		return
	}
	r.handleHTTP(c, br)
}

// SOCKS5 reply codes.
const (
	socksOK              = 0x00
	socksFailure         = 0x01
	socksNotAllowed      = 0x02
	socksHostUnreachable = 0x04
	socksCmdUnsupported  = 0x07
	socksAddrUnsupported = 0x08
)

func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

func (r *Router) handleSOCKS5(c net.Conn, br *bufio.Reader) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		if m == 0x00 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != 0x05 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		var b [4]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return
		}
		host = netip.AddrFrom4(b).String()
	case 0x03:
		n, err := br.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	case 0x04:
		var b [16]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return
		}
		host = netip.AddrFrom16(b).String()
	default:
		_ = socksReply(c, socksAddrUnsupported)
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(pb[:])
	if req[1] != 0x01 {
		_ = socksReply(c, socksCmdUnsupported)
		return
	}
	up, oe := r.open(host, strconv.Itoa(int(port)))
	if oe != nil {
		_ = socksReply(c, oe.socks)
		return
	}
	defer up.Close()
	if err := socksReply(c, socksOK); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	pipe(c, br, up, up)
}

func (r *Router) dialUp(owner, host, port string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
	defer cancel()
	return r.dial(ctx, owner, "tcp", net.JoinHostPort(host, port))
}

// writeHTTPError writes a short plain-text error and asks to close.
func writeHTTPError(c net.Conn, code int, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
}

func refusal(host string) string { return "no tailfuse profile owns hostname " + host }

func (r *Router) handleHTTP(c net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		writeHTTPError(c, http.StatusBadRequest, "tailfuse router: bad request")
		return
	}
	switch {
	case req.Method == http.MethodConnect:
		r.handleConnect(c, br, req)
	case req.URL.IsAbs():
		if r.isSelf(req.URL.Host) {
			r.serveLocal(c, req)
			return
		}
		r.proxyHTTP(c, br, req)
	default:
		if !hostIsLoopback(req.Host) {
			// A page that rebinds its own name to 127.0.0.1 gets nothing here.
			writeHTTPError(c, http.StatusForbidden, "tailfuse router: wrong host")
			return
		}
		r.serveLocal(c, req)
	}
}

// isSelf reports whether hostport is this router's own address.
func (r *Router) isSelf(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	_, myPort, _ := net.SplitHostPort(r.addr)
	return port == myPort && isLoopbackHost(host)
}

func hostIsLoopback(hostport string) bool {
	if hostport == "" {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return isLoopbackHost(host)
}

// serveLocal answers requests for the router itself: the PAC and a health check.
func (r *Router) serveLocal(c net.Conn, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeHTTPError(c, http.StatusMethodNotAllowed, "tailfuse router: method not allowed")
		return
	}
	var body, ctype string
	switch req.URL.Path {
	case "/tailfuse.pac":
		domains, hosts, ips := r.routes.Extras()
		body = PAC(append(r.routes.Suffixes(), domains...), hosts, ips, r.addr)
		ctype = "application/x-ns-proxy-autoconfig"
	case "/healthz":
		body, ctype = "ok", "text/plain"
	default:
		writeHTTPError(c, http.StatusNotFound, "tailfuse router: not found")
		return
	}
	fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n", ctype, len(body))
	if req.Method == http.MethodGet {
		io.WriteString(c, body)
	}
}

func (r *Router) handleConnect(c net.Conn, br *bufio.Reader, req *http.Request) {
	target := req.RequestURI
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, "443" // no port: 443, as on desktop
	}
	host = normalizeHost(host)
	up, oe := r.open(host, port)
	if oe != nil {
		writeHTTPError(c, oe.status, oe.msg)
		return
	}
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	pipe(c, br, up, up)
}

// Only Read/Write, so io.CopyBuffer uses the pooled buffer instead of a
// ReadFrom/WriteTo path that allocates its own.
type onlyReader struct{ io.Reader }
type onlyWriter struct{ io.Writer }

type closeWriter interface{ CloseWrite() error }

// pipe copies both ways until both directions end. Reads from the client go
// through cr (which may hold bytes already buffered), from upstream through ur.
func pipe(client net.Conn, cr io.Reader, up net.Conn, ur io.Reader) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyHalf(up, cr, client)
	}()
	copyHalf(client, ur, up)
	<-done
}

// copyHalf copies src to dst; at EOF it half-closes dst, on error it closes
// both ends so the other direction stops too.
func copyHalf(dst net.Conn, src io.Reader, srcConn net.Conn) {
	bp := bufPool.Get().(*[]byte)
	_, err := io.CopyBuffer(onlyWriter{dst}, onlyReader{src}, *bp)
	bufPool.Put(bp)
	if err == nil {
		if cw, ok := dst.(closeWriter); ok {
			if cw.CloseWrite() == nil {
				return
			}
		}
	}
	dst.Close()
	srcConn.Close()
}
