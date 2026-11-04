package gate

import "github.com/codeinthecreek/eK8s/internal/stallrun"

// Result is the outcome of one gate check. Checks within a stage are
// logged distinctly (DESIGN.md sec 7), so each gets its own Result.
type Result struct {
	Stage  int    // 1-5, DESIGN.md sec 7
	Check  string // e.g. "candidate-files", "imports"
	Pass   bool
	Detail string // why the check failed; empty on pass
}

// Report is every check the gate ran, in order. The gate stops at the
// first failure (zero tolerance), so a failing Report ends with it.
type Report struct {
	Results []Result

	// Image is the candidate's docker image tag, set only when the
	// candidate passed; Stage 5 loads it into kind. On a failure the gate
	// removes the image, unless it existed before the run.
	Image string

	// BurstHashes are the hashes of the Stage 5 burst's recorded /hash
	// pairs, in record order, set only when Stage 5 passed: the
	// orchestrator seeds the variant's host salt history with them, so a
	// window hash that replays a burst salt is confirmed on the host
	// (orchestrator.Session.Adopt). They are the candidate's output.
	BurstHashes []string

	// Stage5Stall is what internal/stallrun finds in the Stage 5 burst's
	// records at the burst rate b, set as soon as the burst's records are
	// read, before pod-health, so a burst that failed pod-health or
	// "responses" carries it too; nil when no burst was read. It is
	// detection and recording only (docs/decisions.md, 2026-11-02 Q4):
	// never a Result, never in a Detail, and Passed ignores it.
	Stage5Stall *stallrun.Stats
	// Stage5RunID is the Stage 5 burst's loadgen run_id, set with
	// Stage5Stall, so a recorded count names its run.
	Stage5RunID string
}

// Passed reports whether every check ran and passed.
func (r Report) Passed() bool {
	if len(r.Results) == 0 {
		return false
	}
	for _, res := range r.Results {
		if !res.Pass {
			return false
		}
	}
	return true
}

// add records a check's outcome and reports whether it passed.
func (r *Report) add(stage int, check, detail string) bool {
	r.Results = append(r.Results, Result{Stage: stage, Check: check, Pass: detail == "", Detail: detail})
	return detail == ""
}
