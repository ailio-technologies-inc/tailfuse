package engine

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Variables only so tests can shorten them.
var (
	// responseTimeout: how long an upstream may take to start answering
	// (long enough for long-polling).
	responseTimeout = 5 * time.Minute
	// bodyIdleTimeout: a transfer that makes no progress for this long is
	// ended. There is no limit on a transfer that keeps moving.
	bodyIdleTimeout = 5 * time.Minute
)

// hopHeaders apply to one connection only and are never passed on
// (RFC 9110 7.6.1). Connection and Upgrade are rebuilt for upgrades.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", // framing is rewritten by the router
}

// wantsUpgrade reports an upgrade request/response and its Upgrade value.
func wantsUpgrade(h http.Header) (string, bool) {
	up := h.Get("Upgrade")
	if up == "" {
		return "", false
	}
	for _, f := range h.Values("Connection") {
		for _, tok := range strings.Split(f, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return up, true
			}
		}
	}
	return "", false
}

// stripHop removes hop-by-hop headers, including those named in Connection.
func stripHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// proxyHTTP forwards plain-HTTP absolute-form requests (what an HTTP proxy
// setting sends for http:// URLs). The client connection is kept alive and
// the upstream connection reused while the destination stays the same.
// Bodies stream through pooled buffers in both directions; nothing is held
// whole in memory.
func (r *Router) proxyHTTP(c net.Conn, br *bufio.Reader, req *http.Request) {
	var (
		up     net.Conn
		upBR   *bufio.Reader
		upKey  string
		reused bool // up already carried a request
	)
	defer func() {
		if up != nil {
			up.Close()
		}
	}()
	for {
		if req.URL.Scheme != "http" {
			writeHTTPError(c, http.StatusBadRequest, "tailfuse router: only http:// URLs can be proxied without CONNECT")
			return
		}
		host := normalizeHost(req.URL.Hostname())
		port := req.URL.Port()
		if port == "" {
			port = "80"
		}
		key := net.JoinHostPort(host, port)
		if key != upKey {
			if up != nil {
				up.Close()
				up, upBR, upKey = nil, nil, ""
			}
			conn, oe := r.open(host, port)
			if oe != nil {
				writeHTTPError(c, oe.status, oe.msg)
				return
			}
			up, upBR, upKey = conn, bufio.NewReaderSize(conn, headerBufSize), key
			reused = false
		}
		_ = c.SetDeadline(time.Time{})
		res := r.forwardOne(c, br, req, up, upBR, reused)
		if res.retry {
			// The upstream had closed the kept-alive connection before this
			// request reached it: dial again once.
			up.Close()
			conn, oe := r.open(host, port)
			if oe != nil {
				writeHTTPError(c, oe.status, oe.msg)
				return
			}
			up, upBR = conn, bufio.NewReaderSize(conn, headerBufSize)
			res = r.forwardOne(c, br, req, up, upBR, false)
		}
		reused = true
		if res.upgraded || !res.clientAlive {
			return
		}
		if !res.upstreamAlive {
			up.Close()
			up, upBR, upKey = nil, nil, ""
		}
		_ = c.SetReadDeadline(time.Now().Add(idleKeepAlive))
		next, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if !next.URL.IsAbs() || next.Method == http.MethodConnect {
			writeHTTPError(c, http.StatusBadRequest, "tailfuse router: expected an absolute-form request")
			return
		}
		req = next
	}
}

type forwardResult struct {
	clientAlive   bool // the client connection can take another request
	upstreamAlive bool // the upstream connection can be reused
	upgraded      bool // the connection was handed over (101); all done
	retry         bool // nothing was sent to either side; safe to redial
}

// forwardOne sends one request upstream and its response back.
func (r *Router) forwardOne(c net.Conn, br *bufio.Reader, req *http.Request, up net.Conn, upBR *bufio.Reader, canRetry bool) forwardResult {
	upgradeVal, upgrade := wantsUpgrade(req.Header)
	hadCL := req.Header.Get("Content-Length") != ""
	expect := strings.EqualFold(req.Header.Get("Expect"), "100-continue")
	stripHop(req.Header)
	if expect {
		// Answer 100 here and send the body at once: waiting for the
		// upstream's 100 while holding the body back would stall.
		req.Header.Del("Expect")
	}
	chunked := len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked"
	hasBody := chunked || req.ContentLength > 0

	var head strings.Builder
	uri := req.URL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	hostHdr := req.Host
	if hostHdr == "" {
		hostHdr = req.URL.Host
	}
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, uri, hostHdr)
	_ = req.Header.Write(&head)
	switch {
	case upgrade:
		fmt.Fprintf(&head, "Connection: Upgrade\r\nUpgrade: %s\r\n", upgradeVal)
	}
	switch {
	case chunked:
		head.WriteString("Transfer-Encoding: chunked\r\n")
	case req.ContentLength > 0 || (hadCL && req.ContentLength == 0):
		head.WriteString("Content-Length: " + strconv.FormatInt(req.ContentLength, 10) + "\r\n")
	}
	head.WriteString("\r\n")
	fail := func(code int, msg string) forwardResult {
		writeHTTPError(c, code, msg)
		return forwardResult{}
	}
	// Only a request without a body can be sent again on a new connection.
	canRetry = canRetry && !hasBody
	_ = up.SetWriteDeadline(time.Now().Add(bodyIdleTimeout))
	if _, err := io.WriteString(up, head.String()); err != nil {
		if canRetry {
			return forwardResult{retry: true}
		}
		return fail(http.StatusBadGateway, "tailfuse router: upstream write failed")
	}
	if expect && hasBody {
		if _, err := io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return forwardResult{}
		}
	}
	bodyDone := make(chan error, 1)
	if hasBody {
		// Upload while the response is read: an upstream may answer early.
		go func() {
			err := copyBody(idleWriter{up, bodyIdleTimeout}, idleReader{req.Body, c, bodyIdleTimeout}, chunked)
			_ = c.SetReadDeadline(time.Time{})
			bodyDone <- err
		}()
	} else {
		bodyDone <- nil
	}
	waitBody := func() error {
		err := <-bodyDone
		bodyDone <- err // allow a second wait
		return err
	}

	var resp *http.Response
	for {
		_ = up.SetReadDeadline(time.Now().Add(responseTimeout))
		var err error
		resp, err = http.ReadResponse(upBR, req)
		if err != nil {
			if canRetry && upBR.Buffered() == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnReset(err)) {
				return forwardResult{retry: true}
			}
			if hasBody {
				up.Close() // unblock the upload
				waitBody()
			}
			return fail(http.StatusBadGateway, "tailfuse router: no response from upstream")
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			if !(resp.StatusCode == http.StatusContinue && expect) {
				var h strings.Builder
				fmt.Fprintf(&h, "HTTP/1.1 %s\r\n", resp.Status)
				_ = resp.Header.Write(&h)
				h.WriteString("\r\n")
				if _, err := io.WriteString(c, h.String()); err != nil {
					return forwardResult{}
				}
			}
			continue
		}
		break
	}
	_ = up.SetReadDeadline(time.Time{})

	if resp.StatusCode == http.StatusSwitchingProtocols {
		val, ok := wantsUpgrade(resp.Header)
		if !upgrade || !ok {
			return fail(http.StatusBadGateway, "tailfuse router: upstream switched protocols unasked")
		}
		stripHop(resp.Header)
		var h strings.Builder
		fmt.Fprintf(&h, "HTTP/1.1 %s\r\n", resp.Status)
		_ = resp.Header.Write(&h)
		fmt.Fprintf(&h, "Connection: Upgrade\r\nUpgrade: %s\r\n\r\n", val)
		if _, err := io.WriteString(c, h.String()); err != nil {
			return forwardResult{upgraded: true}
		}
		if waitBody() != nil {
			return forwardResult{upgraded: true}
		}
		_ = c.SetDeadline(time.Time{})
		_ = up.SetDeadline(time.Time{})
		pipe(c, br, up, upBR)
		return forwardResult{upgraded: true}
	}

	noBody := req.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
	respChunkedIn := len(resp.TransferEncoding) > 0
	// The router frames the body for the client itself, so an upstream that
	// closes (or never said how long its body is) does not end the client's
	// connection; only the client's own wish or HTTP/1.0 does.
	clientAlive := !req.Close && req.ProtoAtLeast(1, 1)
	upstreamAlive := !resp.Close
	stripHop(resp.Header)
	var h strings.Builder
	fmt.Fprintf(&h, "HTTP/1.1 %s\r\n", resp.Status)
	_ = resp.Header.Write(&h)
	outChunked := false
	switch {
	case noBody:
		if req.Method == http.MethodHead && resp.ContentLength >= 0 && !respChunkedIn {
			h.WriteString("Content-Length: " + strconv.FormatInt(resp.ContentLength, 10) + "\r\n")
		}
	case resp.ContentLength >= 0:
		h.WriteString("Content-Length: " + strconv.FormatInt(resp.ContentLength, 10) + "\r\n")
	case req.ProtoAtLeast(1, 1):
		h.WriteString("Transfer-Encoding: chunked\r\n")
		outChunked = true
	default:
		clientAlive = false // HTTP/1.0 client: the body ends when we close
	}
	if !clientAlive {
		h.WriteString("Connection: close\r\n")
	}
	h.WriteString("\r\n")
	_ = c.SetWriteDeadline(time.Now().Add(bodyIdleTimeout))
	if _, err := io.WriteString(c, h.String()); err != nil {
		resp.Body.Close()
		return forwardResult{}
	}
	if !noBody {
		err := copyBody(idleWriter{c, bodyIdleTimeout}, idleReader{resp.Body, up, bodyIdleTimeout}, outChunked)
		if err != nil {
			resp.Body.Close()
			return forwardResult{}
		}
	}
	resp.Body.Close()
	_ = c.SetWriteDeadline(time.Time{})
	_ = up.SetReadDeadline(time.Time{})
	if err := waitBody(); err != nil {
		return forwardResult{}
	}
	_ = up.SetWriteDeadline(time.Time{})
	return forwardResult{clientAlive: clientAlive, upstreamAlive: upstreamAlive}
}

// idleReader ends a read that waits longer than d (no progress); a transfer
// that keeps moving is never cut.
type idleReader struct {
	r io.Reader
	c net.Conn
	d time.Duration
}

func (x idleReader) Read(p []byte) (int, error) {
	_ = x.c.SetReadDeadline(time.Now().Add(x.d))
	return x.r.Read(p)
}

type idleWriter struct {
	c net.Conn
	d time.Duration
}

func (x idleWriter) Write(p []byte) (int, error) {
	_ = x.c.SetWriteDeadline(time.Now().Add(x.d))
	return x.c.Write(p)
}

var errShortWrite = errors.New("short write")

func isConnReset(err error) bool {
	var ne *net.OpError
	return errors.As(err, &ne) && !ne.Timeout()
}

// copyBody streams src to dst with a pooled buffer, optionally as HTTP/1.1
// chunks (ending with the last chunk).
func copyBody(dst io.Writer, src io.Reader, chunked bool) error {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	if !chunked {
		_, err := io.CopyBuffer(onlyWriter{dst}, onlyReader{src}, buf)
		return err
	}
	// Leave room for the chunk header and trailing CRLF in the same buffer,
	// so each chunk is one write.
	const pre = 10 // up to 8 hex digits + CRLF
	data := buf[pre : len(buf)-2]
	for {
		n, rerr := src.Read(data)
		if n > 0 {
			hdr := strconv.FormatInt(int64(n), 16) + "\r\n"
			start := pre - len(hdr)
			copy(buf[start:], hdr)
			end := pre + n
			buf[end], buf[end+1] = '\r', '\n'
			w, err := dst.Write(buf[start : end+2])
			if err != nil {
				return err
			}
			if w != end+2-start {
				return errShortWrite
			}
		}
		if rerr == io.EOF {
			_, err := io.WriteString(dst, "0\r\n\r\n")
			return err
		}
		if rerr != nil {
			return rerr
		}
	}
}
