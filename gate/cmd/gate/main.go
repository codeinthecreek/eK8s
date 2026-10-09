// Command gate runs the correctness gate on one candidate wrapper, for
// manual runs; the orchestrator calls package gate directly.
//
// Usage, from the repo root (after `go mod download`, since the gate's go
// commands run with GOPROXY=off):
//
//	go run ./gate/cmd/gate -candidate DIR
//
// Exit status: 0 pass, 1 the candidate failed a check, 2 the gate itself
// failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/codeinthecreek/eK8s/gate"
)

func main() {
	repo := flag.String("repo", ".", "repo checkout providing the fixed tree")
	candidate := flag.String("candidate", "", "directory holding the candidate wrapper's files")
	flag.Parse()
	if *candidate == "" || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := gate.Run(ctx, gate.Config{RepoRoot: *repo, CandidateDir: *candidate})
	for _, r := range rep.Results {
		if r.Pass {
			fmt.Printf("stage %d %-30s PASS\n", r.Stage, r.Check)
		} else {
			fmt.Printf("stage %d %-30s FAIL: %s\n", r.Stage, r.Check, r.Detail)
		}
	}
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "gate error:", err)
		os.Exit(2)
	case !rep.Passed():
		os.Exit(1)
	}
	fmt.Println("image", rep.Image)
}
