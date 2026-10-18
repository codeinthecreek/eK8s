package loadgen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
)

// dialErr wraps errno as a failed TCP connect does.
func dialErr(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
}

func TestCanaryResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"none", nil, canaryConnected},
		{"ECONNREFUSED", dialErr(syscall.ECONNREFUSED), canaryRefused},
		{"EHOSTUNREACH", dialErr(syscall.EHOSTUNREACH), canaryRefused},
		{"ENETUNREACH", dialErr(syscall.ENETUNREACH), canaryRefused},
		{"wrapped ECONNREFUSED", fmt.Errorf("x: %w", dialErr(syscall.ECONNREFUSED)), canaryRefused},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, canaryRefused},
		{"ETIMEDOUT", dialErr(syscall.ETIMEDOUT), canaryRefused}, // syscall.Errno.Timeout
		{"EAGAIN", dialErr(syscall.EAGAIN), canaryError},         // Errno.Timeout, but local
		{"EPERM", dialErr(syscall.EPERM), canaryError},
		{"EACCES", dialErr(syscall.EACCES), canaryError},
		{"ECONNRESET", dialErr(syscall.ECONNRESET), canaryError},
		{"EADDRNOTAVAIL", dialErr(syscall.EADDRNOTAVAIL), canaryError},
		{"cancelled", &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}, canaryError},
		{"bare ECONNREFUSED text", errors.New("connect: connection refused"), canaryError},
		{"unrelated", errors.New("boom"), canaryError},
	} {
		if got := canaryResult(tc.err); got != tc.want {
			t.Errorf("canaryResult(%s: %v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestCanaryConnected(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if got := canary(t.Context(), srv.Listener.Addr().String()); got != canaryConnected {
		t.Errorf("canary to a listener = %q, want %q", got, canaryConnected)
	}
}

func TestCanaryRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if got := canary(t.Context(), addr); got != canaryRefused {
		t.Errorf("canary to a closed port = %q, want %q", got, canaryRefused)
	}
}

func TestCanaryEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	cfg := validConfig()
	cfg.CanaryAddr = srv.Listener.Addr().String()
	c := newTestControl(t, cfg)
	got := decode[wireCanary](t, mustCall(t, c.Handler(), "POST", "/canary", http.StatusOK))
	if want := (wireCanary{c.bootID, canaryConnected}); got != want {
		t.Errorf("POST /canary = %+v, want %+v", got, want)
	}
}

// TestCanaryEndpointDetached checks that POST /canary dials detached from
// the request's context: with that context already cancelled, a dial to
// a listener still reads connected, not error.
func TestCanaryEndpointDetached(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	cfg := validConfig()
	cfg.CanaryAddr = srv.Listener.Addr().String()
	c := newTestControl(t, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/canary", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /canary = %d %s, want 200", rec.Code, rec.Body)
	}
	if got, want := decode[wireCanary](t, rec), (wireCanary{c.bootID, canaryConnected}); got != want {
		t.Errorf("POST /canary with a cancelled request context = %+v, want %+v", got, want)
	}
}
