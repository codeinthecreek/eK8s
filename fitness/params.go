package fitness

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// Params are the fixed absolute bounds and weights a run scores with
// (DESIGN.md sec 9). They are starting guesses, set once real telemetry
// spread is visible; changing them changes the run's comparability
// epoch.
type Params struct {
	// ShedCredit is the partial credit a graceful_shed response gets in
	// success_rate, against 1 for a success and 0 for a failure.
	ShedCredit float64

	// LatencyFloor and LatencyCeiling bound p95 latency: at or under the
	// floor scores 1, at or over the ceiling 0, linear between. The
	// ceiling is the hang timeout, the constant section 8 classifies
	// against.
	LatencyFloor, LatencyCeiling time.Duration

	// CPUFloor and CPUCeiling bound CPU-seconds per success the same way.
	CPUFloor, CPUCeiling float64

	// The weights of the three normalized components; they sum to 1.
	WeightSuccess, WeightLatency, WeightCPU float64
}

// DefaultParams are the starting values: shed credit 0.5; a latency
// floor of 25 ms, near the single-hash time at the Stage 3 cost floor,
// and the hang timeout as the ceiling; 0.025 to 0.5 CPU-seconds per
// success; weights 0.4, 0.3 and 0.3, a slight lean toward success_rate.
func DefaultParams() Params {
	return Params{
		ShedCredit:   0.5,
		LatencyFloor: 25 * time.Millisecond, LatencyCeiling: classifier.HangTimeout,
		CPUFloor: 0.025, CPUCeiling: 0.5,
		WeightSuccess: 0.4, WeightLatency: 0.3, WeightCPU: 0.3,
	}
}

// Validate reports the first thing wrong with p.
func (p Params) Validate() error {
	finite := func(xs ...float64) bool {
		for _, x := range xs {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return false
			}
		}
		return true
	}
	switch {
	case !finite(p.ShedCredit, p.CPUFloor, p.CPUCeiling, p.WeightSuccess, p.WeightLatency, p.WeightCPU):
		return errors.New("fitness: a non-finite parameter")
	case p.ShedCredit < 0 || p.ShedCredit > 1:
		return fmt.Errorf("fitness: shed credit %v, want 0 to 1", p.ShedCredit)
	case p.LatencyFloor < 0 || p.LatencyCeiling <= p.LatencyFloor:
		return fmt.Errorf("fitness: latency bounds %v to %v, want 0 <= floor < ceiling", p.LatencyFloor, p.LatencyCeiling)
	case p.CPUFloor < 0 || p.CPUCeiling <= p.CPUFloor:
		return fmt.Errorf("fitness: cpu bounds %v to %v, want 0 <= floor < ceiling", p.CPUFloor, p.CPUCeiling)
	case p.WeightSuccess < 0 || p.WeightLatency < 0 || p.WeightCPU < 0 ||
		math.Abs(p.WeightSuccess+p.WeightLatency+p.WeightCPU-1) > 1e-9:
		return fmt.Errorf("fitness: weights %v, %v, %v, want non-negative and summing to 1", p.WeightSuccess, p.WeightLatency, p.WeightCPU)
	}
	return nil
}
