// Command orchestrator runs one evolutionary run (DESIGN.md sec 11,
// orchestrator.Loop) on the kind cluster: generation 0, then up to
// -generations mutation attempts, each gated and, on a pass, observed
// with the live variants in probation windows until one is scored. It writes the run
// to a new lineage directory, -run.
//
// Before a run, from the repo root: `go mod download` (the gate's go
// commands run with GOPROXY=off); the cluster up with loadgen deployed
// at r (deploy/up.sh, then deploy/loadgen-up.sh <r>); a pinned claude
// binary; a dedicated token from `claude setup-token` in
// CLAUDE_CODE_OAUTH_TOKEN, which the orchestrator reads and then removes
// from its own environment, so no child process inherits it. Then:
//
//	go run ./orchestrator/cmd/orchestrator -run DIR -rate R \
//	    -claude PATH -claude-version VERSION
//
// A run starts on a clean cluster: no namespace labelled
// ek8s.io/role=variant and no loadgen target, or it refuses to start. A
// halted run leaves its variants deployed for inspection; to clean up,
// `kubectl --context kind-ek8s delete namespace -l ek8s.io/role=variant`
// and `kubectl --context kind-ek8s -n ek8s-system rollout restart
// deploy/loadgen` (a new loadgen holds no target).
//
// Exit status: 0 when the generations or the usage ceiling ran out, 1
// when the run halted (excluded windows, a loadgen restart, no parent),
// 2 on a fault or an interrupt, or when it could not start.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/codeinthecreek/eK8s/driver"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/lineage"
	"github.com/codeinthecreek/eK8s/orchestrator"
)

// tokenEnv carries the mutation sessions' token (DESIGN.md sec 12); it is
// never a flag, since a command line is visible to every local user.
const tokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// controlTimeout is the control API client's per-request timeout, as
// gate Stage 5's.
const controlTimeout = 30 * time.Second

func main() {
	runDir := flag.String("run", "", "the run directory to create; it must not exist")
	repo := flag.String("repo", ".", "repo checkout: the fixed tree for the gate and generation 0's wrapper")
	rate := flag.Float64("rate", 0, "loadgen's r, as deployed; its burst rate is 4 x r")
	window := flag.Float64("window", 300, "loadgen's window length in seconds, as deployed")
	kubeCtx := flag.String("kube-context", "kind-ek8s", "the kubectl context")
	cluster := flag.String("cluster", "ek8s", "the kind cluster name")
	gens := flag.Int("generations", 10, "mutation attempts to make, at most")
	turns := flag.Int("turn-ceiling", 300, "stop once the mutation calls' num_turns sum to this")
	tokens := flag.Int64("token-ceiling", 0, "stop once the mutation calls' tokens, of every kind, sum to this (0: no token ceiling)")
	claude := flag.String("claude", "", "the claude binary (a symlink is resolved once)")
	version := flag.String("claude-version", "", "the version the claude binary must report, such as 2.1.310")
	scratch := flag.String("scratch", "", "an existing directory for mutation and gate scratch files (default: a new temporary one)")
	workers := flag.Int("workers", 0, "host Argon2 goroutines (0: GOMAXPROCS)")
	seed := flag.Uint64("seed", 0, "tournament seed (0: a random one, logged)")
	flag.Parse()
	if flag.NArg() > 0 || *runDir == "" || *rate <= 0 || *window <= 0 || *claude == "" || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	token := os.Getenv(tokenEnv)
	os.Unsetenv(tokenEnv)
	if token == "" {
		fail(errors.New(tokenEnv + " is not set"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reason, err := run(ctx, config{
		runDir: *runDir, repo: *repo, rate: *rate, window: *window, kubeCtx: *kubeCtx, cluster: *cluster,
		gens: *gens, turns: *turns, tokens: *tokens, claude: *claude, version: *version, token: token,
		scratch: *scratch, workers: *workers, seed: *seed,
	})
	switch {
	case err != nil:
		fail(err)
	case reason == orchestrator.StopGenerations || reason == orchestrator.StopUsageCeiling:
		fmt.Println("run stopped:", reason)
	default:
		fmt.Println("run halted:", reason)
		os.Exit(1)
	}
}

type config struct {
	runDir, repo, kubeCtx, cluster, claude, version, token, scratch string
	rate, window                                                    float64
	gens, turns, workers                                            int
	tokens                                                          int64
	seed                                                            uint64
}

func run(ctx context.Context, c config) (string, error) {
	repo, err := filepath.Abs(c.repo)
	if err != nil {
		return "", err
	}
	gen0, err := readWrapper(filepath.Join(repo, "internal", "wrapper"))
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(c.runDir); err == nil {
		return "", fmt.Errorf("run directory %s exists; a run starts in a new one", c.runDir)
	}
	scratch := c.scratch
	if scratch == "" {
		if scratch, err = os.MkdirTemp("", "ek8s-orchestrator-"); err != nil {
			return "", err
		}
		defer os.RemoveAll(scratch)
	}
	if scratch, err = filepath.Abs(scratch); err != nil {
		return "", err
	}

	kube := &orchestrator.Kube{Context: c.kubeCtx}
	left, err := kube.VariantNamespaces(ctx)
	if err != nil {
		return "", err
	}
	if len(left) > 0 {
		return "", fmt.Errorf("variant namespaces from an earlier run are left: %s (see the command's doc for the clean-up)", strings.Join(left, " "))
	}
	proxy, err := gate.StartControlProxy(ctx, c.kubeCtx, controlTimeout)
	if err != nil {
		return "", fmt.Errorf("control API: %w", err)
	}
	defer proxy.Close()
	shape := orchestrator.Shape{Rate: c.rate, WindowS: c.window, BurstRate: 4 * c.rate}
	core := hashcore.New()
	params := fitness.DefaultParams()
	session, err := orchestrator.NewSession(ctx, orchestrator.SessionConfig{
		Client: proxy.Client(), Cluster: kube, Core: core, Want: shape, Params: params, Workers: c.workers,
	})
	if err != nil {
		return "", err
	}
	ts, err := proxy.Client().Targets(ctx)
	if err != nil {
		return "", fmt.Errorf("loadgen targets: %w", err)
	}
	if len(ts.Targets) > 0 {
		return "", fmt.Errorf("loadgen holds %d targets from an earlier run (see the command's doc for the clean-up)", len(ts.Targets))
	}
	drv, err := driver.New(ctx, driver.Config{Claude: c.claude, Version: c.version, Token: c.token, ScratchDir: scratch})
	if err != nil {
		return "", err
	}

	seed := c.seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	runRec, err := lineage.Create(c.runDir)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "run %s: r %v, window %v s, %d generations, turn ceiling %d, token ceiling %d, seed %d, claude %s\n",
		c.runDir, c.rate, c.window, c.gens, c.turns, c.tokens, seed, drv.Claude())
	loop, err := orchestrator.NewLoop(orchestrator.LoopConfig{
		Run: runRec, Windows: session, Cluster: kube,
		Gate: orchestrator.GateRunner{
			RepoRoot: repo, KubeContext: c.kubeCtx, Cluster: c.cluster, Shape: shape, Core: core, Workers: c.workers,
		},
		Mutator: drv, Params: params, Gen0: gen0, Generations: c.gens,
		TurnCeiling: c.turns, TokenCeiling: c.tokens, ScratchDir: scratch,
		Rand: rand.New(rand.NewPCG(seed, seed)), Log: os.Stderr,
	})
	if err != nil {
		return "", err
	}
	return loop.Run(ctx)
}

// readWrapper reads generation 0: the wrapper's non-test .go files.
func readWrapper(dir string) (map[string][]byte, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte)
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	if _, ok := files["hasher.go"]; !ok {
		return nil, fmt.Errorf("%s has no hasher.go: is -repo a checkout of this repo?", dir)
	}
	return files, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "orchestrator:", err)
	os.Exit(2)
}
