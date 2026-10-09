// Command relay connects the gate to a candidate booted under bubblewrap
// with its own network namespace (DESIGN.md sec 7, Stage 1). The gate
// builds it once per run and starts it with
//
//	nsenter -t <sandbox pid> -U -n --preserve-credentials relay SOCKET
//
// so it shares only the candidate's user and network namespaces: it
// stays in the host's mount and pid namespaces, where the candidate
// cannot see it. It listens on the Unix socket SOCKET (a path in the
// gate's private temp dir, reachable across network namespaces), prints
// "ready" once listening, and forwards each connection to
// 127.0.0.1:8080 inside the sandbox. Stdlib only.
//
// It exits when its stdin reaches EOF. The gate holds the only write end
// of that pipe and never writes to it, so the relay ends with the gate
// even when the gate is killed outright, instead of keeping the dead
// candidate's namespaces and its listening socket alive (a killed gate
// still leaves its temp dir behind). (Pdeathsig would not do:
// in Go it fires when the creating OS thread exits, not the process.)
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// target is cmd/service's fixed listen address, seen from inside the
// candidate's network namespace.
const target = "127.0.0.1:8080"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: relay SOCKET")
		os.Exit(2)
	}
	ln, err := net.Listen("unix", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	fmt.Println("ready")
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "relay:", err)
			os.Exit(1)
		}
		go forward(c.(*net.UnixConn))
	}
}

// forward copies c to and from a new connection to target, passing each
// side's end of stream on as a half close, until both directions end.
func forward(c *net.UnixConn) {
	defer c.Close()
	u, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return // the client sees the connection close with no response
	}
	t := u.(*net.TCPConn)
	defer t.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(t, c)
		t.CloseWrite()
		close(done)
	}()
	io.Copy(c, t)
	c.CloseWrite()
	<-done
}
