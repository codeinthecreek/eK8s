package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/codeinthecreek/eK8s/deploy/variant"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Why a window is excluded from scoring (DESIGN.md sec 10, sec 11), as
// WindowResult.Excluded lists them. Each is a lowercase token.
const (
	ExcludedRestart  = "loadgen-restart"   // loadgen's boot_id changed: its records are gone
	ExcludedKindnet  = "kindnet-not-ready" // the first look failed; the window did not start
	ExcludedCanary   = "canary-not-refused"
	ExcludedRunEnded = "run-not-done" // a target ended cancelled or error
	ExcludedGap      = "ring-gap"
	ExcludedSkipped  = "skipped-send"
	ExcludedFault    = "loadgen-fault"
	ExcludedLagP99   = "lag-p99"
	ExcludedLagMax   = "lag-max"
)

// recordsPage is the records page size asked for: loadgen's cap.
const recordsPage = 1000

// WindowResult is one observation window's outcome. In the first run a
// window is one baseline half (docs/decisions.md, PR #52 S4), so its
// exclusion is the half's.
type WindowResult struct {
	RunID string
	// Excluded lists why the window is not scored, for every variant
	// alike; empty when it is scored.
	Excluded []string
	// Variants holds every live variant's result, by id. It is empty
	// after a loadgen restart, whose records are gone, and when the
	// window never started.
	Variants map[string]VariantResult
}

// Scored reports whether the window was scored.
func (w WindowResult) Scored() bool { return len(w.Excluded) == 0 }

// VariantResult is one variant's part of a window. It holds counts,
// reason names and scores only, never a record's text: records carry
// candidate-controlled bytes (docs/STATUS.md, Parked).
type VariantResult struct {
	Records int
	// Disqualified is the host-confirmed integrity reason (DESIGN.md sec
	// 9), ReasonNone for none; DisqualifiedIndex is its record's index.
	// A disqualified variant is culled before scoring and never scored,
	// whether or not the window is.
	Disqualified      classifier.Reason
	DisqualifiedIndex int
	// Score is the variant's baseline score; nil when the window is
	// excluded or the variant disqualified.
	Score *fitness.Result
	// CPUUnmeasured says why the kubelet's counters gave no CPU figure
	// for the variant (a pod gone or a counter reset in the window); it
	// is then scored at the CPU ceiling, earning no CPU credit. Empty
	// when they did.
	CPUUnmeasured string
}

// RunWindow runs one observation window on the live variants, each with
// its own batch (batches[i] for live[i]; extra batches are unused), and
// judges it on the host (DESIGN.md sec 10, sec 11):
//
//  1. pre-run: the targets' read cursors, every batch uploaded, stress-ng
//     off and gone, the kindnet first look, the policy canary (polled
//     after a new boot_id), the kubelet's CPU counters;
//  2. the window run, polled until no target is running;
//  3. the counters again, then every record of every target;
//  4. the exclusion decisions (the run's states, a ring gap, a skipped
//     or faulted send, schedule lag), the host's integrity pass on every
//     variant's records whatever they are (a full verification of every
//     /hash pair and a re-check of every integrity claim), and, unless
//     excluded, a baseline score for each variant not disqualified.
//
// A window that fails the first look or the canary is excluded without
// starting. A loadgen restart is an exclusion, and Recover must run
// before the next window. An error is a fault; on one the run may be
// left in loadgen, cancelled and drained where it could be.
func (s *Session) RunWindow(ctx context.Context, live []string, batches []Batch) (WindowResult, error) {
	if err := s.checkWindow(live, batches); err != nil {
		return WindowResult{}, err
	}
	res := WindowResult{RunID: newRunID(), Variants: make(map[string]VariantResult)}
	restart := func(err error) (WindowResult, error) {
		if !errors.Is(err, client.ErrBootChanged) {
			return WindowResult{}, err
		}
		s.restarted = true
		return WindowResult{RunID: res.RunID, Excluded: []string{ExcludedRestart}, Variants: map[string]VariantResult{}}, nil
	}
	excluded := func(why string) (WindowResult, error) {
		return WindowResult{RunID: res.RunID, Excluded: []string{why}, Variants: map[string]VariantResult{}}, nil
	}

	for i := range live {
		s.used[batches[i].key()] = true
	}
	since, err := s.cursors(ctx, live)
	if err != nil {
		return restart(err)
	}
	for i, id := range live {
		up, err := s.c.Client.PutPairs(ctx, id, res.RunID, batches[i].upload())
		if err != nil {
			return restart(fmt.Errorf("orchestrator: uploading pairs for %s: %w", id, err))
		}
		if up.Count != batches[i].Len() {
			return WindowResult{}, fmt.Errorf("orchestrator: loadgen holds %d pairs for %s, uploaded %d", up.Count, id, batches[i].Len())
		}
	}
	if err := s.c.Cluster.StressOff(ctx); err != nil {
		return WindowResult{}, err
	}
	ready, err := s.c.Cluster.KindnetReady(ctx)
	if err != nil {
		return WindowResult{}, err
	}
	if !ready {
		return excluded(ExcludedKindnet)
	}
	refused, err := s.canary(ctx)
	if err != nil {
		return restart(err)
	}
	if !refused {
		return excluded(ExcludedCanary)
	}

	cpu0, err := s.c.Cluster.PodCPU(ctx)
	if err != nil {
		return WindowResult{}, err
	}
	states, err := s.runWindow(ctx, res.RunID, live)
	if err != nil {
		return restart(err)
	}
	cpu1, err := s.c.Cluster.PodCPU(ctx)
	if err != nil {
		return WindowResult{}, err
	}

	recs := make(map[string][]wire.Record, len(live))
	var why []string
	for _, id := range live {
		r, gap, err := s.readRecords(ctx, id, since[id])
		if err != nil {
			return restart(err)
		}
		done := states[id] == wire.StateDone
		if err := checkRecords(r, id, res.RunID, s.cfg.WindowSends, done && !gap); err != nil {
			return WindowResult{}, fmt.Errorf("orchestrator: target %s: %w", id, err)
		}
		recs[id] = r
		if !done {
			why = appendOnce(why, ExcludedRunEnded)
		}
		if gap {
			why = appendOnce(why, ExcludedGap)
		}
	}
	why = append(why, s.loadgenExclusions(recs)...)

	results, err := verifyAll(ctx, s.c.Core, recs, live, s.c.Workers)
	if err != nil {
		return WindowResult{}, err
	}
	for i, id := range live {
		v := judgeIntegrity(s.c.Core, s.hist[id], batches[i].truth, recs[id], results[id])
		if v.Reason == classifier.ReasonNone && v.Claims > 0 {
			return WindowResult{}, fmt.Errorf("orchestrator: target %s: %w (%d claims, %d with evidence unconfirmed)",
				id, errUnconfirmed, v.Claims, v.Unconfirmed)
		}
		res.Variants[id] = VariantResult{Records: len(recs[id]), Disqualified: v.Reason, DisqualifiedIndex: v.Index}
	}
	slices.Sort(why)
	res.Excluded = why
	if !res.Scored() {
		return res, nil
	}
	scored, unmeasured := 0, 0
	for _, id := range live {
		vr := res.Variants[id]
		if vr.Disqualified != classifier.ReasonNone {
			continue
		}
		cpu, err := fitness.CPUSeconds(cpu0, cpu1, variant.Params{ID: id}.Namespace())
		if err != nil {
			// A variant's pods are its own doing (a crash, a scale-down):
			// no CPU credit, never an exclusion it could cause at will.
			vr.CPUUnmeasured = err.Error()
			cpu = s.c.Params.CPUCeiling * float64(successes(recs[id]))
		}
		score, err := fitness.Score(s.c.Params, recs[id], cpu)
		if err != nil {
			return WindowResult{}, fmt.Errorf("orchestrator: scoring %s: %w", id, err)
		}
		vr.Score = &score
		res.Variants[id] = vr
		scored++
		if vr.CPUUnmeasured != "" {
			unmeasured++
		}
	}
	// Every variant at once is the counters', not the variants' doing
	// (a metrics endpoint that drops pods); one variant alone cannot be
	// told apart, and pays for it.
	if scored > 1 && unmeasured == scored {
		return WindowResult{}, fmt.Errorf("orchestrator: no variant's CPU counters were usable (%d variants)", scored)
	}
	return res, nil
}

// checkWindow checks RunWindow's arguments against the Session.
func (s *Session) checkWindow(live []string, batches []Batch) error {
	switch {
	case s.restarted:
		return errors.New("orchestrator: loadgen restarted; Recover before the next window")
	case len(live) == 0:
		return errors.New("orchestrator: a window needs a live variant")
	case len(batches) < len(live):
		return fmt.Errorf("orchestrator: %d batches for %d live variants", len(batches), len(live))
	}
	seen := make(map[string]bool, len(live))
	keys := make(map[string]bool, len(live))
	for i, id := range live {
		if seen[id] {
			return fmt.Errorf("orchestrator: variant %s is live twice", id)
		}
		seen[id] = true
		if _, ok := s.hist[id]; !ok {
			return fmt.Errorf("orchestrator: variant %s is not adopted", id)
		}
		if batches[i].Len() < s.cfg.WindowPairs {
			return fmt.Errorf("orchestrator: batch of %d pairs, want %d", batches[i].Len(), s.cfg.WindowPairs)
		}
		// A pair is sent once, to one variant (DESIGN.md sec 10): a batch
		// used before, or twice now, would let a mutant memoize it.
		k := batches[i].key()
		if s.used[k] || keys[k] {
			return errors.New("orchestrator: a batch is used twice")
		}
		keys[k] = true
	}
	return nil
}

// cursors reads GET /targets and returns each live target's Next, where
// its window records will start. A live variant with no target is a
// fault.
func (s *Session) cursors(ctx context.Context, live []string) (map[string]uint64, error) {
	ts, err := s.c.Client.Targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: loadgen targets: %w", err)
	}
	next := make(map[string]uint64, len(ts.Targets))
	for _, t := range ts.Targets {
		next[t.ID] = t.Next
	}
	since := make(map[string]uint64, len(live))
	for _, id := range live {
		n, ok := next[id]
		if !ok {
			return nil, fmt.Errorf("orchestrator: live variant %s has no loadgen target", id)
		}
		since[id] = n
		// Records before n are the gate's (its Stage 5 burst) or an
		// earlier window's: all read, so Drop may go through n - 1.
		s.next[id] = n
	}
	return since, nil
}

// canary runs the policy canary once, or, after a new boot_id, polls it
// until it reads refused, at most canaryBound (DESIGN.md sec 11). It
// reports whether it read refused; connected and error both exclude the
// half.
func (s *Session) canary(ctx context.Context) (bool, error) {
	deadline := time.Now().Add(s.t.canaryBound)
	for {
		c, err := s.c.Client.Canary(ctx)
		if err != nil {
			return false, fmt.Errorf("orchestrator: canary: %w", err)
		}
		if c.Result == wire.CanaryRefused {
			s.pollCanary = false
			return true, nil
		}
		if !s.pollCanary || !time.Now().Add(s.t.canaryEvery).Before(deadline) {
			return false, nil
		}
		if err := sleep(ctx, s.t.canaryEvery); err != nil {
			return false, err
		}
	}
}

// runWindow starts the window on live and polls it until no target is
// running, at most the window's length plus one client timeout and
// runSlack. It returns each target's final state. If ctx ends or the
// bound passes it cancels and drains the run (abandon).
func (s *Session) runWindow(ctx context.Context, runID string, live []string) (map[string]string, error) {
	if _, err := s.c.Client.Start(ctx, runID, wire.Start{Kind: wire.KindWindow, Targets: live}); err != nil {
		// A status loadgen answered means it started nothing (a start is
		// all or nothing); any other failure may have started the run.
		var ae *client.APIError
		if !errors.As(err, &ae) {
			err = s.abandon(ctx, runID, live, err)
		}
		return nil, fmt.Errorf("orchestrator: starting the window: %w", err)
	}
	length := time.Duration(s.cfg.WindowS * float64(time.Second))
	deadline := time.Now().Add(length + s.t.clientTimeout + s.t.runSlack)
	for {
		run, err := s.c.Client.Run(ctx, runID)
		if err != nil {
			return nil, s.abandon(ctx, runID, live, fmt.Errorf("orchestrator: polling the window: %w", err))
		}
		states, err := runStates(run, live)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(mapValues(states), wire.StateRunning) {
			return states, nil
		}
		if !time.Now().Add(s.t.pollEvery).Before(deadline) {
			return nil, s.abandon(ctx, runID, live, errors.New("orchestrator: the window is still running past its bound"))
		}
		if err := sleep(ctx, s.t.pollEvery); err != nil {
			return nil, s.abandon(ctx, runID, live, err)
		}
	}
}

// abandon cancels the run and waits, at most drainBound, for every
// target to leave running, on a context detached from ctx's
// cancellation so that a cancelled orchestrator still stops its run. It
// returns cause, joined with any drain error. A cause that is a loadgen
// restart skips the drain.
func (s *Session) abandon(ctx context.Context, runID string, live []string, cause error) error {
	if errors.Is(cause, client.ErrBootChanged) {
		return cause
	}
	dctx, stop := context.WithTimeout(context.WithoutCancel(ctx), s.t.drainBound)
	defer stop()
	run, err := s.c.Client.Cancel(dctx, runID)
	for err == nil {
		var states map[string]string
		if states, err = runStates(run, live); err != nil || !slices.Contains(mapValues(states), wire.StateRunning) {
			break
		}
		if err = sleep(dctx, s.t.pollEvery); err != nil {
			break
		}
		run, err = s.c.Client.Run(dctx, runID)
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("orchestrator: draining the cancelled window: %w", err))
	}
	return cause
}

// runStates returns each live target's state in run, which must be a
// window of exactly the live targets.
func runStates(run wire.Run, live []string) (map[string]string, error) {
	if run.Kind != wire.KindWindow || len(run.Targets) != len(live) {
		return nil, fmt.Errorf("orchestrator: run is a %q of %d targets, want a window of %d", run.Kind, len(run.Targets), len(live))
	}
	out := make(map[string]string, len(live))
	for _, id := range live {
		rt, ok := run.Targets[id]
		if !ok {
			return nil, fmt.Errorf("orchestrator: run has no target %s", id)
		}
		switch rt.State {
		case wire.StateRunning, wire.StateDone, wire.StateCancelled, wire.StateError:
		default:
			return nil, fmt.Errorf("orchestrator: target %s in state %q", id, rt.State)
		}
		out[id] = rt.State
	}
	return out, nil
}

// readRecords reads target id's records from since, page by page, and
// moves its cursor past them. gap reports records overwritten before
// they were read.
func (s *Session) readRecords(ctx context.Context, id string, since uint64) (recs []wire.Record, gap bool, err error) {
	limit := s.cfg.WindowSends/recordsPage + 2
	for pages := 0; ; pages++ {
		if pages > limit {
			return nil, false, fmt.Errorf("orchestrator: target %s: records past the window's last page", id)
		}
		page, err := s.c.Client.Records(ctx, id, since, recordsPage)
		if err != nil {
			return nil, false, fmt.Errorf("orchestrator: reading %s's records: %w", id, err)
		}
		gap = gap || page.Gap
		if len(page.Records) == 0 {
			break
		}
		if page.Next <= since {
			return nil, false, fmt.Errorf("orchestrator: target %s: records page since %d has next %d", id, since, page.Next)
		}
		recs = append(recs, page.Records...)
		since = page.Next
	}
	s.next[id] = since
	return recs, gap, nil
}

// checkRecords checks one target's window records: each of this target
// and run, an index below n and none twice, and, when complete is set
// (the target ended done with no gap), exactly n of them. A record that
// loadgen could not account for the same way gate Stage 5 refuses one
// (checkBurstRecords) is an error too: an unknown bucket on a record
// that is neither skipped nor faulted, a pair on a record that is not a
// /hash, a success /hash with no pair (a hash left unverified). Skipped
// and faulted records pass: they exclude the window instead.
func checkRecords(recs []wire.Record, id, runID string, n int, complete bool) error {
	seen := make([]bool, n)
	for _, r := range recs {
		switch {
		case r.Target != id || r.RunID != runID:
			return fmt.Errorf("record %d is of another target or run", r.Seq)
		case r.Index < 0 || r.Index >= n:
			return fmt.Errorf("record %d has index %d, of %d", r.Seq, r.Index, n)
		case seen[r.Index]:
			return fmt.Errorf("index %d is recorded twice", r.Index)
		case r.Pair != nil && r.Kind != kindHash:
			return fmt.Errorf("record %d has a pair on a %q", r.Seq, r.Kind)
		}
		seen[r.Index] = true
		if r.Skipped || r.Fault != "" {
			continue
		}
		switch r.Bucket {
		case classifier.Success.String(), classifier.GracefulShed.String(), classifier.Failed.String():
		default:
			return fmt.Errorf("record %d has bucket %q", r.Seq, r.Bucket)
		}
		if r.Kind == kindHash && r.Bucket == classifier.Success.String() && r.Pair == nil {
			return fmt.Errorf("record %d is a success hash with no pair", r.Seq)
		}
	}
	if complete && len(recs) != n {
		return fmt.Errorf("%d records, want %d", len(recs), n)
	}
	return nil
}

// loadgenExclusions are the exclusion triggers loadgen-wide, over every
// target's records (loadgen is one process): a skipped send, a faulted
// send, lag p99 over lagP99, any lag over lagMax (DESIGN.md sec 10). A
// skipped or faulted record's lag is left out.
func (s *Session) loadgenExclusions(recs map[string][]wire.Record) []string {
	var why []string
	var lags []time.Duration
	for _, rs := range recs {
		for _, r := range rs {
			switch {
			case r.Skipped:
				why = appendOnce(why, ExcludedSkipped)
			case r.Fault != "":
				why = appendOnce(why, ExcludedFault)
			default:
				lags = append(lags, time.Duration(r.ActualMonoNS-r.IntendedMonoNS))
			}
		}
	}
	if len(lags) == 0 {
		return why
	}
	slices.Sort(lags)
	// Nearest rank, as fitness's p95.
	if p99 := lags[(99*len(lags)+99)/100-1]; p99 > s.t.lagP99 {
		why = append(why, ExcludedLagP99)
	}
	if lags[len(lags)-1] > s.t.lagMax {
		why = append(why, ExcludedLagMax)
	}
	return why
}

// successes counts recs' scored successes, the records fitness.Score
// divides CPU-seconds by.
func successes(recs []wire.Record) int {
	n := 0
	for _, r := range recs {
		if r.Kind != kindNegative && r.Bucket == classifier.Success.String() {
			n++
		}
	}
	return n
}

func appendOnce(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Streak counts consecutive excluded windows: after 3 the orchestrator
// flags the run and halts (DESIGN.md sec 10).
type Streak struct{ n int }

// HaltAfter is the number of consecutive excluded windows that halts a
// run (DESIGN.md sec 10, a starting guess).
const HaltAfter = 3

// Add counts one window and reports whether the run must halt.
func (s *Streak) Add(w WindowResult) bool {
	if w.Scored() {
		s.n = 0
		return false
	}
	s.n++
	return s.n >= HaltAfter
}
