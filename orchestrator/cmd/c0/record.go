package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hostclock"
)

// Record is the JSON result file. It is rewritten after every step, so a
// halt keeps what was measured.
type Record struct {
	Head    string `json:"repo_head"`
	Dirty   bool   `json:"repo_dirty"`
	Image   string `json:"image"`
	ID      string `json:"id"`
	Label   string `json:"variant_label"`
	NProc   int    `json:"nproc"`
	NodeCPU string `json:"node_allocatable_cpu"`
	Ladder  []int  `json:"ladder"`
	Confirm int    `json:"confirm"`

	// HostClock is the cpufreq state read at start; AllowUnpinned is set
	// when -allow-unpinned let the run measure on unpinned clocks.
	HostClock     hostclock.State `json:"host_clock"`
	AllowUnpinned bool            `json:"allow_unpinned"`

	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`

	Steps []StepRecord `json:"steps"`

	// Result is set when the search ended; Halt says why it did not
	// resolve, or the fault that stopped it.
	Result *Result `json:"result,omitempty"`
	Halt   string  `json:"halt,omitempty"`
}

// Result is C0 and the r derived from it.
type Result struct {
	// C0 is "13", ">= 19" or "unresolved".
	C0 string `json:"c0"`
	// R is 0.5 x C0 (for ">= N", 0.5 x N), in strconv 'g' -1 form; empty
	// when unresolved.
	R string `json:"r,omitempty"`
	// Pass and Fail are the last passing rate and the first failing one
	// above it, where known.
	Pass int `json:"last_pass,omitempty"`
	Fail int `json:"first_fail,omitempty"`
}

// StepRecord is one window at one rate (one attempt).
type StepRecord struct {
	Rate    int       `json:"r"`
	Attempt int       `json:"attempt"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`

	Valid  bool   `json:"valid"`
	WhyNot string `json:"invalid_because,omitempty"`
	Pass   bool   `json:"pass"`
	// PassWhyNot says which half of the rule failed; empty on a pass or
	// on an invalid step.
	PassWhyNot string `json:"fail_because,omitempty"`

	Requests  int `json:"requests"`
	Successes int `json:"successes"`
	Sheds     int `json:"sheds"`
	Failures  int `json:"failures"`
	Negatives int `json:"negatives"`
	// P95LatencyMS is the successes' p95; with no success the scoring
	// ceiling (fitness.Stats).
	P95LatencyMS float64 `json:"p95_latency_ms"`
	// CPUSeconds is the pod CPU-seconds in the window, derived from the
	// score (CPUPerSuccess x Successes); zero with no success or when
	// CPUUnmeasured is set.
	CPUSeconds    float64 `json:"cpu_seconds"`
	CPUPerSuccess float64 `json:"cpu_per_success"`
	CPUUnmeasured string  `json:"cpu_unmeasured,omitempty"`

	// Excluded lists why the window was not scored (orchestrator.Excluded*).
	Excluded []string `json:"excluded,omitempty"`

	BootID        string `json:"loadgen_boot_id,omitempty"`
	Pod           string `json:"pod,omitempty"`
	RestartsStart int    `json:"restarts_start"`
	RestartsEnd   int    `json:"restarts_end"`

	// Error is the fault or disqualification that halted the search.
	Error string `json:"error,omitempty"`
}

// save writes the record to path atomically: a temporary file in the
// same directory, then a rename.
func (r *Record) save(path string) error {
	// An encoder, not json.MarshalIndent, so < > & in a halt text stay
	// readable instead of becoming \u003c.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".c0-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
