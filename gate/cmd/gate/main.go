// Command gate runs the correctness gate on one candidate wrapper, for
// manual runs; the orchestrator calls package gate directly.
//
// Usage, from the repo root (after `go mod download`, since the gate's go
// commands run with GOPROXY=off):
//
//	go run ./gate/cmd/gate -candidate DIR
//
// With -stage5 it runs Stage 5 alone, on an image already built and a
// running kind cluster with loadgen deployed (deploy/up.sh, then
// deploy/loadgen-up.sh <r>) and stress-ng switched on:
//
//	go run ./gate/cmd/gate -stage5 -image ek8s-wrapper:<12 hex> -id N -rate R
//
// It leaves the candidate's namespace ek8s-v<N> and its loadgen target
// where Stage 5 leaves them (DESIGN.md sec 11); deploy/stage5-smoke.sh
// cleans up after itself.
//
// With -stage5, once the burst's records were read, it prints one more
// line after the checks, the burst's stall counts (gate.Report.
// Stage5Stall), which never change the exit status:
//
//	stall: records N oht N stall-runs N tail N longest N start-index I start-wall T threshold N
//
// with start-wall in RFC 3339, UTC, or "-" with no OHT record.
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
	"time"

	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/stallrun"
)

func main() {
	repo := flag.String("repo", ".", "repo checkout providing the fixed tree")
	candidate := flag.String("candidate", "", "directory holding the candidate wrapper's files")
	stage5 := flag.Bool("stage5", false, "run Stage 5 alone, on -image, instead of Stages 1-4 on -candidate")
	image := flag.String("image", "", "with -stage5: the candidate's image, ek8s-wrapper:<12 hex>")
	id := flag.String("id", "", "with -stage5: the variant id, a decimal number")
	rate := flag.Float64("rate", 0, "with -stage5: loadgen's r; its burst rate is 4 x r")
	window := flag.Float64("window", 300, "with -stage5: loadgen's window length in seconds")
	kubeCtx := flag.String("kube-context", "kind-ek8s", "with -stage5: the kubectl context")
	cluster := flag.String("cluster", "ek8s", "with -stage5: the kind cluster name")
	stress := flag.Int("stress-pods", 1, "with -stage5: how many stress-ng pods are switched on")
	flag.Parse()
	if flag.NArg() > 0 || *stage5 == (*candidate != "") ||
		*stage5 && (*image == "" || *id == "" || *rate <= 0) {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var rep gate.Report
	var err error
	if *stage5 {
		err = gate.Stage5(ctx, gate.Stage5Config{
			KubeContext: *kubeCtx, Cluster: *cluster, ID: *id, Image: *image,
			Rate: *rate, WindowS: *window, BurstRate: 4 * *rate,
			StressPods: *stress, Core: hashcore.New(),
		}, &rep)
	} else {
		rep, err = gate.Run(ctx, gate.Config{RepoRoot: *repo, CandidateDir: *candidate})
	}
	for _, r := range rep.Results {
		if r.Pass {
			fmt.Printf("stage %d %-30s PASS\n", r.Stage, r.Check)
		} else {
			fmt.Printf("stage %d %-30s FAIL: %s\n", r.Stage, r.Check, r.Detail)
		}
	}
	if *stage5 && rep.Stage5Stall != nil {
		fmt.Println(stallLine(*rep.Stage5Stall))
	}
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "gate error:", err)
		os.Exit(2)
	case !rep.Passed():
		os.Exit(1)
	}
	if !*stage5 {
		fmt.Println("image", rep.Image)
	}
}

// stallLine is the -stage5 stall line for s. It starts "stall:", so it
// never matches a check's "stage 5 <check> PASS" line.
func stallLine(s stallrun.Stats) string {
	wall := "-"
	if !s.LongestStartWall.IsZero() {
		wall = s.LongestStartWall.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("stall: records %d oht %d stall-runs %d tail %d longest %d start-index %d start-wall %s threshold %d",
		s.Records, s.OHT, s.StallRuns, s.TailRun, s.Longest, s.LongestStartIndex, wall, s.Threshold)
}
