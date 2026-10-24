package loadgen

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// canaryTimeout is the policy canary's dial timeout (DESIGN.md sec 11).
const canaryTimeout = time.Second

// canary makes the policy canary's one connection attempt, to addr (the
// API server's ClusterIP), which loadgen's own NetworkPolicy must refuse
// (DESIGN.md sec 11), and returns canaryResult's reading of it. A
// connection that is made is closed at once.
func canary(ctx context.Context, addr string) string {
	d := net.Dialer{Timeout: canaryTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		conn.Close()
	}
	return canaryResult(err)
}

// canaryResult reads a canary dial's error: connected for none; refused
// only for what a policy does to a connection, a timeout (a dropped
// packet) or ECONNREFUSED, EHOSTUNREACH or ENETUNREACH (a reject); error
// for anything else. EAGAIN is error although syscall.Errno.Timeout
// calls it a timeout: on connect it is local (no free port), and shows
// nothing about the policy.
func canaryResult(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return wire.CanaryConnected
	case errors.Is(err, syscall.EAGAIN):
		return wire.CanaryError
	case errors.As(err, &ne) && ne.Timeout(),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH):
		return wire.CanaryRefused
	}
	return wire.CanaryError
}
