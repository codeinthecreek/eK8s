package gate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
)

// candidateServer serves handler over real HTTP and returns an interop
// that reaches it as the gate reaches a candidate: post's
// "http://candidate" URLs, dialed to the server.
func candidateServer(t *testing.T, label string, handler http.HandlerFunc) *interop {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	return newInterop(client, label)
}

func matchingPair(t *testing.T) testvectors.Pair {
	t.Helper()
	for _, p := range testvectors.Pairs() {
		if p.Match {
			return p
		}
	}
	t.Fatal("no matching pool pair")
	return testvectors.Pair{}
}

// TestPostBodyCap: post reads at most classifier.MaxBodyBytes+1 bytes and
// passes them with Err nil, cap hit or not, so a 2xx body of exactly
// MaxBodyBytes passes and any longer one is malformed_body; a body cut
// off mid-read is transport_error. It goes through mutantVerify, the
// entry point that takes post's Observation straight to the classifier
// without Argon2. The padding is trailing JSON whitespace, so a body cut
// at MaxBodyBytes would still be a valid answer.
func TestPostBodyCap(t *testing.T) {
	pair := matchingPair(t)
	padded := func(n int) []byte {
		b := []byte(`{"match":true}`)
		return append(b, bytes.Repeat([]byte(" "), n-len(b))...)
	}
	for _, tc := range []struct {
		name    string
		n       int
		wantLen int
		want    string
	}{
		{"exactly MaxBodyBytes", classifier.MaxBodyBytes, classifier.MaxBodyBytes, ""},
		{"MaxBodyBytes+1", classifier.MaxBodyBytes + 1, classifier.MaxBodyBytes + 1, "malformed_body"},
		{"1 MiB", 1 << 20, classifier.MaxBodyBytes + 1, "malformed_body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := newLabel()
			s := candidateServer(t, label, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(variantHeader, label)
				w.Write(padded(tc.n))
			})
			o, problem := s.post(context.Background(), "/verify", nil)
			if problem != "" || o.Err != nil || o.Status != http.StatusOK || len(o.Body) != tc.wantLen {
				t.Fatalf("post: problem %q, err %v, status %d, %d body bytes; want 200 with %d bytes and Err nil", problem, o.Err, o.Status, len(o.Body), tc.wantLen)
			}
			got := s.mutantVerify(context.Background(), pair)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, "("+tc.want+",") {
				t.Errorf("mutantVerify = %q, want reason %q", got, tc.want)
			}
		})
	}

	t.Run("cut off mid-read", func(t *testing.T) {
		label := newLabel()
		s := candidateServer(t, label, func(w http.ResponseWriter, r *http.Request) {
			c, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n" + variantHeader + ": " + label + "\r\n\r\n" + `{"match":`)
			rw.Flush()
		})
		o, problem := s.post(context.Background(), "/verify", nil)
		if problem != "" || !errors.Is(o.Err, io.ErrUnexpectedEOF) || o.Status != http.StatusOK {
			t.Fatalf("post: problem %q, err %v, status %d; want 200 and io.ErrUnexpectedEOF", problem, o.Err, o.Status)
		}
		if got := s.mutantVerify(context.Background(), pair); !strings.Contains(got, "(transport_error, status 200") {
			t.Errorf("mutantVerify = %q, want transport_error", got)
		}
	})
}
