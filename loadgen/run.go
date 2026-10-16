package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// maxEvidence is how many integrity-reason records carry Evidence per
// target per window or burst (DESIGN.md sec 10).
const maxEvidence = 8

// Target is one variant loadgen sends to: its ID, the base URL of its
// Service (no trailing slash), and the Checker and Ring that live as long
// as loadgen serves it. PR 4 makes and owns these: it makes a target's
// Checker and Ring together and drops them together, only after the
// target's records have been read (DESIGN.md sec 10).
type Target struct {
	ID      string
	URL     string
	Checker *Checker
	Ring    *Ring
}

// RunWindow sends one sustained-mode window to t: WindowSends(r, w)
// requests from a new Stream over pairs, send i intended at start + i/r
// (DESIGN.md sec 10). Each send's Record goes to t.Ring under runID, and
// at most 8 of this call's integrity-reason records carry Evidence.
//
// start must carry a monotonic reading (time.Now(), or a time derived
// from it with Add): the schedule and the records' monotonic times are
// measured from it.
//
// It returns WindowSends' error before sending anything. Otherwise it
// blocks until every send it launched has been recorded, so run one per
// target, each in its own goroutine. It returns nil after the last send,
// or ctx.Err() if ctx ends first: no later send is launched, and those
// already launched finish and are recorded.
func (s *Sender) RunWindow(ctx context.Context, t Target, runID string, start time.Time, r float64, w time.Duration, pairs []Pair) error {
	n, err := WindowSends(r, w)
	if err != nil {
		return err
	}
	stream := NewStream(pairs)
	return s.schedule(ctx, t, runID, start, r, n, func(int) (Request, error) { return stream.Next() })
}

// RunBurst sends one Stage 5 burst to t: Burst(n, pairs), send i intended
// at start + i/rate. It returns an error before sending anything if rate
// is not a positive finite number or Burst errors. Otherwise it records,
// blocks and returns as RunWindow does, and start must likewise carry a
// monotonic reading.
func (s *Sender) RunBurst(ctx context.Context, t Target, runID string, start time.Time, rate float64, n int, pairs []Pair) error {
	if !(rate > 0) || math.IsInf(rate, 1) {
		return fmt.Errorf("loadgen: burst rate %v, want a positive finite rate", rate)
	}
	reqs, err := Burst(n, pairs)
	if err != nil {
		return err
	}
	return s.schedule(ctx, t, runID, start, rate, n, func(i int) (Request, error) { return reqs[i], nil })
}

// schedule runs the open-loop schedule in the calling goroutine: send i
// of n is intended at start + i/rate, from the index, so no drift
// accumulates. It takes send i's request from next(i) before waiting for
// its time, waits on one timer, and launches each send in its own
// goroutine, so a slow response delays no later send. A late wake sends
// at once; no send is skipped to catch up. next's ErrOutOfPairs is a
// Skipped record and any other next error a Fault record, neither sent;
// a record is added only once its intended time has come. It returns
// when every launched send has been recorded: nil after the last launch,
// or ctx.Err() if ctx ends first.
func (s *Sender) schedule(ctx context.Context, t Target, runID string, start time.Time, rate float64, n int, next func(i int) (Request, error)) error {
	startMono, startWall := start.Sub(s.base), start.Round(0).UTC()
	var evidence atomic.Int64 // integrity-reason records in this call
	var wg sync.WaitGroup
	defer wg.Wait()
	timer := time.NewTimer(time.Until(start)) // armed for send 0
	defer timer.Stop()
	for i := range n {
		d := time.Duration(float64(i) * float64(time.Second) / rate)
		if i > 0 {
			// timer.C was drained by the last wait, so Reset leaves no stale fire.
			timer.Reset(time.Until(start.Add(d)))
		}
		rec := Record{Target: t.ID, RunID: runID, Index: i, IntendedMono: startMono + d, IntendedWall: startWall.Add(d)}
		req, err := next(i)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if ctx.Err() != nil { // a late wake can tie with ctx.Done in the select
			return ctx.Err()
		}
		switch {
		case errors.Is(err, ErrOutOfPairs):
			rec.Kind, rec.Skipped = KindVerify, true
			t.Ring.Add(rec)
		case err != nil:
			rec.Fault = err.Error()
			t.Ring.Add(rec)
		default:
			wg.Go(func() { s.sendRecord(t, req, rec, &evidence) })
		}
	}
	return nil
}

// sendRecord sends req to t, checks the response with t.Checker, and adds
// rec, filled in, to t.Ring. evidence counts the call's integrity-reason
// records; only the first maxEvidence carry Evidence.
func (s *Sender) sendRecord(t Target, req Request, rec Record, evidence *atomic.Int64) {
	rec.Kind, rec.Class = req.Kind, req.Class
	actual, resp, fault := s.send(t.URL, req)
	if fault != nil {
		rec.Fault = fault.Error()
		t.Ring.Add(rec)
		return
	}
	rec.ActualMono, rec.Latency = actual, resp.Latency
	v, pair, err := t.Checker.Check(req, resp)
	if err != nil {
		rec.Fault = err.Error() // Verdict and Pair stay zero: Check's are not the variant's
		t.Ring.Add(rec)
		return
	}
	rec.Verdict, rec.Pair = v, pair
	if classifier.Integrity(v.Reason) && evidence.Add(1) <= maxEvidence {
		rec.Evidence = &Evidence{RequestBody: req.Body, Status: resp.Status, Body: resp.Body}
	}
	t.Ring.Add(rec)
}
