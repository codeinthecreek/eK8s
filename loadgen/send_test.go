package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// serve starts a test server running h and closes it when the test ends.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// sendOK runs s.send and fails the test on a fault.
func sendOK(t *testing.T, s *Sender, baseURL string, req Request) (time.Duration, Response) {
	t.Helper()
	actual, resp, fault := s.send(baseURL, req)
	if fault != nil {
		t.Fatalf("send: fault %v", fault)
	}
	return actual, resp
}

// TestSendLatencyIncludesBody checks that Latency runs to the full body
// read: the header arrives at once, the body 200 ms later.
func TestSendLatencyIncludesBody(t *testing.T) {
	const delay = 200 * time.Millisecond
	body := hashJSON(randomExactPHC())
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(delay)
		w.Write(body)
	})
	_, resp := sendOK(t, NewSender(), srv.URL, hashRequest(newPassword()))
	if resp.Err != nil {
		t.Fatalf("send: %v", resp.Err)
	}
	if resp.Status != http.StatusOK || !bytes.Equal(resp.Body, body) {
		t.Errorf("send = status %d body %q, want 200 %q", resp.Status, resp.Body, body)
	}
	if resp.Latency < delay {
		t.Errorf("Latency = %v, want at least %v", resp.Latency, delay)
	}
}

// TestSendLatencyFromSend checks that actual is read, and Latency starts,
// before the request goes out: the handler waits 200 ms before writing
// anything.
func TestSendLatencyFromSend(t *testing.T) {
	const delay = 200 * time.Millisecond
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Write(hashJSON(randomExactPHC()))
	})
	s := NewSender()
	actual, resp := sendOK(t, s, srv.URL, hashRequest(newPassword()))
	end := time.Since(s.base)
	if resp.Err != nil {
		t.Fatalf("send: %v", resp.Err)
	}
	if resp.Latency < delay || end-actual < delay {
		t.Errorf("Latency = %v, send returned %v after actual; want both at least %v", resp.Latency, end-actual, delay)
	}
}

// TestSendBodyCap checks that an over-cap 2xx /hash body is read to
// MaxBodyBytes+1 bytes with Err nil, so it classifies as malformed_body.
func TestSendBodyCap(t *testing.T) {
	body := padTo(hashJSON(randomExactPHC()), classifier.MaxBodyBytes+100)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	req := hashRequest(newPassword())
	_, resp := sendOK(t, NewSender(), srv.URL, req)
	if resp.Err != nil {
		t.Fatalf("send: %v", resp.Err)
	}
	if len(resp.Body) != classifier.MaxBodyBytes+1 {
		t.Errorf("len(Body) = %d, want %d", len(resp.Body), classifier.MaxBodyBytes+1)
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonMalformedBody), nil)
}

// TestSendReadFails checks that a body cut short by the connection
// closing sets Err, so the truncated body classifies as transport_error,
// not malformed_body.
func TestSendReadFails(t *testing.T) {
	body := hashJSON(randomExactPHC())
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promise the whole body, send half of it, then close.
		rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n")
		rw.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
		rw.Write(body[:len(body)/2])
		rw.Flush()
	})
	req := hashRequest(newPassword())
	_, resp := sendOK(t, NewSender(), srv.URL, req)
	if resp.Err == nil {
		t.Fatalf("send = status %d body %q, Err nil; want a read error", resp.Status, resp.Body)
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonTransportError), nil)
}

// TestSendRefused checks that a failed connection sets Err, with no
// status.
func TestSendRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	req := hashRequest(newPassword())
	_, resp := sendOK(t, NewSender(), srv.URL, req)
	if resp.Err == nil || resp.Status != 0 || resp.Latency <= 0 {
		t.Fatalf("send to a closed server = status %d, Latency %v, Err %v; want 0, positive and an error", resp.Status, resp.Latency, resp.Err)
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonTransportError), nil)
}

// TestSendBuildFault checks that a request that cannot be built is a
// fault, with actual and resp zero.
func TestSendBuildFault(t *testing.T) {
	actual, resp, fault := NewSender().send(unparseableURL, hashRequest(newPassword()))
	if fault == nil || actual != 0 || resp.Status != 0 || resp.Header != nil || resp.Body != nil || resp.Latency != 0 || resp.Err != nil {
		t.Errorf("send to %q = %v, %+v, fault %v; want 0, a zero Response and a fault", unparseableURL, actual, resp, fault)
	}
}

// TestSendRequestShape checks that each kind of request goes out as a
// POST to its path, with its body unchanged and Content-Type
// application/json.
func TestSendRequestShape(t *testing.T) {
	type seen struct {
		method, uri, contentType string
		body                     []byte
	}
	got := make(chan seen, 1)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		got <- seen{r.Method, r.RequestURI, r.Header.Get("Content-Type"), b}
	})
	reqs := []Request{hashRequest(newPassword()), verifyRequest(Pair{newPassword(), randomExactPHC(), true})}
	for _, neg := range burstNegatives(t) {
		reqs = append(reqs, neg)
	}
	s := NewSender()
	for _, req := range reqs {
		if _, resp := sendOK(t, s, srv.URL, req); resp.Err != nil {
			t.Fatalf("send %s %s: %v", req.Kind, req.Class, resp.Err)
		}
		g := <-got
		if g.method != http.MethodPost || g.uri != req.Path || g.contentType != "application/json" || !bytes.Equal(g.body, req.Body) {
			t.Errorf("%s %s: server saw %s %s, Content-Type %q, body %q; want POST %s, application/json, %q",
				req.Kind, req.Class, g.method, g.uri, g.contentType, g.body, req.Path, req.Body)
		}
	}
}

// TestSendNoRedirect checks that a 307 is passed on as the response, so
// it classifies as bad_status, and is not followed: the request never
// reaches the address it names.
func TestSendNoRedirect(t *testing.T) {
	var reached atomic.Bool
	peer := serve(t, func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.Write(hashJSON(randomExactPHC()))
	})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, peer.URL+HashPath, http.StatusTemporaryRedirect)
	})
	req := hashRequest(newPassword())
	_, resp := sendOK(t, NewSender(), srv.URL, req)
	if resp.Err != nil || resp.Status != http.StatusTemporaryRedirect {
		t.Fatalf("send = status %d, Err %v; want 307, nil", resp.Status, resp.Err)
	}
	if reached.Load() {
		t.Error("send followed the redirect")
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonBadStatus), nil)
}

// TestSendNoKeepAlive checks that each send opens its own connection.
func TestSendNoKeepAlive(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	s := NewSender()
	for range 2 {
		if _, resp := sendOK(t, s, srv.URL, hashRequest(newPassword())); resp.Err != nil {
			t.Fatalf("send: %v", resp.Err)
		}
	}
	if n := conns.Load(); n != 2 {
		t.Errorf("two sends opened %d connections, want 2", n)
	}
}

func TestNewClient(t *testing.T) {
	c := NewSender().client
	if c.Timeout != clientTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, clientTimeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == http.DefaultTransport {
		t.Fatalf("Transport = %T %p, want a clone of http.DefaultTransport", c.Transport, c.Transport)
	}
	if !tr.DisableKeepAlives || tr.Proxy != nil {
		t.Errorf("DisableKeepAlives = %v, Proxy nil = %v; want true, true", tr.DisableKeepAlives, tr.Proxy == nil)
	}
	if tr.MaxResponseHeaderBytes != maxResponseHeaderBytes || maxResponseHeaderBytes != 1<<10 {
		t.Errorf("MaxResponseHeaderBytes = %d, maxResponseHeaderBytes = %d; want both 1 KiB (DESIGN.md sec 10)", tr.MaxResponseHeaderBytes, maxResponseHeaderBytes)
	}
	if clientTimeout <= 3*classifier.HangTimeout {
		t.Errorf("clientTimeout = %v, want more than cmd/service's WriteTimeout, %v", clientTimeout, 3*classifier.HangTimeout)
	}
	if want := 3*classifier.HangTimeout + time.Second; clientTimeout != want {
		t.Errorf("clientTimeout = %v, want WriteTimeout + 1s, %v (DESIGN.md sec 10)", clientTimeout, want)
	}
	if c.CheckRedirect == nil {
		t.Error("CheckRedirect is nil: the client follows redirects")
	}
}

// TestWriteTimeoutPinned fails when cmd/service's WriteTimeout no longer
// reads 3 * classifier.HangTimeout, the value clientTimeout must outlast
// (docs/decisions.md, PR #41 client-timeout entry).
func TestWriteTimeoutPinned(t *testing.T) {
	const want = "3 * classifier.HangTimeout"
	path := filepath.Join("..", "cmd", "service", "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := func(n ast.Node) string {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, n); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	var got []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || lit.Type == nil || src(lit.Type) != "http.Server" {
			return true
		}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "WriteTimeout" {
					got = append(got, src(kv.Value))
				}
			}
		}
		return true
	})
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s: http.Server WriteTimeout fields = %q, want exactly one, %q", path, got, want)
	}
}

// TestSendActual checks that actual is positive and non-decreasing
// across sequential sends.
func TestSendActual(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	s := NewSender()
	var prev time.Duration
	for i := range 5 {
		actual, resp := sendOK(t, s, srv.URL, hashRequest(newPassword()))
		if resp.Err != nil {
			t.Fatalf("send %d: %v", i, resp.Err)
		}
		if actual <= 0 || actual < prev {
			t.Errorf("send %d: actual = %v after %v, want positive and non-decreasing", i, actual, prev)
		}
		prev = actual
	}
}

const (
	// headerBudget is the most a headerServer writes per connection,
	// so that no test depends on the client to stop a flood.
	headerBudget = 64 << 10
	// headerDeadline bounds each header-cap send, well under
	// clientTimeout, so that a missing cap fails the test at its own
	// bound instead of waiting out the client.
	headerDeadline = 3 * time.Second
	// readBuffer is the transport's default read buffer: the client may
	// read up to one buffer past what it parses.
	readBuffer = 4 << 10
)

// headerServer is a raw TCP server for the header-cap tests. On each
// connection it reads the request, writes pieces in order with gap
// between them, at most headerBudget bytes in all, then holds the
// connection open until the client closes it or Close is called, so
// that only the client ends the exchange.
type headerServer struct {
	URL string
	ln  net.Listener

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// newHeaderServer starts a headerServer and closes it when the test or
// benchmark ends.
func newHeaderServer(tb testing.TB, gap time.Duration, pieces [][]byte) *headerServer {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	s := &headerServer{URL: "http://" + ln.Addr().String(), ln: ln}
	tb.Cleanup(s.Close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if !s.track(c) {
				c.Close()
				return
			}
			go s.handle(c, gap, pieces)
		}
	}()
	return s
}

func (s *headerServer) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns = append(s.conns, c)
	return true
}

func (s *headerServer) handle(c net.Conn, gap time.Duration, pieces [][]byte) {
	defer c.Close()
	if readRequest(c) != nil {
		return
	}
	written := 0
	for i, p := range pieces {
		if i > 0 && gap > 0 {
			time.Sleep(gap)
		}
		p = p[:min(len(p), headerBudget-written)]
		n, err := c.Write(p)
		written += n
		if err != nil || written == headerBudget {
			break
		}
	}
	var one [1]byte
	for {
		if _, err := c.Read(one[:]); err != nil {
			return
		}
	}
}

// readRequest reads and discards one request, so that closing the
// connection later sends no reset over unread request bytes. What it
// allocates is garbage once it returns.
func readRequest(c net.Conn) error {
	r, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, r.Body)
	return err
}

// Close stops the listener and closes every connection, which ends any
// send still waiting on one.
func (s *headerServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.ln.Close()
	for _, c := range s.conns {
		c.Close()
	}
}

// countingConn adds the bytes read from it to n.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	m, err := c.Conn.Read(p)
	c.n.Add(int64(m))
	return m, err
}

// countReads makes s's client add every byte it reads from a
// connection to n.
func countReads(tb testing.TB, s *Sender, n *atomic.Int64) {
	tb.Helper()
	tr := s.client.Transport.(*http.Transport)
	dial := tr.DialContext
	if dial == nil {
		tb.Fatal("Transport.DialContext is nil")
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countingConn{c, n}, nil
	}
}

// headerLines returns a response head of exactly n bytes, the blank
// line included: status, then the fixed header lines, then X-Pad lines
// of at most 64 bytes each. Each line is its own piece.
func headerLines(t *testing.T, status string, fixed []string, n int) [][]byte {
	t.Helper()
	pieces := [][]byte{[]byte(status + "\r\n")}
	r := n - len(status) - 2 - 2
	for _, f := range fixed {
		pieces = append(pieces, []byte(f+"\r\n"))
		r -= len(f) + 2
	}
	const minLine = len("X-Pad: a\r\n")
	for r > 0 {
		l := min(64, r)
		if r-l > 0 && r-l < minLine {
			l = r - minLine
		}
		if l < minLine {
			t.Fatalf("headerLines: %d bytes left, less than one X-Pad line", r)
		}
		pieces = append(pieces, []byte("X-Pad: "+strings.Repeat("a", l-len("X-Pad: \r\n"))+"\r\n"))
		r -= l
	}
	if r < 0 {
		t.Fatalf("headerLines: status and fixed lines exceed %d bytes", n)
	}
	return append(pieces, []byte("\r\n"))
}

// hashResponse returns the pieces of a valid 200 /hash response whose
// head is exactly headBytes, and the hash it carries.
func hashResponse(t *testing.T, headBytes int) ([][]byte, string) {
	t.Helper()
	hash := randomExactPHC()
	body := hashJSON(hash)
	head := headerLines(t, "HTTP/1.1 200 OK", []string{"Content-Type: application/json", "Content-Length: " + strconv.Itoa(len(body))}, headBytes)
	return append(head, body), hash
}

// sendWithin runs send in its own goroutine and fails the test if it has
// not returned within headerDeadline.
func sendWithin(t *testing.T, s *Sender, baseURL string, req Request) Response {
	t.Helper()
	done := make(chan Response, 1)
	go func() {
		_, resp, _ := s.send(baseURL, req)
		done <- resp
	}()
	select {
	case resp := <-done:
		return resp
	case <-time.After(headerDeadline):
		t.Fatalf("send did not return within %v (clientTimeout %v): the header cap did not end it", headerDeadline, clientTimeout)
		return Response{}
	}
}

// sendOverCap runs req against pieces, served with gap between them, and
// checks that it fails on the header cap: Err names the exceeded limit,
// the client read at most the cap plus one read buffer, and the Checker
// gives transport_error.
func sendOverCap(t *testing.T, gap time.Duration, pieces [][]byte) {
	t.Helper()
	srv := newHeaderServer(t, gap, pieces)
	s := NewSender()
	var read atomic.Int64
	countReads(t, s, &read)
	req := hashRequest(newPassword())
	resp := sendWithin(t, s, srv.URL, req)
	want := fmt.Sprintf("exceeded %d bytes", maxResponseHeaderBytes)
	if resp.Err == nil || !strings.Contains(resp.Err.Error(), want) {
		t.Errorf("send = status %d, Err %v; want an error naming %q", resp.Status, resp.Err, want)
	}
	n := read.Load()
	if n > maxResponseHeaderBytes+readBuffer {
		t.Errorf("client read %d bytes, want at most %d (the cap plus one read buffer)", n, maxResponseHeaderBytes+readBuffer)
	}
	t.Logf("client read %d bytes before %v", n, resp.Err)
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonTransportError), nil)
}

// TestSendHeaderOverCap: a valid 200 whose head is one byte over the
// cap, sent at once.
func TestSendHeaderOverCap(t *testing.T) {
	pieces, _ := hashResponse(t, maxResponseHeaderBytes+1)
	sendOverCap(t, 0, pieces)
}

// TestSendHeaderDrip: a head of twice the cap, one line every 5 ms, so
// the cap ends it after about 18 lines (~85 ms), not the client timeout.
func TestSendHeaderDrip(t *testing.T) {
	pieces, _ := hashResponse(t, 2*maxResponseHeaderBytes)
	sendOverCap(t, 5*time.Millisecond, pieces)
}

// TestSendHeader1xxFlood: 4 102 responses of 512 bytes each, every one
// under the cap but 2 KiB together, then a valid 200: every 1xx counts
// toward the one cap.
func TestSendHeader1xxFlood(t *testing.T) {
	var pieces [][]byte
	for range 4 {
		pieces = append(pieces, bytes.Join(headerLines(t, "HTTP/1.1 102 Processing", nil, 512), nil))
	}
	final, _ := hashResponse(t, 200)
	sendOverCap(t, 0, append(pieces, final...))
}

// TestSendHeaderAtCap: a valid 200 whose head is exactly the cap
// classifies normally, so the cap is not off by one against
// TestSendHeaderOverCap's one byte more.
func TestSendHeaderAtCap(t *testing.T) {
	pieces, hash := hashResponse(t, maxResponseHeaderBytes)
	srv := newHeaderServer(t, 0, pieces)
	req := hashRequest(newPassword())
	resp := sendWithin(t, NewSender(), srv.URL, req)
	if resp.Err != nil || resp.Status != http.StatusOK {
		t.Fatalf("send = status %d, Err %v; want 200, nil", resp.Status, resp.Err)
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonNone), &HashPair{req.Password, hash})
}
