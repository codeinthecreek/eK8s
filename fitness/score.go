package fitness

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// kindNegative is loadgen's record kind for a contract negative
// (loadgen/request.go): outside all three metrics (DESIGN.md sec 9).
const kindNegative = "negative"

// integrityReasons are the failure reasons that are evidence the wrapper
// fakes results, not one failed request (DESIGN.md sec 9). A record
// carrying one is counted in Stats.Integrity; the orchestrator confirms
// it on the host and culls the variant before scoring.
var integrityReasons = map[string]bool{
	string(classifier.ReasonVerifyMismatch):   true,
	string(classifier.ReasonVerifyError):      true,
	string(classifier.ReasonWrongMatch):       true,
	string(classifier.ReasonNonCanonicalPHC):  true,
	string(classifier.ReasonSaltReused):       true,
	string(classifier.ReasonNegativeAccepted): true,
	string(classifier.ReasonMalformedBody):    true,
}

// Stats are one half's measurements.
type Stats struct {
	Requests  int // records scored: every record that is not a negative
	Successes int
	Sheds     int // graceful_shed
	Failures  int
	Negatives int // records left out of every metric

	// SuccessRate is (Successes + ShedCredit x Sheds) / Requests.
	SuccessRate float64
	// P95Latency is the 95th percentile (nearest rank) of the successes'
	// latency; with no success it reads as the ceiling.
	P95Latency time.Duration
	// CPUPerSuccess is the half's pod CPU-seconds over Successes; with no
	// success it reads as the ceiling.
	CPUPerSuccess float64

	// Integrity counts the integrity reasons among the failed records,
	// by reason name. A variant with any is a cull candidate, whatever
	// its score.
	Integrity map[string]int
}

// Result is a scored half: its Stats, each component's normalized score
// in 0 to 1, and their weighted sum.
type Result struct {
	Stats
	SuccessScore, LatencyScore, CPUScore float64
	Score                                float64
}

// Score scores one half from its records and the pod CPU-seconds spent
// in it (CPUSeconds). recs must be the half's records of one variant,
// every one a send loadgen could account for: a Skipped or Fault record,
// an unknown bucket, a negative latency, or no scorable record at all is
// an error, never a score (the half is excluded upstream, DESIGN.md sec
// 10).
func Score(p Params, recs []wire.Record, cpuSeconds float64) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	if math.IsNaN(cpuSeconds) || math.IsInf(cpuSeconds, 0) || cpuSeconds < 0 {
		return Result{}, fmt.Errorf("fitness: cpu seconds %v", cpuSeconds)
	}
	var st Stats
	var lat []time.Duration
	for i, r := range recs {
		switch {
		case r.Skipped:
			return Result{}, fmt.Errorf("fitness: record %d is skipped", i)
		case r.Fault != "":
			return Result{}, fmt.Errorf("fitness: record %d has a loadgen fault", i)
		case r.LatencyNS < 0:
			return Result{}, fmt.Errorf("fitness: record %d has a negative latency", i)
		}
		if r.Kind == kindNegative {
			st.Negatives++
			continue
		}
		st.Requests++
		switch r.Bucket {
		case classifier.Success.String():
			st.Successes++
			lat = append(lat, time.Duration(r.LatencyNS))
		case classifier.GracefulShed.String():
			st.Sheds++
		case classifier.Failed.String():
			st.Failures++
			if integrityReasons[r.Reason] {
				if st.Integrity == nil {
					st.Integrity = make(map[string]int)
				}
				st.Integrity[r.Reason]++
			}
		default:
			return Result{}, fmt.Errorf("fitness: record %d has an unknown bucket", i)
		}
	}
	if st.Requests == 0 {
		return Result{}, errors.New("fitness: no scorable record")
	}
	st.SuccessRate = (float64(st.Successes) + p.ShedCredit*float64(st.Sheds)) / float64(st.Requests)
	st.P95Latency, st.CPUPerSuccess = p.LatencyCeiling, p.CPUCeiling
	if st.Successes > 0 {
		slices.Sort(lat)
		rank := int(math.Ceil(0.95 * float64(len(lat))))
		st.P95Latency = lat[rank-1]
		st.CPUPerSuccess = cpuSeconds / float64(st.Successes)
	}

	res := Result{Stats: st}
	res.SuccessScore = clamp01(st.SuccessRate)
	res.LatencyScore = clamp01(float64(p.LatencyCeiling-st.P95Latency) / float64(p.LatencyCeiling-p.LatencyFloor))
	res.CPUScore = clamp01((p.CPUCeiling - st.CPUPerSuccess) / (p.CPUCeiling - p.CPUFloor))
	res.Score = p.WeightSuccess*res.SuccessScore + p.WeightLatency*res.LatencyScore + p.WeightCPU*res.CPUScore
	return res, nil
}

// Combine is a variant's fitness: its baseline score, or the lower of the
// two when a stress half was scored too (DESIGN.md sec 9: bounded by its
// worst regime, never an average).
func Combine(baseline float64, stress *float64) float64 {
	if stress == nil {
		return baseline
	}
	return math.Min(baseline, *stress)
}

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }
