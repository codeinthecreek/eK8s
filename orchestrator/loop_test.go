package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/driver"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/lineage"
)

// These tests run the Loop against fakes: the window mechanics, the
// gate, the mutator and the cluster each append to one event log, so a
// test can check what ran and in which order.

type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, fmt.Sprintf(format, args...))
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// index returns the position of the first event equal to s, or -1.
func (e *events) index(s string) int { return slices.Index(e.all(), s) }

// fakeWindows scores every live variant by score[id] unless window, if
// set, says otherwise for the n-th window (from 1).
type fakeWindows struct {
	ev        *events
	score     map[string]float64
	window    func(n int, live []string) WindowResult
	windows   int
	restarted bool
	dropErr   error // Drop fails with it and sets restarted
	batchErr  error
	batchCtx  []error // each Batches call's context error at its end
}

func (w *fakeWindows) Batches(ctx context.Context, n int) ([]Batch, error) {
	w.ev.add("batches %d", n)
	if w.batchErr != nil {
		return nil, w.batchErr
	}
	// Long enough that the mutation ends first, unless cancelled.
	select {
	case <-ctx.Done():
	case <-time.After(20 * time.Millisecond):
	}
	w.ev.mu.Lock()
	w.batchCtx = append(w.batchCtx, ctx.Err())
	w.ev.mu.Unlock()
	w.ev.add("batches done")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return make([]Batch, n), nil
}

func (w *fakeWindows) RunWindow(_ context.Context, live []string, batches []Batch) (WindowResult, error) {
	w.windows++
	w.ev.add("window %s", strings.Join(live, ","))
	if len(batches) < HalvesPerWindow*len(live) {
		return WindowResult{}, fmt.Errorf("%d batches for %d live", len(batches), len(live))
	}
	if w.window != nil {
		if res := w.window(w.windows, live); res.RunID != "" {
			return res, nil
		}
	}
	return scoredWindow(w.windows, live, w.score), nil
}

// scoredWindow scores each live variant score[id] in its stress half
// and 0.1 more in its baseline half, so its fitness, the lower, is
// score[id].
func scoredWindow(n int, live []string, score map[string]float64) WindowResult {
	res := WindowResult{RunID: fmt.Sprintf("w-%d", n), Variants: make(map[string]VariantResult)}
	for _, id := range live {
		b, s := &HalfScore{}, &HalfScore{}
		b.Score, s.Score = score[id]+0.1, score[id]
		b.SuccessRate, b.P95Latency, b.CPUPerSuccess = 0.99, 250*time.Millisecond, 0.2
		s.SuccessRate, s.P95Latency, s.CPUPerSuccess = 0.9, 400*time.Millisecond, 0.3
		res.Variants[id] = VariantResult{Records: 40, Baseline: b, Stress: s}
	}
	return res
}

func (w *fakeWindows) Adopt(id string, burst []string) error {
	w.ev.add("adopt %s %d", id, len(burst))
	return nil
}

func (w *fakeWindows) Drop(_ context.Context, id string) error {
	w.ev.add("drop %s", id)
	if w.dropErr != nil {
		w.restarted = true
		return w.dropErr
	}
	return nil
}

func (w *fakeWindows) Restarted() bool { return w.restarted }

// fakeGate fails a candidate whose server.go holds "fail-local" at
// Stage 2, "fail-stage5" at Stage 5, and faults on "fault".
type fakeGate struct{ ev *events }

func (g fakeGate) Local(_ context.Context, dir string) (gate.Report, error) {
	b, err := os.ReadFile(filepath.Join(dir, "server.go"))
	if err != nil {
		return gate.Report{}, err
	}
	g.ev.add("local %s", b)
	switch {
	case strings.Contains(string(b), "fault"):
		return gate.Report{}, errors.New("no docker daemon")
	case strings.Contains(string(b), "fail-local"):
		return gate.Report{Results: []gate.Result{{Stage: 1, Check: "build", Pass: true}, {Stage: 2, Check: "contract", Detail: "secret pool password\n" + strings.Repeat("x", 2000)}}}, nil
	}
	return gate.Report{Results: []gate.Result{{Stage: 4, Check: "race-report", Pass: true}}, Image: "ek8s-wrapper:x"}, nil
}

func (g fakeGate) Stage5(_ context.Context, id, image string, r *gate.Report) error {
	g.ev.add("stage5 %s %s", id, image)
	if failStage5[id] {
		r.Results = append(r.Results, gate.Result{Stage: 5, Check: "hash-verify", Detail: "candidate text"})
		return nil
	}
	r.Results = append(r.Results, gate.Result{Stage: 5, Check: "hash-verify", Pass: true})
	r.BurstHashes = []string{"h1", "h2"}
	return nil
}

// failStage5 names the variant ids Stage 5 fails; tests set it before
// they run and never in parallel.
var failStage5 = map[string]bool{}

// fakeMutator answers call n (from 1) with mut(n).
type fakeMutator struct {
	ev      *events
	calls   int
	overlap bool // fail a call unless a Batches call is running
	mut     func(n int, parent map[string][]byte) (*driver.Mutation, error)
}

func (m *fakeMutator) Mutate(ctx context.Context, parent map[string][]byte, prompt string) (*driver.Mutation, error) {
	m.calls++
	m.ev.add("mutate %s", parent["server.go"])
	if m.overlap {
		// The gap's pairs are made while the mutation runs: a Batches
		// call must have started and not yet ended.
		deadline := time.Now().Add(time.Second)
		for {
			ev := m.ev.all()
			if start := lastPrefix(ev, "batches "); start >= 0 && !slices.Contains(ev[start:], "batches done") {
				break
			}
			if time.Now().After(deadline) {
				return nil, errors.New("no batches running alongside the mutation")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !strings.Contains(prompt, driver.NonNegotiable) {
		return nil, errors.New("prompt without the non-negotiable line")
	}
	return m.mut(m.calls, parent)
}

// lastPrefix returns the index of the last event starting with p, or -1.
func lastPrefix(ev []string, p string) int {
	for i := len(ev) - 1; i >= 0; i-- {
		if strings.HasPrefix(ev[i], p) && ev[i] != "batches done" {
			return i
		}
	}
	return -1
}

// edit returns a successful mutation that sets server.go to body.
func edit(parent map[string][]byte, body string, turns int) *driver.Mutation {
	files := make(map[string][]byte, len(parent))
	for k, v := range parent {
		files[k] = v
	}
	files["server.go"] = []byte(body)
	return &driver.Mutation{
		Files: files, Changed: []string{"server.go"}, Rationale: "changed " + body,
		Usage: lineage.Usage{NumTurns: turns, InputTokens: 100, OutputTokens: 10}, Transcript: []byte(`{"type":"result"}` + "\n"),
	}
}

type loopCluster struct{ ev *events }

func (c loopCluster) StressOff(context.Context) error { c.ev.add("stress off"); return nil }
func (c loopCluster) StressOn(context.Context) error  { c.ev.add("stress on"); return nil }
func (c loopCluster) CNIReady(context.Context) (bool, error) {
	return true, nil
}
func (c loopCluster) PodCPU(context.Context) (fitness.Sample, error) { return nil, nil }
func (c loopCluster) DeleteNamespace(_ context.Context, ns string) error {
	c.ev.add("delete %s", ns)
	return nil
}

var gen0Files = map[string][]byte{
	"doc.go": []byte("package wrapper"), "hasher.go": []byte("hasher"),
	"server.go": []byte("gen0"), "tunables.go": []byte("tunables"),
}

type loopFixture struct {
	ev   *events
	win  *fakeWindows
	mut  *fakeMutator
	run  *lineage.Run
	loop *Loop
	cfg  LoopConfig
}

func newLoopFixture(t *testing.T, gens int, mut func(n int, parent map[string][]byte) (*driver.Mutation, error)) *loopFixture {
	t.Helper()
	ev := &events{}
	run, err := lineage.Create(filepath.Join(t.TempDir(), "run"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	fx := &loopFixture{
		ev: ev, run: run,
		win: &fakeWindows{ev: ev, score: map[string]float64{}},
		mut: &fakeMutator{ev: ev, mut: mut},
	}
	fx.cfg = LoopConfig{
		Run: run, Windows: fx.win, Cluster: loopCluster{ev}, Gate: fakeGate{ev}, Mutator: fx.mut,
		Params: fitness.DefaultParams(), Gen0: gen0Files, Generations: gens, TurnCeiling: 1000,
		ScratchDir: t.TempDir(), Rand: rand.New(rand.NewPCG(1, 2)),
	}
	t.Cleanup(func() { clear(failStage5) })
	return fx
}

func (fx *loopFixture) start(t *testing.T) (string, error) {
	t.Helper()
	l, err := NewLoop(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.loop = l
	return l.Run(context.Background())
}

func (fx *loopFixture) attempts(t *testing.T) map[string]lineage.Attempt {
	t.Helper()
	all, err := fx.run.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]lineage.Attempt, len(all))
	for _, a := range all {
		out[a.GenerationID] = a
	}
	return out
}

func (fx *loopFixture) summary(t *testing.T) lineage.Summary {
	t.Helper()
	s, err := fx.run.ReadSummary()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestLoopRun: generation 0 is gated and scored alone; each passing
// newcomer gets a window with the live variants; once five are live the
// lowest is culled back to three, a tie culling the newer; every
// attempt is recorded, and the summary says the generations ran out.
func TestLoopRun(t *testing.T) {
	fx := newLoopFixture(t, 4, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
		return edit(parent, fmt.Sprintf("gen%d", n), 3), nil
	})
	// gen 3 ties gen 0 at the bottom of the window with four live: the
	// newer, 3, goes. Then 4 is the lowest of 0, 1, 2, 4.
	fx.win.score = map[string]float64{"0": 0.5, "1": 0.7, "2": 0.6, "3": 0.5, "4": 0.4}
	fx.mut.overlap = true
	reason, err := fx.start(t)
	if err != nil || reason != StopGenerations {
		t.Fatalf("Run = %q, %v", reason, err)
	}
	as := fx.attempts(t)
	if len(as) != 5 {
		t.Fatalf("%d attempts, want 5: %+v", len(as), as)
	}
	for id, want := range map[string]struct {
		action string
		live   bool
		score  float64
	}{
		"0": {lineage.ActionPromoted, true, 0.5}, "1": {lineage.ActionPromoted, true, 0.7},
		"2": {lineage.ActionPromoted, true, 0.6}, "3": {lineage.ActionArchived, false, 0.5},
		"4": {lineage.ActionArchived, false, 0.4},
	} {
		a := as[id]
		if a.Action != want.action || a.CurrentlyLive != want.live || !a.EverLive || a.Fitness == nil || a.Fitness.Score != want.score {
			t.Errorf("attempt %s = %+v, want %+v", id, a, want)
		}
		if !a.Gate.Passed || a.GenomeSHA256 == "" || a.Fitness.Baseline == nil || a.Fitness.Baseline.P95LatencyMS != 250 ||
			a.Fitness.Stress == nil || a.Fitness.Stress.P95LatencyMS != 400 || a.Fitness.Stress.Score != want.score ||
			a.Fitness.Baseline.Score != want.score+0.1 {
			t.Errorf("attempt %s gate or fitness = %+v %+v", id, a.Gate, a.Fitness)
		}
		if id != "0" && (a.Usage.NumTurns != 3 || a.Rationale == "") {
			t.Errorf("attempt %s usage %+v rationale %q", id, a.Usage, a.Rationale)
		}
		if _, err := fx.run.Transcript(id); (err == nil) != (id != "0") {
			t.Errorf("transcript %s: %v", id, err)
		}
	}
	if !slices.Equal(fx.loop.live, []string{"0", "1", "2"}) {
		t.Errorf("live = %q", fx.loop.live)
	}
	s := fx.summary(t)
	if s.Reason != StopGenerations || s.Generations != 4 || s.Usage.NumTurns != 12 || s.Usage.InputTokens != 400 {
		t.Errorf("summary = %+v", s)
	}
	ev := fx.ev.all()
	for _, want := range []string{"window 0", "window 0,1", "window 0,1,2", "window 0,1,2,3", "window 0,1,2,4", "drop 3", "drop 4", "adopt 4 2"} {
		if !slices.Contains(ev, want) {
			t.Errorf("no event %q in %q", want, ev)
		}
	}
	// Generation 0: the gate, then a batch per half, then the window.
	if !(fx.ev.index("stage5 0 ek8s-wrapper:x") < fx.ev.index("batches 2") && fx.ev.index("batches 2") < fx.ev.index("window 0")) {
		t.Errorf("generation 0 order: %q", ev)
	}
	// Generation 1 onwards: pairs for live + 1 alongside the mutation,
	// done before the gate; stress on before Stage 5.
	g1 := fx.ev.index("mutate gen0")
	rest := ev[g1:]
	order := []string{"batches done", "local gen1", "stress on", "stage5 1 ek8s-wrapper:x", "stress off", "adopt 1 2", "window 0,1"}
	at := -1
	for _, want := range order {
		i := slices.Index(rest, want)
		if i <= at {
			t.Fatalf("generation 1: %q out of order in %q", want, rest)
		}
		at = i
	}
	// The batches goroutine may log before or after "mutate gen0", so the
	// span is the gap since generation 0's window, not since the mutation.
	gap := ev[slices.Index(ev, "window 0"):]
	if !slices.Contains(gap[:slices.Index(gap, "local gen1")], "batches 4") {
		t.Errorf("generation 1 made no batches for live + 1 before its gate: %q", rest)
	}
	if ev[0] != "stress off" {
		t.Errorf("the run did not start with stress off: %q", ev)
	}
}

// TestLoopNoCandidate: a failed session and one that edited nothing are
// recorded with their reason and usage, never gated, and run no window;
// their pairs are cancelled.
func TestLoopNoCandidate(t *testing.T) {
	fx := newLoopFixture(t, 2, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
		if n == 1 {
			return &driver.Mutation{Usage: lineage.Usage{NumTurns: 30}, Transcript: []byte("{}\n")}, errors.New("driver: session ended error_max_turns")
		}
		return &driver.Mutation{Files: parent, Rationale: "nothing to do", Usage: lineage.Usage{NumTurns: 2}}, nil
	})
	fx.win.score["0"] = 0.5
	reason, err := fx.start(t)
	if err != nil || reason != StopGenerations {
		t.Fatalf("Run = %q, %v", reason, err)
	}
	as := fx.attempts(t)
	if a := as["1"]; a.Mutation != lineage.MutationFailed || a.Action != lineage.ActionDiscarded || a.Usage.NumTurns != 30 || a.ParentID != "0" {
		t.Errorf("attempt 1 = %+v", a)
	}
	if a := as["2"]; a.Mutation != lineage.MutationNoChange || a.Rationale != "nothing to do" || a.Usage.NumTurns != 2 {
		t.Errorf("attempt 2 = %+v", a)
	}
	if _, err := fx.run.Transcript("1"); err != nil {
		t.Errorf("failed session's transcript: %v", err)
	}
	if fx.win.windows != 1 {
		t.Errorf("%d windows, want generation 0's only", fx.win.windows)
	}
	for _, e := range fx.ev.all() {
		if strings.HasPrefix(e, "local gen0") && fx.ev.index(e) > fx.ev.index("mutate gen0") {
			t.Errorf("a gate ran after a mutation: %q", fx.ev.all())
		}
	}
	if len(fx.win.batchCtx) != 3 {
		t.Fatalf("%d Batches calls, want 3", len(fx.win.batchCtx))
	}
	for i, err := range fx.win.batchCtx[1:] {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("generation %d's pairs were not cancelled: %v", i+1, err)
		}
	}
	if s := fx.summary(t); s.Usage.NumTurns != 32 || s.Generations != 2 {
		t.Errorf("summary = %+v", s)
	}
}

// TestLoopGateFailures: a Stage 1-4 failure and a Stage 5 failure are
// discarded with their stage and check, no Detail; the Stage 5 failure's
// namespace is deleted; neither runs a window.
func TestLoopGateFailures(t *testing.T) {
	fx := newLoopFixture(t, 2, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
		if n == 1 {
			return edit(parent, "fail-local", 1), nil
		}
		return edit(parent, "gen2", 1), nil
	})
	failStage5["2"] = true
	fx.win.score["0"] = 0.5
	var log strings.Builder
	fx.cfg.Log = &log
	reason, err := fx.start(t)
	if err != nil || reason != StopGenerations {
		t.Fatalf("Run = %q, %v", reason, err)
	}
	as := fx.attempts(t)
	if a := as["1"]; a.Gate != (lineage.Gate{FailStage: 2, FailCheck: "contract"}) || a.Action != lineage.ActionDiscarded || a.GenomeSHA256 != "" {
		t.Errorf("attempt 1 = %+v", a)
	}
	if a := as["2"]; a.Gate != (lineage.Gate{FailStage: 5, FailCheck: "hash-verify"}) || a.EverLive {
		t.Errorf("attempt 2 = %+v", a)
	}
	if fx.run.HasGenome("1") || fx.run.HasGenome("2") {
		t.Error("a failed candidate was archived")
	}
	ev := fx.ev.all()
	if !slices.Contains(ev, "delete ek8s-v2") || slices.Contains(ev, "delete ek8s-v1") || slices.Contains(ev, "stage5 1 ek8s-wrapper:x") {
		t.Errorf("events = %q", ev)
	}
	if fx.win.windows != 1 {
		t.Errorf("%d windows, want 1", fx.win.windows)
	}
	// The Detail goes to the log only, quoted and cut.
	if !strings.Contains(log.String(), `check contract: "secret pool password\n`+strings.Repeat("x", maxDetailLog-len("secret pool password\n"))+`"`+"\n") {
		t.Errorf("log lacks the quoted, cut Detail:\n%s", log.String())
	}
	if !slices.Contains(ev, "stress off") || ev[len(ev)-1] == "stress on" {
		t.Errorf("stress left on after Stage 5: %q", ev)
	}
	b, _ := os.ReadFile(filepath.Join(fx.run.Dir(), "attempts.jsonl"))
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "candidate text") {
		t.Errorf("a gate Detail reached lineage: %s", b)
	}
}

// TestLoopProbation: an excluded window repeats with fresh batches; a
// disqualification culls in an excluded window too, and a newcomer
// culled so ends its probation; generation 0 disqualified leaves no
// parent.
func TestLoopProbation(t *testing.T) {
	excluded := func(n int, live []string) WindowResult {
		res := WindowResult{RunID: fmt.Sprintf("w-%d", n), Excluded: []string{ExcludedLagMax}, Variants: map[string]VariantResult{}}
		for _, id := range live {
			res.Variants[id] = VariantResult{Records: 20}
		}
		return res
	}
	t.Run("excluded then scored", func(t *testing.T) {
		fx := newLoopFixture(t, 1, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, "gen1", 1), nil
		})
		fx.win.score = map[string]float64{"0": 0.5, "1": 0.6}
		fx.win.window = func(n int, live []string) WindowResult {
			if n == 2 || n == 3 { // generation 1's first two windows
				return excluded(n, live)
			}
			return WindowResult{}
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopGenerations || fx.win.windows != 4 {
			t.Fatalf("Run = %q, %v after %d windows", reason, err, fx.win.windows)
		}
		if n := strings.Count(strings.Join(fx.ev.all(), "|"), "batches 4"); n != 3 {
			t.Errorf("%d batch calls for 2 live, 2 halves, want 3 (one per window): %q", n, fx.ev.all())
		}
		if a := fx.attempts(t)["1"]; a.Fitness == nil || a.Fitness.Score != 0.6 {
			t.Errorf("attempt 1 = %+v", a)
		}
	})
	t.Run("disqualified in an excluded window", func(t *testing.T) {
		fx := newLoopFixture(t, 1, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, "gen1", 1), nil
		})
		fx.win.score["0"] = 0.5
		fx.win.window = func(n int, live []string) WindowResult {
			if n == 2 {
				res := excluded(n, live)
				res.Variants["1"] = VariantResult{Disqualified: classifier.ReasonSaltReused}
				return res
			}
			return WindowResult{}
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopGenerations || fx.win.windows != 2 {
			t.Fatalf("Run = %q, %v after %d windows", reason, err, fx.win.windows)
		}
		a := fx.attempts(t)["1"]
		if a.Disqualified != "salt_reused" || a.Action != lineage.ActionArchived || a.CurrentlyLive || !a.EverLive {
			t.Errorf("attempt 1 = %+v", a)
		}
		if !slices.Contains(fx.ev.all(), "drop 1") || !slices.Equal(fx.loop.live, []string{"0"}) {
			t.Errorf("live %q, events %q", fx.loop.live, fx.ev.all())
		}
	})
	t.Run("integrity cull restores the cap", func(t *testing.T) {
		fx := newLoopFixture(t, 3, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, fmt.Sprintf("gen%d", n), 1), nil
		})
		// 3 scores lowest, but 1, an incumbent, is disqualified in the
		// scored window with four live: no further cull (sec 11 step 7).
		fx.win.score = map[string]float64{"0": 0.5, "1": 0.9, "2": 0.6, "3": 0.1}
		fx.win.window = func(n int, live []string) WindowResult {
			if len(live) == 4 {
				res := scoredWindow(n, live, fx.win.score)
				res.Variants["1"] = VariantResult{Disqualified: classifier.ReasonWrongMatch}
				return res
			}
			return WindowResult{}
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopGenerations {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if !slices.Equal(fx.loop.live, []string{"0", "2", "3"}) {
			t.Errorf("live = %q, want 0 2 3", fx.loop.live)
		}
		as := fx.attempts(t)
		if a := as["1"]; a.Disqualified != "wrong_match" || a.CurrentlyLive {
			t.Errorf("attempt 1 = %+v", a)
		}
		if a := as["3"]; !a.CurrentlyLive || a.Fitness == nil || a.Fitness.Score != 0.1 {
			t.Errorf("attempt 3 = %+v", a)
		}
	})
	t.Run("restart met by a cull", func(t *testing.T) {
		fx := newLoopFixture(t, 1, nil)
		fx.win.dropErr = errors.New("loadgen's boot_id changed")
		fx.win.window = func(n int, live []string) WindowResult {
			res := scoredWindow(n, live, nil)
			res.Variants["0"] = VariantResult{Disqualified: classifier.ReasonSaltReused}
			return res
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopRestart {
			t.Fatalf("Run = %q, %v", reason, err)
		}
	})
	t.Run("no parent", func(t *testing.T) {
		fx := newLoopFixture(t, 3, nil)
		fx.win.window = func(n int, live []string) WindowResult {
			res := scoredWindow(n, live, nil)
			res.Variants["0"] = VariantResult{Disqualified: classifier.ReasonVerifyMismatch}
			return res
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopNoParent || fx.mut.calls != 0 {
			t.Fatalf("Run = %q, %v, %d mutations", reason, err, fx.mut.calls)
		}
		if s := fx.summary(t); s.Reason != StopNoParent || s.Generations != 0 {
			t.Errorf("summary = %+v", s)
		}
	})
	t.Run("excluded streak", func(t *testing.T) {
		fx := newLoopFixture(t, 3, nil)
		fx.win.window = excluded
		reason, err := fx.start(t)
		if err != nil || reason != StopExcluded || fx.win.windows != HaltAfter {
			t.Fatalf("Run = %q, %v after %d windows", reason, err, fx.win.windows)
		}
	})
	t.Run("restart", func(t *testing.T) {
		fx := newLoopFixture(t, 3, nil)
		fx.win.window = func(n int, live []string) WindowResult {
			fx.win.restarted = true
			return WindowResult{RunID: "w-1", Excluded: []string{ExcludedRestart}, Variants: map[string]VariantResult{}}
		}
		reason, err := fx.start(t)
		if err != nil || reason != StopRestart || fx.win.windows != 1 {
			t.Fatalf("Run = %q, %v after %d windows", reason, err, fx.win.windows)
		}
	})
}

// TestLoopStops: the usage ceiling, a gate fault, generation 0 failing
// the gate, a fault making pairs, and an interrupt.
func TestLoopStops(t *testing.T) {
	t.Run("turn ceiling", func(t *testing.T) {
		fx := newLoopFixture(t, 10, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return &driver.Mutation{Files: parent, Usage: lineage.Usage{NumTurns: 30}}, nil
		})
		fx.cfg.TurnCeiling = 60
		fx.win.score["0"] = 0.5
		reason, err := fx.start(t)
		if err != nil || reason != StopUsageCeiling || fx.mut.calls != 2 {
			t.Fatalf("Run = %q, %v after %d calls", reason, err, fx.mut.calls)
		}
	})
	t.Run("token ceiling", func(t *testing.T) {
		fx := newLoopFixture(t, 10, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return &driver.Mutation{Files: parent, Usage: lineage.Usage{NumTurns: 1, InputTokens: 1, OutputTokens: 10, CacheReadTokens: 100, CacheCreationTokens: 1000}}, nil
		})
		fx.cfg.TokenCeiling = 3333 // reached on the third call only if every kind counts
		fx.win.score["0"] = 0.5
		reason, err := fx.start(t)
		if err != nil || reason != StopUsageCeiling || fx.mut.calls != 3 {
			t.Fatalf("Run = %q, %v after %d calls", reason, err, fx.mut.calls)
		}
	})
	t.Run("gate fault", func(t *testing.T) {
		fx := newLoopFixture(t, 3, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, "fault", 1), nil
		})
		fx.win.score["0"] = 0.5
		reason, err := fx.start(t)
		if err == nil || reason != StopFault || !strings.Contains(err.Error(), "gate fault, generation 1") {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if s := fx.summary(t); s.Reason != StopFault || s.Generations != 1 || s.Usage.NumTurns != 1 {
			t.Errorf("summary = %+v", s)
		}
		if _, ok := fx.attempts(t)["1"]; ok {
			t.Error("a gate fault was recorded as the candidate's outcome")
		}
	})
	t.Run("driver fault", func(t *testing.T) {
		fx := newLoopFixture(t, 3, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return nil, errors.New("driver: claude binary changed since the driver started")
		})
		fx.win.score["0"] = 0.5
		reason, err := fx.start(t)
		if err == nil || reason != StopFault || fx.mut.calls != 1 {
			t.Fatalf("Run = %q, %v after %d calls", reason, err, fx.mut.calls)
		}
		if _, ok := fx.attempts(t)["1"]; ok {
			t.Error("a harness fault was recorded as the session's outcome")
		}
	})
	t.Run("generation 0 fails", func(t *testing.T) {
		fx := newLoopFixture(t, 3, nil)
		fx.cfg.Gen0 = map[string][]byte{"hasher.go": []byte("h"), "server.go": []byte("fail-local")}
		reason, err := fx.start(t)
		if err == nil || reason != StopFault {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if a := fx.attempts(t)["0"]; a.Gate.FailStage != 2 || a.Action != lineage.ActionDiscarded {
			t.Errorf("attempt 0 = %+v", a)
		}
	})
	t.Run("pairs fault", func(t *testing.T) {
		fx := newLoopFixture(t, 3, nil)
		fx.win.batchErr = errors.New("hashcore: out of memory")
		reason, err := fx.start(t)
		if err == nil || reason != StopFault || fx.win.windows != 0 {
			t.Fatalf("Run = %q, %v", reason, err)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fx := newLoopFixture(t, 3, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			cancel()
			return nil, fmt.Errorf("driver: %w", context.Canceled)
		})
		fx.win.score["0"] = 0.5
		l, err := NewLoop(fx.cfg)
		if err != nil {
			t.Fatal(err)
		}
		reason, err := l.Run(ctx)
		if !errors.Is(err, context.Canceled) || reason != StopInterrupted {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if _, ok := fx.attempts(t)["1"]; ok {
			t.Error("an interrupted mutation was recorded")
		}
		if s := fx.summary(t); s.Reason != StopInterrupted {
			t.Errorf("summary = %+v", s)
		}
	})
}

// TestSelectParent: the tournament draws two distinct genomes from the
// scored, eligible archive and takes the better, so the worst of three
// never wins and each of the others does; unscored, disqualified and
// gate-failed genomes are never drawn.
func TestSelectParent(t *testing.T) {
	l := &Loop{c: LoopConfig{Rand: rand.New(rand.NewPCG(3, 4))}, records: map[string]lineage.Attempt{}}
	if _, ok := l.selectParent(); ok {
		t.Fatal("a parent from an empty archive")
	}
	fit := func(s float64) *lineage.Fitness {
		return &lineage.Fitness{Score: s, Baseline: &lineage.Half{}}
	}
	l.records["0"] = lineage.Attempt{GenerationID: "0", Gate: lineage.Gate{Passed: true}, Fitness: fit(0.2)}
	l.records["7"] = lineage.Attempt{GenerationID: "7", Gate: lineage.Gate{Passed: true}}
	l.records["8"] = lineage.Attempt{GenerationID: "8", Gate: lineage.Gate{Passed: true}, Fitness: fit(0.99), Disqualified: "salt_reused"}
	l.records["9"] = lineage.Attempt{GenerationID: "9", Gate: lineage.Gate{FailStage: 2, FailCheck: "contract"}}
	if p, ok := l.selectParent(); !ok || p != "0" {
		t.Fatalf("one eligible: %q, %v", p, ok)
	}
	l.records["10"] = lineage.Attempt{GenerationID: "10", Gate: lineage.Gate{Passed: true}, Fitness: fit(0.5)}
	l.records["2"] = lineage.Attempt{GenerationID: "2", Gate: lineage.Gate{Passed: true}, Fitness: fit(0.4)}
	wins := map[string]int{}
	for range 300 {
		p, _ := l.selectParent()
		wins[p]++
	}
	if wins["0"] != 0 || wins["10"] == 0 || wins["2"] == 0 || len(wins) != 2 {
		t.Errorf("wins = %v", wins)
	}
	// About 2/3 for the best (it wins every draw it is in).
	if wins["10"] < 150 || wins["10"] > 250 {
		t.Errorf("the best won %d of 300", wins["10"])
	}
}

func TestCmpID(t *testing.T) {
	ids := []string{"10", "9", "100", "0", "11"}
	slices.SortFunc(ids, cmpID)
	if !slices.Equal(ids, []string{"0", "9", "10", "11", "100"}) {
		t.Errorf("sorted = %q", ids)
	}
}

func TestNewLoopRefusals(t *testing.T) {
	fx := newLoopFixture(t, 1, nil)
	for name, f := range map[string]func(c *LoopConfig){
		"no run":       func(c *LoopConfig) { c.Run = nil },
		"no mutator":   func(c *LoopConfig) { c.Mutator = nil },
		"no gen 0":     func(c *LoopConfig) { c.Gen0 = nil },
		"no ceiling":   func(c *LoopConfig) { c.TurnCeiling = 0 },
		"relative dir": func(c *LoopConfig) { c.ScratchDir = "scratch" },
		"bad params":   func(c *LoopConfig) { c.Params.WeightCPU = 2 },
	} {
		c := fx.cfg
		f(&c)
		if _, err := NewLoop(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	a := lineage.Attempt{GenerationID: "0", Time: time.Unix(1, 0), Gate: lineage.Gate{FailStage: 1, FailCheck: "build"},
		Action: lineage.ActionDiscarded, SelectionMode: lineage.ModeTournament}
	if err := fx.run.AppendAttempt(a); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoop(fx.cfg); err == nil {
		t.Error("a run directory with attempts was accepted")
	}
}
