package orchestrator

import (
	"context"

	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

// GateRunner is Gater through package gate, for one run.
type GateRunner struct {
	// RepoRoot is the checkout gate.Run copies the fixed tree from.
	RepoRoot string
	// KubeContext and Cluster name the kind cluster (kind-ek8s, ek8s).
	KubeContext, Cluster string
	// Shape is loadgen's, as deployed; Stage 5 checks it exactly.
	Shape   Shape
	Core    *hashcore.Core
	Workers int
}

var _ Gater = GateRunner{}

func (g GateRunner) Local(ctx context.Context, dir string) (gate.Report, error) {
	return gate.Run(ctx, gate.Config{RepoRoot: g.RepoRoot, CandidateDir: dir})
}

// Stage5 runs gate.Stage5 against the one stress-ng pod Cluster.StressOn
// leaves Running.
func (g GateRunner) Stage5(ctx context.Context, id, image string, r *gate.Report) error {
	return gate.Stage5(ctx, gate.Stage5Config{
		KubeContext: g.KubeContext, Cluster: g.Cluster, ID: id, Image: image,
		Rate: g.Shape.Rate, WindowS: g.Shape.WindowS, BurstRate: g.Shape.BurstRate,
		StressPods: 1, Core: g.Core, Workers: g.Workers,
	}, r)
}
