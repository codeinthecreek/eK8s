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
	"github.com/codeinthecreek/eK8s/internal/stallrun"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Why a window is excluded from scoring (DESIGN.md sec 10, sec 11), as
// WindowResult.Excluded lists them. Each is a lowercase token.
const (
	ExcludedRestart  = "loadgen-restart" // loadgen's boot_id changed: its records are gone
	ExcludedCNI      = "cni-not-ready"   // the CNI first look failed; the window did not start
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

// The halves of a window, in the order they run (DESIGN.md sec 9, sec
// 11), as WindowResult and VariantResult name them. Each half is a
// loadgen window run of its own, so a record's half is its run's.
const (
	HalfBaseline = "baseline"
	HalfStress   = "stress"
)

// HalvesPerWindow is how many batches RunWindow takes per live variant:
// one per half, since a run consumes its batch.
const HalvesPerWindow = 2

// WindowResult is one observation window's outcome: a baseline half,
// then a stress half (RunWindow), or the baseline half alone
// (RunBaseline).
type WindowResult struct {
	// RunID is the baseline half's run_id, which names the window;
	// StressRunID is the stress half's, "" when it did not run.
	RunID, StressRunID string
	// Excluded lists why the window is not scored, for every variant
	// alike: each half's reasons, sorted, each once. A window is scored
	// only when every half it ran was, and the stress half is not run
	// once the baseline half is excluded (docs/decisions.md, PR #68).
	Excluded []string
	// BaselineExcluded and StressExcluded are each half's own reasons.
	BaselineExcluded, StressExcluded []string
	// Variants holds every live variant's result, by id. It is empty
	// after a loadgen restart, whose records are gone, and when the
	// window never started.
	Variants map[string]VariantResult
	// BaselineSpan and StressSpan are each half's host time; zero for a
	// half that never started, and StressSpan zero after a loadgen
	// restart in the stress half. Recording only (docs/decisions.md,
	// 2026-11-04 D1-D7): nothing that decides a score, an exclusion, a
	// streak, a cull or a halt reads them.
	BaselineSpan, StressSpan Span
}

// Span is one half's host time: Start just before its first upload, End
// just after its last records are read.
type Span struct{ Start, End time.Time }

// Scored reports whether the window was scored.
func (w WindowResult) Scored() bool { return len(w.Excluded) == 0 }

// VariantResult is one variant's part of a window. It holds counts,
// reason names and scores only, never a record's text: records carry
// candidate-controlled bytes (docs/STATUS.md, Parked).
type VariantResult struct {
	Records int // every half's that ran
	// Disqualified is the host-confirmed integrity reason (DESIGN.md sec
	// 9), ReasonNone for none; DisqualifiedHalf is the half it was found
	// in (the baseline's first) and DisqualifiedIndex its record's index
	// in that half's run. A disqualified variant is culled before
	// scoring and never scored, whether or not the window is.
	Disqualified      classifier.Reason
	DisqualifiedHalf  string
	DisqualifiedIndex int
	// Baseline and Stress are the variant's half scores; nil when the
	// window is excluded or the variant disqualified, and Stress nil from
	// RunBaseline.
	Baseline, Stress *HalfScore
	// BaselineStall and StressStall are each half's stall counts for the
	// variant (stallrun.Detect over its records of that half, at r); nil
	// for a half that never started, and StressStall nil after a loadgen
	// restart in the stress half. They are set whether or not the window
	// is scored or the variant disqualified. Recording only
	// (docs/decisions.md, 2026-11-04 D1-D7): nothing that decides a
	// score, an exclusion, a streak, a cull or a halt reads them.
	BaselineStall, StressStall *stallrun.Stats
}

// HalfScore is one half's score for one variant.
type HalfScore struct {
	fitness.Result
	// CPUUnmeasured says why the kubelet's counters gave no CPU figure
	// for the variant in the half (a pod gone or a counter reset); it is
	// then scored at the CPU ceiling, earning no CPU credit. Empty when
	// they did.
	CPUUnmeasured string
}

// Fitness is the variant's fitness from the window: the lower of its two
// half scores (fitness.Combine; the baseline score alone from
// RunBaseline). ok is false when the variant was not scored.
func (v VariantResult) Fitness() (score float64, ok bool) {
	if v.Baseline == nil {
		return 0, false
	}
	if v.Stress == nil {
		return fitness.Combine(v.Baseline.Score, nil), true
	}
	return fitness.Combine(v.Baseline.Score, &v.Stress.Score), true
}

// RunWindow runs one observation window on the live variants (DESIGN.md
// sec 9-11): a baseline half with stress-ng off, then, unless the
// baseline half is excluded, a stress half with stress-ng on, each a
// loadgen window run of its own with its own batch (batches[2i] and
// batches[2i+1] for live[i]; extra batches are unused). Each half runs
// as runHalf describes; stress-ng is off again before any record is read
// for the stress half. Then the host judges the window: its integrity
// pass on every variant's records of every half that ran, whatever they
// are (a full verification of every /hash pair and a re-check of every
// integrity claim), and, unless excluded, each half's score for each
// variant not disqualified.
//
// A half that fails the first look or the canary is excluded without
// starting. A loadgen restart is an exclusion, and Recover must run
// before the next window; one in the stress half still leaves the
// baseline half judged. An error is a fault; on one the run may be left
// in loadgen, cancelled and drained where it could be.
func (s *Session) RunWindow(ctx context.Context, live []string, batches []Batch) (WindowResult, error) {
	return s.window(ctx, live, batches, HalvesPerWindow)
}

// RunBaseline runs a window of the baseline half alone, stress-ng off,
// with batches[i] for live[i]: C0's measurement, which has no stress
// (DESIGN.md sec 10; orchestrator/cmd/c0). It is RunWindow otherwise.
func (s *Session) RunBaseline(ctx context.Context, live []string, batches []Batch) (WindowResult, error) {
	return s.window(ctx, live, batches, 1)
}

// window runs a window of halves halves (1 or HalvesPerWindow), with
// batches[halves*i+h] for live[i] in half h.
func (s *Session) window(ctx context.Context, live []string, batches []Batch, halves int) (WindowResult, error) {
	if err := s.checkWindow(live, batches, halves); err != nil {
		return WindowResult{}, err
	}
	res := WindowResult{RunID: newRunID(), Variants: make(map[string]VariantResult)}
	restart := func(err error) (WindowResult, error) {
		if !errors.Is(err, client.ErrBootChanged) {
			return WindowResult{}, err
		}
		s.restarted = true
		res.Excluded, res.Variants = []string{ExcludedRestart}, map[string]VariantResult{}
		return res, nil
	}
	of := func(h int) []Batch {
		out := make([]Batch, len(live))
		for i := range live {
			out[i] = batches[halves*i+h]
		}
		return out
	}

	for i := range live {
		for h := range halves {
			s.used[batches[halves*i+h].key()] = true
		}
	}
	if err := s.cursors(ctx, live); err != nil {
		return restart(err)
	}
	base, err := s.runHalf(ctx, HalfBaseline, res.RunID, live, of(0))
	if err != nil {
		return restart(err)
	}
	res.BaselineExcluded, res.BaselineSpan = base.excluded, base.span
	ran := []*half{base}
	if halves == HalvesPerWindow && len(base.excluded) == 0 {
		res.StressRunID = newRunID()
		st, err := s.runHalf(ctx, HalfStress, res.StressRunID, live, of(1))
		if errors.Is(err, client.ErrBootChanged) {
			// The baseline half's records were read in full before the
			// restart: the host still judges them (DESIGN.md sec 11).
			s.restarted = true
			res.Excluded = []string{ExcludedRestart}
			if err := s.judge(ctx, &res, live, []*half{base}); err != nil {
				return WindowResult{}, err
			}
			return res, nil
		}
		if err != nil {
			return WindowResult{}, err
		}
		res.StressExcluded, res.StressSpan = st.excluded, st.span
		ran = append(ran, st)
	}
	for _, h := range ran {
		for _, why := range h.excluded {
			res.Excluded = appendOnce(res.Excluded, why)
		}
	}
	slices.Sort(res.Excluded)
	ran = slices.DeleteFunc(ran, func(h *half) bool { return !h.started })
	if len(ran) == 0 {
		return res, nil
	}

	if err := s.judge(ctx, &res, live, ran); err != nil {
		return WindowResult{}, err
	}
	if !res.Scored() {
		return res, nil
	}
	for _, h := range ran {
		if err := s.score(&res, live, h); err != nil {
			return WindowResult{}, err
		}
	}
	return res, nil
}

// half is what runHalf read of one half, for the host to judge and
// score.
type half struct {
	name     string
	batches  []Batch // batches[i] for live[i]
	excluded []string
	started  bool // the run started, and every record of it was read
	span     Span // set when started
	recs     map[string][]wire.Record
	cpu0     fitness.Sample
	cpu1     fitness.Sample
}

// runHalf runs one half on live as run runID (DESIGN.md sec 10, sec 11):
//
//  1. every batch uploaded under runID (a run consumes its batch, and an
//     upload replaces a pending one, so each half uploads its own);
//  2. stress-ng off and gone (baseline) or on and Running (stress);
//  3. the CNI first look and the policy canary (polled after a new
//     boot_id), either of which excludes the half without starting it;
//  4. the kubelet's CPU counters, the run, polled until no target is
//     running, and the counters again;
//  5. for the stress half, stress-ng off and gone, so that the host's
//     verification in the gap never shares the host with it;
//  6. every record of every target from its cursor, and the half's
//     exclusion decisions (the run's states, a ring gap, a skipped or
//     faulted send, schedule lag).
//
// Once a stress half has turned stress-ng on, it turns it off again on
// every return. An error is a fault or a loadgen restart.
func (s *Session) runHalf(ctx context.Context, name, runID string, live []string, batches []Batch) (h *half, err error) {
	h = &half{name: name, batches: batches, recs: make(map[string][]wire.Record, len(live))}
	start := time.Now()
	for i, id := range live {
		up, err := s.c.Client.PutPairs(ctx, id, runID, batches[i].upload())
		if err != nil {
			return nil, fmt.Errorf("orchestrator: uploading %s pairs for %s: %w", name, id, err)
		}
		if up.Count != batches[i].Len() {
			return nil, fmt.Errorf("orchestrator: loadgen holds %d pairs for %s, uploaded %d", up.Count, id, batches[i].Len())
		}
	}
	var stressOff func() error
	if name == HalfStress {
		// On from the scale call: a StressOn that scaled up and then
		// failed its wait still leaves pods to stop.
		on := true
		defer func() {
			if !on {
				return
			}
			// Detached: a cancelled orchestrator still stops stress-ng.
			if oerr := s.c.Cluster.StressOff(context.WithoutCancel(ctx)); oerr != nil {
				h, err = nil, errors.Join(err, oerr)
			}
		}()
		stressOff = func() error {
			if err := s.c.Cluster.StressOff(ctx); err != nil {
				return err // the defer tries again, detached
			}
			on = false
			return nil
		}
		if err := s.c.Cluster.StressOn(ctx); err != nil {
			return nil, err
		}
	} else if err := s.c.Cluster.StressOff(ctx); err != nil {
		return nil, err
	}
	ready, err := s.c.Cluster.CNIReady(ctx)
	if err != nil {
		return nil, err
	}
	if !ready {
		h.excluded = []string{ExcludedCNI}
		return h, nil
	}
	refused, err := s.canary(ctx)
	if err != nil {
		return nil, err
	}
	if !refused {
		h.excluded = []string{ExcludedCanary}
		return h, nil
	}

	if h.cpu0, err = s.c.Cluster.PodCPU(ctx); err != nil {
		return nil, err
	}
	states, err := s.runWindow(ctx, runID, live)
	if err != nil {
		return nil, err
	}
	if h.cpu1, err = s.c.Cluster.PodCPU(ctx); err != nil {
		return nil, err
	}
	if stressOff != nil {
		if err := stressOff(); err != nil {
			return nil, err
		}
	}

	for _, id := range live {
		r, gap, err := s.readRecords(ctx, id, s.next[id])
		if err != nil {
			return nil, err
		}
		done := states[id] == wire.StateDone
		if err := checkRecords(r, id, runID, s.cfg.WindowSends, done && !gap); err != nil {
			return nil, fmt.Errorf("orchestrator: target %s, %s half: %w", id, name, err)
		}
		h.recs[id] = r
		if !done {
			h.excluded = appendOnce(h.excluded, ExcludedRunEnded)
		}
		if gap {
			h.excluded = appendOnce(h.excluded, ExcludedGap)
		}
	}
	h.span = Span{Start: start, End: time.Now()}
	h.excluded = append(h.excluded, s.loadgenExclusions(h.recs)...)
	slices.Sort(h.excluded)
	h.started = true
	return h, nil
}

// judge is the host's integrity pass on every half that ran (DESIGN.md
// sec 8, 9): one full verification of every recorded /hash pair across
// targets and halves, then each variant's records judged half by half,
// baseline first, against its salt history and each half's batch truth.
// Claims in a half of which the host confirms none are a fault, as they
// were for a window of one run: loadgen and the host disagree about the
// same bytes (docs/decisions.md, PR #58 host-confirmation entry). It
// also records each variant's stall counts for each half (VariantResult).
func (s *Session) judge(ctx context.Context, res *WindowResult, live []string, ran []*half) error {
	key := func(h *half, id string) string { return h.name + "/" + id }
	recs := make(map[string][]wire.Record, len(live)*len(ran))
	var order []string
	for _, h := range ran {
		for _, id := range live {
			recs[key(h, id)] = h.recs[id]
			order = append(order, key(h, id))
		}
	}
	results, err := verifyAll(ctx, s.c.Core, recs, order, s.c.Workers)
	if err != nil {
		return err
	}
	for i, id := range live {
		vr := VariantResult{Disqualified: classifier.ReasonNone, DisqualifiedIndex: -1}
		for _, h := range ran {
			v := judgeIntegrity(s.c.Core, s.hist[id], h.batches[i].truth, h.recs[id], results[key(h, id)])
			if v.Reason == classifier.ReasonNone && v.Claims > 0 {
				return fmt.Errorf("orchestrator: target %s, %s half: %w (%d claims, %d with evidence unconfirmed)",
					id, h.name, errUnconfirmed, v.Claims, v.Unconfirmed)
			}
			vr.Records += len(h.recs[id])
			stall := stallrun.Detect(h.recs[id], s.cfg.Rate)
			if h.name == HalfStress {
				vr.StressStall = &stall
			} else {
				vr.BaselineStall = &stall
			}
			if v.Reason != classifier.ReasonNone && vr.Disqualified == classifier.ReasonNone {
				vr.Disqualified, vr.DisqualifiedHalf, vr.DisqualifiedIndex = v.Reason, h.name, v.Index
			}
		}
		res.Variants[id] = vr
	}
	return nil
}

// score scores half h for each live variant not disqualified. A
// variant's pods are its own doing (a crash, a scale-down): one whose
// counters fail scores the half at the CPU ceiling, never an exclusion
// it could cause at will. Every variant's at once is the counters', not
// the variants' doing (a metrics endpoint that drops pods), and a fault.
func (s *Session) score(res *WindowResult, live []string, h *half) error {
	scored, unmeasured := 0, 0
	for _, id := range live {
		vr := res.Variants[id]
		if vr.Disqualified != classifier.ReasonNone {
			continue
		}
		var hs HalfScore
		cpu, err := fitness.CPUSeconds(h.cpu0, h.cpu1, variant.Params{ID: id}.Namespace())
		if err != nil {
			hs.CPUUnmeasured = err.Error()
			cpu = s.c.Params.CPUCeiling * float64(successes(h.recs[id]))
		}
		if hs.Result, err = fitness.Score(s.c.Params, h.recs[id], cpu); err != nil {
			return fmt.Errorf("orchestrator: scoring %s's %s half: %w", id, h.name, err)
		}
		if h.name == HalfStress {
			vr.Stress = &hs
		} else {
			vr.Baseline = &hs
		}
		res.Variants[id] = vr
		scored++
		if hs.CPUUnmeasured != "" {
			unmeasured++
		}
	}
	if scored > 1 && unmeasured == scored {
		return fmt.Errorf("orchestrator: no variant's CPU counters were usable in the %s half (%d variants)", h.name, scored)
	}
	return nil
}

// checkWindow checks a window's arguments against the Session: halves
// batches per live variant.
func (s *Session) checkWindow(live []string, batches []Batch, halves int) error {
	switch {
	case s.restarted:
		return errors.New("orchestrator: loadgen restarted; Recover before the next window")
	case len(live) == 0:
		return errors.New("orchestrator: a window needs a live variant")
	case len(batches) < halves*len(live):
		return fmt.Errorf("orchestrator: %d batches for %d live variants and %d halves", len(batches), len(live), halves)
	}
	seen := make(map[string]bool, len(live))
	keys := make(map[string]bool, halves*len(live))
	for i, id := range live {
		if seen[id] {
			return fmt.Errorf("orchestrator: variant %s is live twice", id)
		}
		seen[id] = true
		if _, ok := s.hist[id]; !ok {
			return fmt.Errorf("orchestrator: variant %s is not adopted", id)
		}
		for _, b := range batches[halves*i : halves*(i+1)] {
			if b.Len() < s.cfg.WindowPairs {
				return fmt.Errorf("orchestrator: batch of %d pairs, want %d", b.Len(), s.cfg.WindowPairs)
			}
			// A pair is sent once, to one variant (DESIGN.md sec 10): a
			// batch used before, or twice now, would let a mutant memoize it.
			k := b.key()
			if s.used[k] || keys[k] {
				return errors.New("orchestrator: a batch is used twice")
			}
			keys[k] = true
		}
	}
	return nil
}

// cursors reads GET /targets and sets each live target's read cursor to
// its Next, where its window records will start. A live variant with no
// target is a fault.
func (s *Session) cursors(ctx context.Context, live []string) error {
	ts, err := s.c.Client.Targets(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: loadgen targets: %w", err)
	}
	next := make(map[string]uint64, len(ts.Targets))
	for _, t := range ts.Targets {
		next[t.ID] = t.Next
	}
	for _, id := range live {
		n, ok := next[id]
		if !ok {
			return fmt.Errorf("orchestrator: live variant %s has no loadgen target", id)
		}
		// Records before n are the gate's (its Stage 5 burst) or an
		// earlier window's: all read, so Drop may go through n - 1.
		s.next[id] = n
	}
	return nil
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
