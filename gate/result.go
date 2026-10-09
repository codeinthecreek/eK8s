package gate

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
