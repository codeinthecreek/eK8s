package loadgen

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Evidence is what an integrity-reason record carries for the host to
// confirm the failure (DESIGN.md sec 9, 10).
type Evidence struct {
	RequestBody []byte // the request body as sent
	Status      int    // the response status
	Body        []byte // the response body, at most classifier.MaxBodyBytes+1 bytes
}

// Record is what happened to one send (DESIGN.md sec 10). Loadgen keeps
// raw records and no summaries; the orchestrator assigns each to a half
// by IntendedWall and computes every statistic. The monotonic times are
// durations since loadgen's process-start base, so they compare only
// within one boot_id.
type Record struct {
	// Seq is the record's place in its target's Ring, set by Ring.Add.
	Seq uint64
	// Target is the variant the send was for.
	Target string
	// RunID names the window or Stage 5 burst the send belongs to.
	RunID string
	// Index is i, the send's index in its window or burst.
	Index int
	// Kind is the request's kind.
	Kind Kind
	// Class is the negative's class, set only when Kind is KindNegative.
	Class NegativeClass
	// IntendedWall is its window's or burst's wall start + i/r, UTC, no monotonic.
	IntendedWall time.Time
	// IntendedMono is the intended send time, from the index.
	IntendedMono time.Duration
	// ActualMono is read just before client.Do; lag is ActualMono - IntendedMono.
	ActualMono time.Duration
	// Latency runs from ActualMono to the full body read.
	Latency time.Duration
	// Verdict is the per-request verdict, ignored if Skipped or Fault is set.
	Verdict Verdict
	// Skipped is set when a /verify was due with no pair left (ErrOutOfPairs).
	Skipped bool
	// Fault is a loadgen fault (a Check error), "" for none; it excludes the half.
	Fault string
	// Pair is the /hash pair the host must verify (Checker.Check), nil for none.
	Pair *HashPair
	// Evidence is set on the first 8 integrity-reason records per Target and RunID.
	Evidence *Evidence
}

// maxRecords bounds WindowSends and RingCapacity, so that converting
// round(r x W) to an int is defined and adding the burst cannot overflow.
const maxRecords = math.MaxInt32

// WindowSends returns the number of sends in a window of length w at
// rate r per second: round(r x W) (DESIGN.md sec 10). It errors for
// r <= 0, w <= 0, or a result below 1 or above maxRecords (a NaN or
// infinite r included).
func WindowSends(r float64, w time.Duration) (int, error) {
	if r <= 0 || w <= 0 {
		return 0, fmt.Errorf("loadgen: rate %v and window %v must both be positive", r, w)
	}
	x := math.Round(r * w.Seconds())
	if !(x >= 1 && x <= maxRecords) {
		return 0, fmt.Errorf("loadgen: rate %v over window %v rounds to %v sends, want 1 to %d", r, w, x, maxRecords)
	}
	return int(x), nil
}

// RingCapacity returns a target's ring capacity: WindowSends(r, w) plus
// maxBurst, the largest Stage 5 burst n (DESIGN.md sec 10). It errors
// whenever WindowSends does, for maxBurst < 0, and for a result above
// maxRecords.
func RingCapacity(r float64, w time.Duration, maxBurst int) (int, error) {
	if maxBurst < 0 {
		return 0, errors.New("loadgen: largest burst must not be negative")
	}
	n, err := WindowSends(r, w)
	if err != nil {
		return 0, err
	}
	if maxBurst > maxRecords-n {
		return 0, fmt.Errorf("loadgen: %d sends plus a burst of %d is over %d records", n, maxBurst, maxRecords)
	}
	return n + maxBurst, nil
}
