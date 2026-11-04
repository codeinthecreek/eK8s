package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/codeinthecreek/eK8s/deploy/variant"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/stallrun"
	"github.com/codeinthecreek/eK8s/loadgen/client"
)

// Stage5Config says what Stage 5 gates and where.
type Stage5Config struct {
	// KubeContext and Cluster name the kind cluster: its kubectl context
	// (kind-ek8s) and its kind name (ek8s).
	KubeContext, Cluster string

	// ID is the variant id (a decimal number); the candidate gets the
	// namespace ek8s-v<ID>, which must not exist.
	ID string
	// Image is the candidate's image, Report.Image from Run.
	Image string

	// Rate, WindowS and BurstRate are the numbers loadgen was deployed
	// with (r, W in seconds, b = 4r); Stage 5 refuses a loadgen whose
	// GET /config differs, exactly.
	Rate, WindowS, BurstRate float64

	// StressPods is how many stress-ng pods the orchestrator has
	// switched on (1): Stage 5 only checks they are Running.
	StressPods int

	// Core makes and checks the host's pairs; Workers is how many
	// goroutines do (GOMAXPROCS when <= 0).
	Core    *hashcore.Core
	Workers int
}

func (c Stage5Config) validate() error {
	switch {
	case c.KubeContext == "" || c.Cluster == "":
		return errors.New("stage 5: kube context and cluster are required")
	case c.StressPods < 1:
		return fmt.Errorf("stage 5: %d stress pods, want at least 1", c.StressPods)
	case c.Core == nil:
		return errors.New("stage 5: no hashcore")
	}
	return nil
}

// Names Stage 5 reads from deploy/stress and deploy/variant.
const (
	stressNS       = "ek8s-stress"
	stressSelector = "app=stress-ng"
	hasherSelector = "app=hasher"
)

// stepTimeout bounds each cluster step of Stage 5 that has no bound of
// its own (kubectl's --request-timeout bounds each API call, not a
// step): a hung step is a gate fault, never a hang (inferred; kind load
// of a small static image takes seconds).
var stepTimeout = 2 * time.Minute

// controlTimeout is the control API client's per-request timeout.
const controlTimeout = 30 * time.Second

// openControl opens the transport to loadgen's control API and returns
// its client and a close func; tests replace it.
var openControl = func(ctx context.Context, kubeCtx string) (*client.Client, func(), error) {
	p, err := StartControlProxy(ctx, kubeCtx, controlTimeout)
	if err != nil {
		return nil, nil, err
	}
	return p.Client(), func() { p.Close() }, nil
}

// Stage5 runs Stage 5 (DESIGN.md sec 7) on a candidate that passed Stages
// 1-4: it checks the node and the stress-ng pods, loads and deploys the
// candidate into its own namespace, waits for it to be Ready, runs the
// burst (runBurst), then judges pod health ("pod-health") and the burst's
// responses ("responses", "hash-verify") on the host. A check that fails
// is a Result in r and ends the stage; the error is only for a fault of
// the gate or the cluster, which says nothing about the candidate: a
// NotReady node, stress-ng not Running, a cluster step that failed or
// hung, loadgen misbehaving. The caller halts the run on an error
// (docs/decisions.md, the PR #52 S10 answer).
//
// Stage5 leaves what it made: on a pass the namespace, its pod and the
// loadgen target are the variant's from now on (DESIGN.md sec 11), and
// r.BurstHashes holds the burst's recorded hashes. Once the burst's
// records are read, r.Stage5Stall holds their stall counts and
// r.Stage5RunID the burst's run_id, on a pass, a failure or a later
// fault; they never change a Result. On a
// candidate failure it has already DELETEd the loadgen target, after
// reading the records; deleting the namespace is the caller's.
func Stage5(ctx context.Context, cfg Stage5Config, r *Report) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	label, err := newVariantLabel()
	if err != nil {
		return err
	}
	vp := variant.Params{ID: cfg.ID, Label: label, Image: cfg.Image}
	manifest, err := variant.Render(vp)
	if err != nil {
		return fmt.Errorf("stage 5: %w", err)
	}
	ns := vp.Namespace()

	if err := cfg.step(ctx, "nodes", func(c context.Context) error { return nodesReady(c, cfg.KubeContext) }); err != nil {
		return err
	}
	if err := cfg.step(ctx, "stress-ng", func(c context.Context) error {
		return podsRunning(c, cfg.KubeContext, stressNS, stressSelector, cfg.StressPods)
	}); err != nil {
		return err
	}
	cl, closeControl, err := openControl(ctx, cfg.KubeContext)
	if err != nil {
		return fmt.Errorf("stage 5: control API: %w", err)
	}
	defer closeControl()

	if err := cfg.step(ctx, "kind load", func(c context.Context) error { return kindLoad(c, cfg.Cluster, cfg.Image) }); err != nil {
		return err
	}
	// create, not apply: a namespace that exists is a gate fault.
	if err := cfg.step(ctx, "create", func(c context.Context) error {
		_, err := kubectl(c, cfg.KubeContext, bytes.NewReader(manifest), "create", "-f", "-")
		return err
	}); err != nil {
		return err
	}

	problem, fault := waitReady(ctx, cfg.KubeContext, ns, hasherSelector, readyTimeout)
	if fault != nil {
		return fmt.Errorf("stage 5: deploy-ready: %w", fault)
	}
	if problem != "" {
		if err := cfg.nodeCheck(ctx, "deploy-ready"); err != nil {
			return err
		}
		r.add(5, "deploy-ready", problem)
		return nil
	}

	runID := "gate-" + cfg.ID + "-" + label
	res, err := runBurst(ctx, cl, burstParams{
		Target: cfg.ID, RunID: runID,
		Want: loadgenShape{Rate: cfg.Rate, WindowS: cfg.WindowS, BurstRate: cfg.BurstRate},
		Core: cfg.Core, Workers: cfg.Workers,
	})
	if err != nil {
		return err
	}
	stall := stallrun.Detect(res.Records, cfg.BurstRate)
	r.Stage5Stall, r.Stage5RunID = &stall, runID

	var health string
	if err := cfg.step(ctx, "pod-health", func(c context.Context) (err error) {
		health, err = podHealthCheck(c, cfg.KubeContext, ns, hasherSelector)
		return err
	}); err != nil {
		return err
	}
	if health != "" {
		if err := cfg.nodeCheck(ctx, "pod-health"); err != nil {
			return err
		}
		r.add(5, "pod-health", health)
		return dropTarget(ctx, cl, cfg.ID, res.Through)
	}
	r.add(5, "pod-health", "")

	ok, err := burstVerdicts(ctx, r, cfg.Core, res.Records, cfg.Workers)
	if err != nil {
		return fmt.Errorf("stage 5: %w", err)
	}
	if !ok {
		return dropTarget(ctx, cl, cfg.ID, res.Through)
	}
	for _, rec := range res.Records {
		if rec.Pair != nil {
			r.BurstHashes = append(r.BurstHashes, rec.Pair.Hash)
		}
	}
	return nil
}

// step runs f under stepTimeout; its error is a gate fault named name.
func (c Stage5Config) step(ctx context.Context, name string, f func(ctx context.Context) error) error {
	sctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := f(sctx); err != nil {
		return fmt.Errorf("stage 5: %s: %w", name, err)
	}
	return nil
}

// nodeCheck is the node Ready check made before a pod problem becomes a
// candidate failure: a problem on a NotReady node is a fault.
func (c Stage5Config) nodeCheck(ctx context.Context, name string) error {
	return c.step(ctx, name+": node check", func(sctx context.Context) error { return nodesReady(sctx, c.KubeContext) })
}

// dropTarget DELETEs a failed candidate's loadgen target once its
// records, through Seq through, are read (DESIGN.md sec 11). A refusal
// is a gate fault: the target would stay in loadgen's memory.
func dropTarget(ctx context.Context, cl *client.Client, id string, through uint64) error {
	if _, err := cl.DeleteTarget(ctx, id, through); err != nil {
		return fmt.Errorf("stage 5: deleting the target: %w", err)
	}
	return nil
}

// newVariantLabel returns ek8s-<16 hex>, random per deployment (DESIGN.md
// sec 5).
func newVariantLabel() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("stage 5: %w", err)
	}
	return "ek8s-" + hex.EncodeToString(b), nil
}
