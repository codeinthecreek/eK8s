// Package stallrun counts stall runs in one loadgen run's records for
// one target: option 4's detection and recording, which never changes a
// verdict (docs/decisions.md, 2026-11-02 Q4 and 2026-11-04 D1-D7). The
// gate's Stage 5 path and the orchestrator both use it, so a burst and a
// window half are counted by the same rules.
//
// It imports the standard library, loadgen/wire and internal/classifier
// (constants only). internal/wrapper cannot import it: Stage 1's import
// allowlist (gate/precheck.go) does not list it.
package stallrun

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Stats is what Detect finds in one (loadgen run, target)'s records
// (D7). It holds counts, indices and a time only, never a record's
// Evidence, Pair or any other candidate-written byte.
type Stats struct {
	Records           int       // records read
	OHT               int       // OHT records (D1)
	StallRuns         int       // stall runs (D4)
	TailRun           int       // the length of the OHT run that reaches the last index, else 0 (D5)
	Longest           int       // the longest OHT run's length, tail included (D6)
	LongestStartIndex int       // its first record's Index; -1 with no OHT record
	LongestStartWall  time.Time // its first record's IntendedWall, UTC; zero with no OHT record
	Threshold         int       // the stall-run length threshold used (D4)
}

// Threshold is the shortest OHT run that can be a stall run at a
// loadgen run's rate, in sends per second: ceil(rate x HangTimeout), the
// sends made in one hang timeout (D4). It is at least 1, so a rate that
// is not positive and finite cannot make every OHT record a stall.
func Threshold(rate float64) int {
	t := math.Ceil(rate * classifier.HangTimeout.Seconds())
	switch {
	case !(t >= 1): // also NaN
		return 1
	case t >= math.MaxInt32:
		return math.MaxInt32
	}
	return int(t)
}

// oht reports whether rec is an OHT record (D1): failed, over the hang
// timeout, and neither Skipped nor a Fault.
func oht(rec *wire.Record) bool {
	return !rec.Skipped && rec.Fault == "" &&
		rec.Bucket == classifier.Failed.String() &&
		rec.Reason == string(classifier.ReasonOverHangTimeout)
}

// Detect counts recs, one loadgen run's records for one target, by
// D1-D7 at that run's rate (b for a Stage 5 burst, r for a window half).
// The records may come in any order (a window's arrive in Seq order):
// Detect orders a copy by Index (D2) and leaves recs as it is. An OHT run
// is a maximal run of OHT records at consecutive indices (D3), so a
// missing index or a repeated one ends it. A run of at least Threshold
// records is a stall run when a record at a later index is not OHT (D4);
// a run that reaches the last index is the tail run instead (D5). Ties
// for the longest go to the earliest (D6).
func Detect(recs []wire.Record, rate float64) Stats {
	s := Stats{Records: len(recs), LongestStartIndex: -1, Threshold: Threshold(rate)}
	if len(recs) == 0 {
		return s
	}
	sorted := make([]*wire.Record, len(recs))
	for i := range recs {
		sorted[i] = &recs[i]
	}
	slices.SortStableFunc(sorted, func(a, b *wire.Record) int { return cmp.Compare(a.Index, b.Index) })

	lastIndex := sorted[len(sorted)-1].Index
	// lastNonOHT is the highest Index of a record that is not OHT: a run
	// ending below it is followed by recovery.
	lastNonOHT, anyNonOHT := 0, false
	for _, rec := range sorted {
		if !oht(rec) {
			lastNonOHT, anyNonOHT = rec.Index, true
		}
	}

	var start, prev *wire.Record // the open run's first and last record
	n := 0                       // the open run's length
	closeRun := func() {
		if n == 0 {
			return
		}
		if n > s.Longest {
			s.Longest = n
			s.LongestStartIndex = start.Index
			s.LongestStartWall = start.IntendedWall.UTC()
		}
		switch {
		case prev.Index == lastIndex:
			s.TailRun = n
		case n >= s.Threshold && anyNonOHT && lastNonOHT > prev.Index:
			s.StallRuns++
		}
		start, prev, n = nil, nil, 0
	}
	for _, rec := range sorted {
		if !oht(rec) {
			closeRun()
			continue
		}
		s.OHT++
		if n > 0 && rec.Index != prev.Index+1 {
			closeRun()
		}
		if n == 0 {
			start = rec
		}
		prev = rec
		n++
	}
	closeRun()
	return s
}
