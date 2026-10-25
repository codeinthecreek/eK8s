package loadgen

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// clientTimeout is cmd/service's WriteTimeout (cmd/service/main.go) + 1s,
// so every response the service can write is observed and classified
// (DESIGN.md sec 10).
const clientTimeout = 3*classifier.HangTimeout + time.Second

// maxResponseHeaderBytes caps a response's header bytes, status line
// included, counted together with every 1xx response before it: loadgen
// sets no httptrace Got1xxResponse hook, the one case in which net/http
// gives each 1xx its own limit. A response over it fails the request, a
// transport_error and so the variant's failure. 4 KiB is about 25 times
// gen 0's largest header block (164 B, a 503 with Retry-After) and bounds
// what each in-flight request holds while its headers arrive (DESIGN.md
// sec 10).
const maxResponseHeaderBytes = 4 << 10

// newClient returns loadgen's HTTP client: one connection per request, no
// proxy, no redirects followed, response headers capped at
// maxResponseHeaderBytes, and a timeout of clientTimeout. A
// redirect is the variant's own response (a 3xx): following it would let
// a variant send loadgen's request, and its load, to any address
// loadgen's egress reaches, a peer variant included.
func newClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	tr.Proxy = nil
	tr.MaxResponseHeaderBytes = maxResponseHeaderBytes
	return &http.Client{
		Transport: tr,
		Timeout:   clientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Sender sends requests and times them. It is made with NewSender and is
// safe for concurrent use. Its base anchors every monotonic time in a
// Record, so those compare only within one process (one boot_id).
type Sender struct {
	client *http.Client
	base   time.Time // carries its monotonic reading
}

// NewSender returns a Sender whose base is now.
func NewSender() *Sender {
	return &Sender{client: newClient(), base: time.Now()}
}

// send POSTs req.Body to baseURL+req.Path and returns when it was sent,
// as a duration since the Sender's base, read just before the request
// goes out, and what came back.
//
// It reads at most classifier.MaxBodyBytes+1 bytes of the body and passes
// them with Err nil whether or not it hit that cap, so the classifier
// sees an over-cap 2xx body (malformed_body). Err is set only for a
// failed request: a transport error, or a failed body read, whose
// truncated Body is then a transport_error, never malformed_body.
// Latency runs from the send to the full body read.
//
// A request that cannot be built (a bad baseURL) is loadgen's own fault,
// not the variant's: send returns it as fault, with actual and resp
// zero, and sends nothing.
func (s *Sender) send(baseURL string, req Request) (actual time.Duration, resp Response, fault error) {
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+req.Path, bytes.NewReader(req.Body))
	if err != nil {
		return 0, Response{}, fmt.Errorf("loadgen: building the request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	actual = t0.Sub(s.base)
	hr, err := s.client.Do(r)
	if err != nil {
		resp.Err = err
		resp.Latency = time.Since(t0)
		return actual, resp, nil
	}
	defer hr.Body.Close()
	resp.Status, resp.Header = hr.StatusCode, hr.Header
	resp.Body, resp.Err = io.ReadAll(io.LimitReader(hr.Body, classifier.MaxBodyBytes+1))
	resp.Latency = time.Since(t0)
	return actual, resp, nil
}
