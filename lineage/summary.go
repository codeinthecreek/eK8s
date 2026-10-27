package lineage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const summaryFile = "summary.json"

// Summary is the run-level record written once when the run stops
// (DESIGN.md sec 13): why it stopped, how many mutation attempts it
// made, and the usage of every mutation call, those of attempts with no
// record included.
type Summary struct {
	Reason      string    `json:"reason"` // a lowercase token, such as usage_ceiling
	Generations int       `json:"generations"`
	Usage       Usage     `json:"usage"`
	Time        time.Time `json:"time"`
}

func (s Summary) validate() error {
	u := s.Usage
	switch {
	case !nameTokenRE.MatchString(s.Reason):
		return fmt.Errorf("lineage: summary reason %q is not a token", s.Reason)
	case s.Generations < 0:
		return fmt.Errorf("lineage: summary of %d generations", s.Generations)
	case s.Time.IsZero():
		return fmt.Errorf("lineage: summary has no time")
	case u.NumTurns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 ||
		u.CacheCreationTokens < 0 || u.DurationMS < 0 || !(u.CostUSD >= 0):
		return fmt.Errorf("lineage: summary has negative usage")
	}
	if _, err := json.Marshal(s); err != nil { // an infinite cost
		return fmt.Errorf("lineage: summary: %w", err)
	}
	return nil
}

// WriteSummary writes summary.json, once: a second write is ErrExists.
func (r *Run) WriteSummary(s Summary) error {
	s.Time = s.Time.UTC()
	if err := s.validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	if err := writeNew(filepath.Join(r.dir, summaryFile), append(b, '\n')); err != nil {
		return err
	}
	return syncDir(r.dir)
}

// ReadSummary reads summary.json; a run still going has none
// (fs.ErrNotExist, wrapped).
func (r *Run) ReadSummary() (Summary, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, summaryFile))
	if err != nil {
		return Summary{}, fmt.Errorf("lineage: %w", err)
	}
	var s Summary
	if err := strictDecode(b, &s); err != nil {
		return Summary{}, fmt.Errorf("lineage: %s: %w", summaryFile, err)
	}
	if err := s.validate(); err != nil {
		return Summary{}, err
	}
	return s, nil
}
