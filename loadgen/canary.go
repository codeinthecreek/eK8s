package loadgen

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// canaryTimeout is the policy canary's dial timeout (DESIGN.md sec 11).
const canaryTimeout = time.Second

// The policy canary's results (DESIGN.md sec 11).
const (
	canaryConnected = "connected" // policy enforcement is off: the half is excluded
	canaryRefused   = "refused"   // the policy stopped the connection
	canaryError     = "error"     // shows nothing about the policy: the half is excluded
)

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
		return canaryConnected
	case errors.Is(err, syscall.EAGAIN):
		return canaryError
	case errors.As(err, &ne) && ne.Timeout(),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH):
		return canaryRefused
	}
	return canaryError
}
