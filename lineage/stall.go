package lineage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Kinds of loadgen run a Stall line counts.
const (
	StallBurst    = "burst"    // a Stage 5 burst
	StallBaseline = "baseline" // a window's baseline half
	StallStress   = "stress"   // a window's stress half
)

// Stall is one line of stalls.jsonl: internal/stallrun's counts for one
// (loadgen run, target), copied field by field so lineage keeps to the
// standard library. It is recording only (docs/decisions.md, 2026-11-04
// D1-D7): no verdict, score, exclusion, streak, cull, halt or prompt
// reads it. It holds ids, counts, indices and host times, never a
// record's evidence or any other candidate-written byte.
type Stall struct {
	Kind         string `json:"kind"`                    // StallBurst, StallBaseline or StallStress
	RunID        string `json:"run_id"`                  // the loadgen run's run_id
	WindowRunID  string `json:"window_run_id,omitempty"` // the window's run_id (its baseline half's); halves only
	GenerationID string `json:"generation_id"`           // the target: the variant's generation id

	Records           int       `json:"records"`
	OHT               int       `json:"oht"`
	StallRuns         int       `json:"stall_runs"`
	TailRun           int       `json:"tail_run"`
	Longest           int       `json:"longest"`
	LongestStartIndex int       `json:"longest_start_index"`
	LongestStartWall  time.Time `json:"longest_start_wall,omitzero"` // UTC; omitted with no OHT record
	Threshold         int       `json:"threshold"`

	// SpanStart and SpanEnd are a half's host time (orchestrator
	// WindowResult's Span), UTC, so a half can be matched to other logs
	// of the same host; omitted when zero, and always for a burst.
	SpanStart time.Time `json:"span_start,omitzero"`
	SpanEnd   time.Time `json:"span_end,omitzero"`
}

func (s Stall) validate() error {
	half := s.Kind == StallBaseline || s.Kind == StallStress
	switch {
	case s.Kind != StallBurst && !half:
		return fmt.Errorf("lineage: stall kind %q", s.Kind)
	case !nameRE.MatchString(s.RunID):
		return fmt.Errorf("lineage: stall run_id %q is not a run id", s.RunID)
	case half && !nameRE.MatchString(s.WindowRunID):
		return fmt.Errorf("lineage: stall window_run_id %q is not a run id", s.WindowRunID)
	case !half && s.WindowRunID != "":
		return errors.New("lineage: a burst stall line has a window_run_id")
	case !idRE.MatchString(s.GenerationID):
		return fmt.Errorf("lineage: stall generation_id %q is not a decimal number", s.GenerationID)
	case s.Records < 0 || s.OHT < 0 || s.StallRuns < 0 || s.TailRun < 0 || s.Longest < 0 || s.Threshold < 0:
		return errors.New("lineage: negative stall count")
	case s.LongestStartIndex < -1:
		return fmt.Errorf("lineage: stall longest_start_index %d, below -1", s.LongestStartIndex)
	}
	return nil
}

// AppendStall validates s and appends it to stalls.jsonl as one line,
// written in one call and synced, as AppendAttempt does attempts.jsonl.
// The file is made, mode 0600, by the first append, so a run directory
// that never counts a stall has none.
func (r *Run) AppendStall(s Stall) error {
	s.LongestStartWall, s.SpanStart, s.SpanEnd = utc(s.LongestStartWall), utc(s.SpanStart), utc(s.SpanEnd)
	if err := s.validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkTail(stallsFile, true); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, stallsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
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
	if err := f.Close(); err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	// The first append made the file: its directory entry must survive
	// a crash too.
	return syncDir(r.dir)
}

// utc is t in UTC, the zero time left zero.
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
