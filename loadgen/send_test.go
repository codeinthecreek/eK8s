package loadgen

import (
	"bytes"
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
	_, resp := NewSender().send(srv.URL, hashRequest(newPassword()))
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
	actual, resp := s.send(srv.URL, hashRequest(newPassword()))
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
	_, resp := NewSender().send(srv.URL, req)
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
	_, resp := NewSender().send(srv.URL, req)
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
	_, resp := NewSender().send(srv.URL, req)
	if resp.Err == nil || resp.Status != 0 || resp.Latency <= 0 {
		t.Fatalf("send to a closed server = status %d, Latency %v, Err %v; want 0, positive and an error", resp.Status, resp.Latency, resp.Err)
	}
	check(t, NewChecker(), req, resp, verdict(classifier.ReasonTransportError), nil)
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
		if _, resp := s.send(srv.URL, req); resp.Err != nil {
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
	_, resp := NewSender().send(srv.URL, req)
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
		if _, resp := s.send(srv.URL, hashRequest(newPassword())); resp.Err != nil {
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
		actual, resp := s.send(srv.URL, hashRequest(newPassword()))
		if resp.Err != nil {
			t.Fatalf("send %d: %v", i, resp.Err)
		}
		if actual <= 0 || actual < prev {
			t.Errorf("send %d: actual = %v after %v, want positive and non-decreasing", i, actual, prev)
		}
		prev = actual
	}
}
