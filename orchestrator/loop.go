package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/codeinthecreek/eK8s/deploy/variant"
	"github.com/codeinthecreek/eK8s/driver"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/stallrun"
	"github.com/codeinthecreek/eK8s/lineage"
)

// Why a run stopped, as its summary records it (DESIGN.md sec 13). Each
// is a lowercase token.
const (
	StopGenerations  = "generations"      // every generation asked for was attempted
	StopUsageCeiling = "usage_ceiling"    // the mutation calls' turns or tokens reached a ceiling
	StopExcluded     = "excluded_windows" // HaltAfter windows in a row were excluded
	StopRestart      = "loadgen_restart"  // loadgen's boot_id changed: the first run halts (PR #52 S10)
	StopNoParent     = "no_parent"        // no scored genome is eligible as a parent
	StopFault        = "fault"            // a fault of the harness: the loop's error says which
	StopInterrupted  = "interrupted"      // the caller's context ended
)

// LiveCap is the live population cap; a newcomer makes it LiveCap + 1
// for its probation window (DESIGN.md sec 11).
const LiveCap = 3

// maxDetailLog caps a gate Detail in the run log.
const maxDetailLog = 1 << 10

// Mutator runs one mutation call; *driver.Driver is the implementation.
type Mutator interface {
	Mutate(ctx context.Context, parent map[string][]byte, prompt string) (*driver.Mutation, error)
}

// Gater runs the gate on one candidate; GateRunner is the
// implementation. Each error is a gate fault, never the candidate's.
type Gater interface {
	// Local runs Stages 1-4 on the candidate files in dir (gate.Run).
	Local(ctx context.Context, dir string) (gate.Report, error)
	// Stage5 runs Stage 5 on variant id from image, adding to r
	// (gate.Stage5): stress-ng must be on.
	Stage5(ctx context.Context, id, image string, r *gate.Report) error
}

// Windows is what the loop needs of the window mechanics; *Session is
// the implementation.
type Windows interface {
	Batches(ctx context.Context, n int) ([]Batch, error)
	RunWindow(ctx context.Context, live []string, batches []Batch) (WindowResult, error)
	Adopt(id string, burstHashes []string) error
	Drop(ctx context.Context, id string) error
	Restarted() bool
}

var _ Windows = (*Session)(nil)

// LoopConfig is what a Loop needs.
type LoopConfig struct {
	Run     *lineage.Run // a new run directory, with no attempt yet
	Windows Windows
	Cluster Cluster // StressOn before Stage 5, DeleteNamespace after a Stage 5 failure
	Gate    Gater
	Mutator Mutator
	Params  fitness.Params
	// Gen0 is generation 0's files: the hand-written wrapper.
	Gen0 map[string][]byte
	// Generations is how many mutation attempts the run makes, at most.
	Generations int
	// TurnCeiling stops the run once the mutation calls' num_turns sum
	// to it; TokenCeiling once their tokens, of every kind, do (0: none).
	// DESIGN.md sec 13: set them from observed usage.
	TurnCeiling  int
	TokenCeiling int64
	// ScratchDir holds each candidate's files while the gate runs.
	ScratchDir string
	// Rand draws the tournaments; nil seeds one at random.
	Rand *rand.Rand
	// Log receives one line per step; nil discards them.
	Log io.Writer
}

// Loop is the first run's generational process (DESIGN.md sec 11, with
// the first-run simplifications of docs/decisions.md, PR #52): generation
// 0 is gated and scored, then each generation selects a parent by
// tournament (k = 2) from the scored archive, mutates it while the
// host makes the next window's pairs, gates the candidate (Stages 1-4,
// then stress on and Stage 5), and on a pass runs the newcomer's
// probation: windows until one is scored or the newcomer is gone, with
// every confirmed integrity failure culled first, then the lowest score
// back to LiveCap. Every attempt is logged in lineage.
//
// A fault of the harness (a gate, cluster, loadgen or lineage error)
// halts the run, as does a loadgen restart (S10: no Recover in the first
// run) or HaltAfter excluded windows in a row. A halted run leaves its
// variants deployed for inspection.
type Loop struct {
	c   LoopConfig
	now func() time.Time

	live    []string                   // live variant ids, in the order they joined
	records map[string]lineage.Attempt // the latest record of every attempt, by id
	usage   lineage.Usage              // every mutation call's, recorded or not
	gens    int                        // mutation attempts made
	streak  Streak
}

// NewLoop checks c and returns a Loop for it.
func NewLoop(c LoopConfig) (*Loop, error) {
	switch {
	case c.Run == nil || c.Windows == nil || c.Cluster == nil || c.Gate == nil || c.Mutator == nil:
		return nil, errors.New("orchestrator: loop needs a run, windows, a cluster, a gate and a mutator")
	case c.Generations < 0 || c.TurnCeiling < 1 || c.TokenCeiling < 0:
		return nil, fmt.Errorf("orchestrator: %d generations, turn ceiling %d, token ceiling %d", c.Generations, c.TurnCeiling, c.TokenCeiling)
	case len(c.Gen0) == 0:
		return nil, errors.New("orchestrator: no generation 0 files")
	case !filepath.IsAbs(c.ScratchDir):
		return nil, fmt.Errorf("orchestrator: scratch dir %q is not absolute", c.ScratchDir)
	}
	if err := c.Params.Validate(); err != nil {
		return nil, err
	}
	prior, err := c.Run.Attempts()
	if err != nil {
		return nil, err
	}
	if len(prior) > 0 {
		return nil, errors.New("orchestrator: the run directory has attempts already; a run starts in a new one")
	}
	if c.Rand == nil {
		c.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	return &Loop{c: c, now: time.Now, records: make(map[string]lineage.Attempt)}, nil
}

// Run runs the loop to its end and writes the run summary. It returns
// why the run stopped; the error is set for StopFault and
// StopInterrupted.
func (l *Loop) Run(ctx context.Context) (string, error) {
	reason, err := l.run(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		reason = StopInterrupted
	case err != nil:
		reason = StopFault
	}
	l.logf("run stopped: %s (%d generations, %d turns)", reason, l.gens, l.usage.NumTurns)
	sum := lineage.Summary{Reason: reason, Generations: l.gens, Usage: l.usage, Time: l.now()}
	if serr := l.c.Run.WriteSummary(sum); serr != nil {
		err = errors.Join(err, serr)
	}
	return reason, err
}

func (l *Loop) run(ctx context.Context) (string, error) {
	// Stress is on only for Stage 5 and each window's stress half
	// (DESIGN.md sec 11); a halted run may have left it on.
	if err := l.c.Cluster.StressOff(ctx); err != nil {
		return "", err
	}
	// Generation 0: the gate first, then its pairs, never overlapping.
	passed, rep, err := l.gateCandidate(ctx, "0", l.c.Gen0)
	if err != nil {
		return "", err
	}
	if !passed {
		if err := l.discard("0", "", rep, nil); err != nil {
			return "", err
		}
		last := rep.Results[len(rep.Results)-1]
		return "", fmt.Errorf("orchestrator: generation 0 failed the gate at stage %d, check %s", last.Stage, last.Check)
	}
	if err := l.promote("0", "", l.c.Gen0, rep, nil); err != nil {
		return "", err
	}
	if stop, err := l.probation(ctx, "0", nil); stop != "" || err != nil {
		return stop, err
	}

	for g := 1; g <= l.c.Generations; g++ {
		if stop := l.ceiling(); stop != "" {
			return stop, nil
		}
		parent, ok := l.selectParent()
		if !ok {
			return StopNoParent, nil
		}
		l.gens++
		if stop, err := l.generation(ctx, strconv.Itoa(g), parent); stop != "" || err != nil {
			return stop, err
		}
	}
	if stop := l.ceiling(); stop != "" {
		return stop, nil
	}
	return StopGenerations, nil
}

// generation runs one mutation attempt, id, on parent.
func (l *Loop) generation(ctx context.Context, id, parent string) (string, error) {
	all, err := l.c.Run.Attempts()
	if err != nil {
		return "", err
	}
	prompt, err := driver.BuildPrompt(all, parent, l.c.Params)
	if err != nil {
		return "", err
	}
	files, _, err := l.c.Run.Genome(parent)
	if err != nil {
		return "", err
	}
	l.logf("generation %s: mutating %s", id, parent)

	// The gap (DESIGN.md sec 11): the next window's pairs, a batch per
	// half for each live variant and for the newcomer, are made while the
	// mutation runs, and the gate starts only once they are done.
	type made struct {
		b   []Batch
		err error
	}
	bctx, stopBatches := context.WithCancel(ctx)
	defer stopBatches()
	ch := make(chan made, 1)
	go func() {
		b, err := l.c.Windows.Batches(bctx, HalvesPerWindow*(len(l.live)+1))
		ch <- made{b, err}
	}()
	mut, merr := l.c.Mutator.Mutate(ctx, files, prompt)
	noCandidate := merr != nil || len(mut.Changed) == 0
	if noCandidate {
		stopBatches() // no window follows: the pairs are not needed
	}
	pairs := <-ch

	if mut != nil {
		l.add(mut.Usage)
		if len(mut.Transcript) > 0 {
			if err := l.c.Run.WriteTranscript(id, mut.Transcript); err != nil {
				return "", err
			}
		}
	}
	// A call that never started a session failed in the harness (a
	// changed claude binary, scratch I/O, a refused parent or prompt):
	// it would fail every generation, so it halts the run.
	if merr != nil && (ctx.Err() != nil || mut == nil) {
		return "", merr
	}
	if noCandidate {
		why := lineage.MutationNoChange
		if merr != nil {
			why = lineage.MutationFailed
			l.logf("generation %s: %v", id, merr)
		}
		l.logf("generation %s: %s, discarded", id, why)
		a := l.attempt(id, parent, mut)
		a.Mutation, a.Action = why, lineage.ActionDiscarded
		return "", l.record(a)
	}
	if pairs.err != nil {
		return "", pairs.err
	}

	passed, rep, err := l.gateCandidate(ctx, id, mut.Files)
	if err != nil {
		return "", err
	}
	if !passed {
		return "", l.discard(id, parent, rep, mut)
	}
	if err := l.promote(id, parent, mut.Files, rep, mut); err != nil {
		return "", err
	}
	return l.probation(ctx, id, pairs.b)
}

// gateCandidate runs the gate on a candidate: Stages 1-4, then stress-ng on and
// Stage 5 as variant id. passed reports whether every check passed; on a
// Stage 5 failure the variant's namespace is deleted (gate.Stage5 has
// dropped its loadgen target). An error is a gate fault.
func (l *Loop) gateCandidate(ctx context.Context, id string, files map[string][]byte) (passed bool, rep gate.Report, err error) {
	dir, err := os.MkdirTemp(l.c.ScratchDir, "candidate-"+id+"-")
	if err != nil {
		return false, rep, fmt.Errorf("orchestrator: %w", err)
	}
	defer os.RemoveAll(dir)
	for name, b := range files {
		if filepath.Base(name) != name {
			return false, rep, fmt.Errorf("orchestrator: candidate file %q is not a plain name", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return false, rep, fmt.Errorf("orchestrator: %w", err)
		}
	}
	l.logf("generation %s: gate stages 1-4", id)
	if rep, err = l.c.Gate.Local(ctx, dir); err != nil {
		return false, rep, fmt.Errorf("orchestrator: gate fault, generation %s: %w", id, err)
	}
	if len(rep.Results) == 0 {
		return false, rep, fmt.Errorf("orchestrator: gate fault, generation %s: an empty report", id)
	}
	if !rep.Passed() {
		l.logGateFailure(id, rep)
		return false, rep, nil
	}
	if err := l.c.Cluster.StressOn(ctx); err != nil {
		return false, rep, err
	}
	l.logf("generation %s: gate stage 5", id)
	serr := l.c.Gate.Stage5(ctx, id, rep.Image, &rep)
	// Recorded whenever the burst was read, pass, fail or fault, and
	// before a fault halts the run; it decides nothing.
	if rep.Stage5Stall != nil {
		l.appendStall(stallLine(lineage.StallBurst, rep.Stage5RunID, "", id, rep.Stage5Stall, Span{}))
	}
	if serr != nil {
		return false, rep, fmt.Errorf("orchestrator: gate fault, generation %s: %w", id, serr)
	}
	// Off again at once, so neither the next host pairs nor the next
	// gate run shares the host with stress-ng, and the next window's
	// baseline half starts with it gone.
	if err := l.c.Cluster.StressOff(ctx); err != nil {
		return false, rep, err
	}
	if len(rep.Results) == 0 {
		return false, rep, fmt.Errorf("orchestrator: gate fault, generation %s: an empty report", id)
	}
	if !rep.Passed() {
		l.logGateFailure(id, rep)
		return false, rep, l.c.Cluster.DeleteNamespace(ctx, variant.Params{ID: id}.Namespace())
	}
	return true, rep, nil
}

// logGateFailure logs the failing check. Its Detail can carry
// candidate-controlled output, so it goes to the log only, quoted and
// cut, never to lineage or a prompt (docs/STATUS.md, Parked).
func (l *Loop) logGateFailure(id string, rep gate.Report) {
	last := rep.Results[len(rep.Results)-1]
	d := last.Detail
	if len(d) > maxDetailLog {
		d = d[:maxDetailLog]
	}
	l.logf("generation %s: failed the gate at stage %d, check %s: %q", id, last.Stage, last.Check, d)
}

// discard records a candidate that failed the gate.
func (l *Loop) discard(id, parent string, rep gate.Report, mut *driver.Mutation) error {
	last := rep.Results[len(rep.Results)-1]
	a := l.attempt(id, parent, mut)
	a.Gate = lineage.Gate{FailStage: last.Stage, FailCheck: last.Check}
	a.Action = lineage.ActionDiscarded
	return l.record(a)
}

// promote archives a gate-passing candidate, starts its host salt
// history from its burst, marks it live and records it.
func (l *Loop) promote(id, parent string, files map[string][]byte, rep gate.Report, mut *driver.Mutation) error {
	digest, err := l.c.Run.ArchiveGenome(id, files)
	if err != nil {
		return err
	}
	if err := l.c.Windows.Adopt(id, rep.BurstHashes); err != nil {
		return err
	}
	l.live = append(l.live, id)
	a := l.attempt(id, parent, mut)
	a.Gate, a.GenomeSHA256 = lineage.Gate{Passed: true}, digest
	a.Action, a.EverLive, a.CurrentlyLive = lineage.ActionPromoted, true, true
	l.logf("generation %s: passed the gate; live %q", id, l.live)
	return l.record(a)
}

// probation runs windows on the live variants, the newcomer among them,
// until one is scored or the newcomer is gone. Each window's confirmed
// integrity failures are culled first, scored or not; a scored window
// then updates every live variant's fitness and culls the lowest score
// until LiveCap are left. batches are the first window's, made in the
// gap; nil makes them here.
func (l *Loop) probation(ctx context.Context, newcomer string, batches []Batch) (string, error) {
	for {
		if batches == nil {
			b, err := l.c.Windows.Batches(ctx, HalvesPerWindow*len(l.live))
			if err != nil {
				return "", err
			}
			batches = b
		}
		l.logf("window: live %q", l.live)
		res, err := l.c.Windows.RunWindow(ctx, l.live, batches)
		if err != nil {
			return "", err
		}
		l.windowStalls(res)
		batches = nil
		if l.c.Windows.Restarted() {
			return StopRestart, nil
		}
		if err := l.judge(ctx, res); err != nil {
			if l.c.Windows.Restarted() { // a cull's DELETE met a new boot_id
				l.logf("%v", err)
				return StopRestart, nil
			}
			return "", err
		}
		if l.streak.Add(res) {
			return StopExcluded, nil
		}
		if res.Scored() || !slices.Contains(l.live, newcomer) {
			return "", nil
		}
	}
}

// judge applies one window's result: integrity culls, then, if it was
// scored, every live variant's fitness and the culls back to LiveCap.
func (l *Loop) judge(ctx context.Context, res WindowResult) error {
	if res.Scored() {
		l.logf("window %s: scored", res.RunID)
	} else {
		l.logf("window %s: excluded %q (baseline %q, stress %q)", res.RunID, res.Excluded, res.BaselineExcluded, res.StressExcluded)
	}
	for _, id := range slices.Clone(l.live) {
		vr, ok := res.Variants[id]
		if !ok {
			if res.Scored() {
				return fmt.Errorf("orchestrator: scored window %s has no result for %s", res.RunID, id)
			}
			continue
		}
		if vr.Disqualified == classifier.ReasonNone {
			continue
		}
		l.logf("window %s: %s disqualified (%s, %s half), culled", res.RunID, id, vr.Disqualified, vr.DisqualifiedHalf)
		a := l.records[id]
		a.Disqualified = string(vr.Disqualified)
		if err := l.cull(ctx, a); err != nil {
			return err
		}
	}
	if !res.Scored() {
		return nil
	}
	scores := make(map[string]float64, len(l.live))
	for _, id := range l.live {
		vr := res.Variants[id]
		if vr.Baseline == nil || vr.Stress == nil {
			return fmt.Errorf("orchestrator: scored window %s has no score for %s in both halves", res.RunID, id)
		}
		a := l.records[id]
		a.Fitness = toFitness(vr)
		scores[id] = a.Fitness.Score
		for _, h := range []struct {
			name string
			s    *HalfScore
		}{{HalfBaseline, vr.Baseline}, {HalfStress, vr.Stress}} {
			if h.s.CPUUnmeasured != "" {
				l.logf("window %s: %s scored at the CPU ceiling in the %s half: %s", res.RunID, id, h.name, h.s.CPUUnmeasured)
			}
		}
		l.logf("window %s: %s scored %.4f (baseline %.4f, stress %.4f)", res.RunID, id, a.Fitness.Score, vr.Baseline.Score, vr.Stress.Score)
		if err := l.record(a); err != nil {
			return err
		}
	}
	for len(l.live) > LiveCap {
		low := l.live[0]
		for _, id := range l.live[1:] {
			// A tie culls the newer: an incumbent keeps its place.
			if scores[id] <= scores[low] {
				low = id
			}
		}
		l.logf("window %s: %s has the lowest score, culled", res.RunID, low)
		if err := l.cull(ctx, l.records[low]); err != nil {
			return err
		}
	}
	return nil
}

// windowStalls records each half's stall counts for every variant in
// res, one line per (half, variant) that ran, baseline half first. It is
// recording only: nothing in the loop reads the lines or the counts.
func (l *Loop) windowStalls(res WindowResult) {
	ids := slices.SortedFunc(maps.Keys(res.Variants), cmpID)
	for _, id := range ids {
		if st := res.Variants[id].BaselineStall; st != nil {
			l.appendStall(stallLine(lineage.StallBaseline, res.RunID, res.RunID, id, st, res.BaselineSpan))
		}
	}
	for _, id := range ids {
		if st := res.Variants[id].StressStall; st != nil {
			l.appendStall(stallLine(lineage.StallStress, res.StressRunID, res.RunID, id, st, res.StressSpan))
		}
	}
}

// appendStall appends s to stalls.jsonl. A failed write is one log line
// (the run's stderr) and never a fault, an exclusion or a halt
// (docs/decisions.md, 2026-11-04, the owner's answer Q4).
func (l *Loop) appendStall(s lineage.Stall) {
	if err := l.c.Run.AppendStall(s); err != nil {
		l.logf("stalls: %s %s %s not recorded: %v", s.Kind, s.RunID, s.GenerationID, err)
	}
}

// stallLine is one stalls.jsonl line: st's counts for target id in
// loadgen run runID, of window windowRunID ("" for a burst), over span.
func stallLine(kind, runID, windowRunID, id string, st *stallrun.Stats, span Span) lineage.Stall {
	return lineage.Stall{
		Kind: kind, RunID: runID, WindowRunID: windowRunID, GenerationID: id,
		Records: st.Records, OHT: st.OHT, StallRuns: st.StallRuns, TailRun: st.TailRun,
		Longest: st.Longest, LongestStartIndex: st.LongestStartIndex, LongestStartWall: st.LongestStartWall,
		Threshold: st.Threshold, SpanStart: span.Start, SpanEnd: span.End,
	}
}

// cull retires a live variant (Session.Drop) and records a, its updated
// record, as archived and no longer live.
func (l *Loop) cull(ctx context.Context, a lineage.Attempt) error {
	if err := l.c.Windows.Drop(ctx, a.GenerationID); err != nil {
		return err
	}
	l.live = slices.DeleteFunc(l.live, func(id string) bool { return id == a.GenerationID })
	a.Action, a.CurrentlyLive = lineage.ActionArchived, false
	return l.record(a)
}

// toFitness is a variant's scored window as lineage records it: both
// halves' components and scores, and the variant's fitness, the lower
// half score (DESIGN.md sec 9).
func toFitness(vr VariantResult) *lineage.Fitness {
	half := func(r *HalfScore) *lineage.Half {
		if r == nil {
			return nil
		}
		return &lineage.Half{
			SuccessRate:   r.SuccessRate,
			P95LatencyMS:  float64(r.P95Latency) / float64(time.Millisecond),
			CPUPerSuccess: r.CPUPerSuccess,
			Score:         r.Score,
		}
	}
	score, _ := vr.Fitness()
	return &lineage.Fitness{Score: score, Baseline: half(vr.Baseline), Stress: half(vr.Stress)}
}

// selectParent draws a tournament of k = 2 from the eligible archive:
// every genome that passed the gate, is not disqualified and has a
// scored fitness, live or not (DESIGN.md sec 11). The higher score wins;
// a tie goes to the first drawn. ok is false when none is eligible.
func (l *Loop) selectParent() (string, bool) {
	var pool []string
	for id, a := range l.records {
		if a.Gate.Passed && a.Disqualified == "" && a.Fitness != nil && a.Fitness.Baseline != nil && !a.Fitness.Baseline.Excluded {
			pool = append(pool, id)
		}
	}
	if len(pool) == 0 {
		return "", false
	}
	// Map order is random; the draw must depend on Rand alone.
	slices.SortFunc(pool, func(a, b string) int { return cmpID(a, b) })
	if len(pool) == 1 {
		return pool[0], true
	}
	i := l.c.Rand.IntN(len(pool))
	j := l.c.Rand.IntN(len(pool) - 1)
	if j >= i {
		j++
	}
	a, b := pool[i], pool[j]
	if l.records[b].Fitness.Score > l.records[a].Fitness.Score {
		return b, true
	}
	return a, true
}

// cmpID orders decimal ids numerically.
func cmpID(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ceiling reports StopUsageCeiling once the mutation calls' usage has
// reached a ceiling.
func (l *Loop) ceiling() string {
	u := l.usage
	tokens := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
	if u.NumTurns >= l.c.TurnCeiling || (l.c.TokenCeiling > 0 && tokens >= l.c.TokenCeiling) {
		return StopUsageCeiling
	}
	return ""
}

func (l *Loop) add(u lineage.Usage) {
	l.usage.NumTurns += u.NumTurns
	l.usage.InputTokens += u.InputTokens
	l.usage.OutputTokens += u.OutputTokens
	l.usage.CacheReadTokens += u.CacheReadTokens
	l.usage.CacheCreationTokens += u.CacheCreationTokens
	l.usage.DurationMS += u.DurationMS
	l.usage.CostUSD += u.CostUSD
}

// attempt starts attempt id's record from its mutation call, if any.
func (l *Loop) attempt(id, parent string, mut *driver.Mutation) lineage.Attempt {
	a := lineage.Attempt{GenerationID: id, ParentID: parent, Time: l.now(), SelectionMode: lineage.ModeTournament}
	if mut != nil {
		a.Usage, a.Rationale = mut.Usage, mut.Rationale
	}
	return a
}

// record appends a and keeps it as id's latest record. A record holds
// ids, enums, numbers and the model's rationale only, never a gate
// Detail or a window record's text (lineage package doc).
func (l *Loop) record(a lineage.Attempt) error {
	a.Time = l.now()
	if err := l.c.Run.AppendAttempt(a); err != nil {
		return err
	}
	l.records[a.GenerationID] = a
	return nil
}

func (l *Loop) logf(format string, args ...any) {
	fmt.Fprintf(l.c.Log, "%s "+format+"\n", append([]any{l.now().UTC().Format(time.RFC3339)}, args...)...)
}
