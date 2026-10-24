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

	"github.com/codeinthecreek/eK8s/loadgen/wire"
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
		{"none", nil, wire.CanaryConnected},
		{"ECONNREFUSED", dialErr(syscall.ECONNREFUSED), wire.CanaryRefused},
		{"EHOSTUNREACH", dialErr(syscall.EHOSTUNREACH), wire.CanaryRefused},
		{"ENETUNREACH", dialErr(syscall.ENETUNREACH), wire.CanaryRefused},
		{"wrapped ECONNREFUSED", fmt.Errorf("x: %w", dialErr(syscall.ECONNREFUSED)), wire.CanaryRefused},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, wire.CanaryRefused},
		{"ETIMEDOUT", dialErr(syscall.ETIMEDOUT), wire.CanaryRefused}, // syscall.Errno.Timeout
		{"EAGAIN", dialErr(syscall.EAGAIN), wire.CanaryError},         // Errno.Timeout, but local
		{"EPERM", dialErr(syscall.EPERM), wire.CanaryError},
		{"EACCES", dialErr(syscall.EACCES), wire.CanaryError},
		{"ECONNRESET", dialErr(syscall.ECONNRESET), wire.CanaryError},
		{"EADDRNOTAVAIL", dialErr(syscall.EADDRNOTAVAIL), wire.CanaryError},
		{"cancelled", &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}, wire.CanaryError},
		{"bare ECONNREFUSED text", errors.New("connect: connection refused"), wire.CanaryError},
		{"unrelated", errors.New("boom"), wire.CanaryError},
	} {
		if got := canaryResult(tc.err); got != tc.want {
			t.Errorf("canaryResult(%s: %v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestCanaryConnected(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if got := canary(t.Context(), srv.Listener.Addr().String()); got != wire.CanaryConnected {
		t.Errorf("canary to a listener = %q, want %q", got, wire.CanaryConnected)
	}
}

func TestCanaryRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if got := canary(t.Context(), addr); got != wire.CanaryRefused {
		t.Errorf("canary to a closed port = %q, want %q", got, wire.CanaryRefused)
	}
}

func TestCanaryEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	cfg := validConfig()
	cfg.CanaryAddr = srv.Listener.Addr().String()
	c := newTestControl(t, cfg)
	got := decode[wire.Canary](t, mustCall(t, c.Handler(), "POST", "/canary", http.StatusOK))
	if want := (wire.Canary{BootID: c.bootID, Result: wire.CanaryConnected}); got != want {
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
	if got, want := decode[wire.Canary](t, rec), (wire.Canary{BootID: c.bootID, Result: wire.CanaryConnected}); got != want {
		t.Errorf("POST /canary with a cancelled request context = %+v, want %+v", got, want)
	}
}
