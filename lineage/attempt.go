package lineage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MaxRationale caps Attempt.Rationale, in bytes; the driver cuts the
// model's account to fit.
const MaxRationale = 8 << 10

// Actions an attempt ends in (DESIGN.md sec 14).
const (
	ActionPromoted  = "promoted"
	ActionDiscarded = "discarded"
	ActionArchived  = "archived"
)

// Selection modes (DESIGN.md sec 11). The first run uses tournament only.
const (
	ModeTournament = "tournament"
	ModeEscalated  = "escalated"
)

// nameTokenRE is the shape of the two free-form-looking strings a record
// carries beyond the model's rationale, a gate check name and a
// disqualification reason: lowercase words, so candidate text cannot be
// stored in them.
var nameTokenRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Gate is an attempt's gate result. It carries the failing stage and
// check name only, never a Detail: gate Details carry candidate-controlled
// output (docs/STATUS.md, Parked).
type Gate struct {
	Passed    bool   `json:"passed"`
	FailStage int    `json:"fail_stage"`           // 1 to 5; 0 when Passed
	FailCheck string `json:"fail_check,omitempty"` // a gate.Result.Check name; "" when Passed
}

// Half is one scoring half's fitness components (DESIGN.md sec 9).
type Half struct {
	SuccessRate   float64 `json:"success_rate"`
	P95LatencyMS  float64 `json:"p95_latency_ms"`
	CPUPerSuccess float64 `json:"cpu_per_success"`
	Excluded      bool    `json:"excluded"`
}

// Fitness is a scored variant's fitness: its score and each half's
// components. The first run scores a baseline half only, so Stress is nil.
type Fitness struct {
	Score    float64 `json:"score"`
	Baseline *Half   `json:"baseline,omitempty"`
	Stress   *Half   `json:"stress,omitempty"`
}

// Usage is the mutation call's usage, from the claude -p JSON result
// (DESIGN.md sec 12, sec 13).
type Usage struct {
	NumTurns            int     `json:"num_turns"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	DurationMS          int64   `json:"duration_ms"`
	CostUSD             float64 `json:"cost_usd"`
}

// Attempt is one row of DESIGN.md sec 14's table: one generation attempt,
// whatever its outcome. A later record with the same GenerationID
// supersedes an earlier one (Attempts), which is how a flag changes in an
// append-only file.
type Attempt struct {
	GenerationID  string    `json:"generation_id"`
	ParentID      string    `json:"parent_id"` // "" for generation 0; may be an archived genome
	Time          time.Time `json:"time"`
	Gate          Gate      `json:"gate"`
	GenomeSHA256  string    `json:"genome_sha256,omitempty"` // the Digest of its archived genome; required when Gate.Passed
	Fitness       *Fitness  `json:"fitness,omitempty"`       // only a gate-passing, scored variant has one
	Action        string    `json:"action"`
	SelectionMode string    `json:"selection_mode"`
	EverLive      bool      `json:"ever_live"`
	CurrentlyLive bool      `json:"currently_live"`
	Disqualified  string    `json:"disqualified,omitempty"` // an integrity reason (DESIGN.md sec 9): archived, not a parent
	Usage         Usage     `json:"usage"`
	Rationale     string    `json:"rationale"` // the model's own account of the change, cut to fit
}

func (a Attempt) validate() error {
	switch {
	case !idRE.MatchString(a.GenerationID):
		return fmt.Errorf("lineage: generation_id %q is not a decimal number", a.GenerationID)
	case a.ParentID != "" && !idRE.MatchString(a.ParentID):
		return fmt.Errorf("lineage: parent_id %q is not a decimal number", a.ParentID)
	case a.Time.IsZero():
		return errors.New("lineage: attempt has no time")
	case a.Action != ActionPromoted && a.Action != ActionDiscarded && a.Action != ActionArchived:
		return fmt.Errorf("lineage: action %q", a.Action)
	case a.SelectionMode != ModeTournament && a.SelectionMode != ModeEscalated:
		return fmt.Errorf("lineage: selection_mode %q", a.SelectionMode)
	case len(a.Rationale) > MaxRationale:
		return fmt.Errorf("lineage: rationale is %d bytes, over %d", len(a.Rationale), MaxRationale)
	case a.Disqualified != "" && !nameTokenRE.MatchString(a.Disqualified):
		return fmt.Errorf("lineage: disqualified %q is not a reason name", a.Disqualified)
	case a.Disqualified != "" && (!a.Gate.Passed || a.Action != ActionArchived || a.CurrentlyLive):
		return errors.New("lineage: a disqualified attempt must have passed the gate and be archived, not live")
	case a.CurrentlyLive && !a.EverLive:
		return errors.New("lineage: currently_live without ever_live")
	case a.Usage.NumTurns < 0 || a.Usage.InputTokens < 0 || a.Usage.OutputTokens < 0 ||
		a.Usage.CacheReadTokens < 0 || a.Usage.CacheCreationTokens < 0 || a.Usage.DurationMS < 0 ||
		!(a.Usage.CostUSD >= 0) || math.IsInf(a.Usage.CostUSD, 1):
		return errors.New("lineage: negative or non-finite usage")
	}
	g := a.Gate
	if g.Passed {
		switch {
		case g.FailStage != 0 || g.FailCheck != "":
			return errors.New("lineage: a passed gate names a failing stage")
		case !digestRE.MatchString(a.GenomeSHA256):
			return fmt.Errorf("lineage: genome_sha256 %q, want 64 lowercase hex", a.GenomeSHA256)
		}
	} else {
		switch {
		case g.FailStage < 1 || g.FailStage > 5 || !nameTokenRE.MatchString(g.FailCheck):
			return fmt.Errorf("lineage: failed gate with stage %d, check %q", g.FailStage, g.FailCheck)
		case a.Action != ActionDiscarded:
			return fmt.Errorf("lineage: a gate-failed attempt is %s, want %s", a.Action, ActionDiscarded)
		case a.Fitness != nil || a.EverLive || a.CurrentlyLive:
			return errors.New("lineage: a gate-failed attempt has a fitness or a live flag")
		}
	}
	if a.GenomeSHA256 != "" && !digestRE.MatchString(a.GenomeSHA256) {
		return fmt.Errorf("lineage: genome_sha256 %q, want 64 lowercase hex", a.GenomeSHA256)
	}
	if a.Fitness != nil {
		if _, err := json.Marshal(a.Fitness); err != nil { // a NaN or infinity
			return fmt.Errorf("lineage: fitness: %w", err)
		}
	}
	return nil
}

// AppendAttempt validates a and appends it to attempts.jsonl as one line,
// written in one call and synced. The append is the record: call it
// whatever the attempt's outcome (DESIGN.md sec 11, step 8).
func (r *Run) AppendAttempt(a Attempt) error {
	a.Time = a.Time.UTC()
	if err := a.validate(); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A failed earlier append (a full disk) may have left a torn line,
	// which this one would corrupt too.
	if err := r.checkTail(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, attemptsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("lineage: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("lineage: %w", err)
	}
	return f.Close()
}

// Attempts reads every record and returns one Attempt per generation_id,
// the last record written for it, in the order the ids first appeared. A
// line that is not exactly one valid Attempt (an unknown field included)
// is an error naming its line number.
func (r *Run) Attempts() ([]Attempt, error) {
	r.mu.Lock()
	b, err := os.ReadFile(filepath.Join(r.dir, attemptsFile))
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("lineage: %w", err)
	}
	var out []Attempt
	at := make(map[string]int) // generation_id to its index in out
	for n, line := range bytes.SplitAfter(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if line[len(line)-1] != '\n' {
			return nil, fmt.Errorf("lineage: %s line %d: torn (no newline)", attemptsFile, n+1)
		}
		var a Attempt
		if err := strictDecode(line, &a); err != nil {
			return nil, fmt.Errorf("lineage: %s line %d: %w", attemptsFile, n+1, err)
		}
		if err := a.validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", attemptsFile, n+1, err)
		}
		if i, ok := at[a.GenerationID]; ok {
			out[i] = a
		} else {
			at[a.GenerationID] = len(out)
			out = append(out, a)
		}
	}
	return out, nil
}

// checkTail errors if attempts.jsonl is not empty and does not end in a
// newline.
func (r *Run) checkTail() error {
	f, err := os.Open(filepath.Join(r.dir, attemptsFile))
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	if st.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	if last[0] != '\n' {
		return fmt.Errorf("lineage: %s ends in a torn line; fix it by hand", attemptsFile)
	}
	return nil
}

// strictDecode decodes b, one JSON value and white space, into v, refusing
// a field v lacks.
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}
