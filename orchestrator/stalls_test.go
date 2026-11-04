package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/driver"
	"github.com/codeinthecreek/eK8s/internal/stallrun"
	"github.com/codeinthecreek/eK8s/lineage"
)

var spanT0 = time.Date(2026, 11, 4, 4, 0, 0, 0, time.FixedZone("x", 3600))

// stallWindow is scoredWindow with each half's stall counts and span
// set, the stress half's counts naming a stall run.
func stallWindow(n int, live []string, score map[string]float64) WindowResult {
	res := scoredWindow(n, live, score)
	res.StressRunID = fmt.Sprintf("w-%ds", n)
	res.BaselineSpan = Span{spanT0, spanT0.Add(10 * time.Second)}
	res.StressSpan = Span{spanT0.Add(11 * time.Second), spanT0.Add(21 * time.Second)}
	for _, id := range live {
		vr := res.Variants[id]
		vr.BaselineStall = &stallrun.Stats{Records: 20, LongestStartIndex: -1, Threshold: 20}
		vr.StressStall = &stallrun.Stats{Records: 20, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 3, LongestStartWall: spanT0.Add(12 * time.Second), Threshold: 4}
		res.Variants[id] = vr
	}
	return res
}

// readStallLines decodes the run's stalls.jsonl strictly.
func readStallLines(t *testing.T, run *lineage.Run) []lineage.Stall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(run.Dir(), "stalls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []lineage.Stall
	for _, line := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var s lineage.Stall
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("stalls.jsonl line %s: %v", line, err)
		}
		out = append(out, s)
	}
	return out
}

func stallKey(s lineage.Stall) string {
	return strings.Join([]string{s.Kind, s.RunID, s.WindowRunID, s.GenerationID}, " ")
}

// decisions is what a run decided: why it stopped and each attempt's
// gate, action, live flags, fitness and reason, its time left out.
func decisions(t *testing.T, fx *loopFixture, reason string) string {
	t.Helper()
	all, err := fx.run.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "stop %s\n", reason)
	for _, a := range all {
		a.Time = time.Time{}
		j, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\n", j)
	}
	return b.String()
}

// TestLoopStalls: the burst line is written after a passing and a
// failing Stage 5; each window writes one line per variant per half,
// baseline first; and the counts change nothing the loop decides, nor
// does a failed write, which is one log line while the run goes on.
func TestLoopStalls(t *testing.T) {
	run := func(t *testing.T, stalls bool, breakFile bool) (*loopFixture, string, string) {
		fx := newLoopFixture(t, 2, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, fmt.Sprintf("gen%d", n), 3), nil
		})
		fx.win.score = map[string]float64{"0": 0.5, "1": 0.7}
		failStage5["2"] = true
		if stalls {
			fx.win.window = func(n int, live []string) WindowResult { return stallWindow(n, live, fx.win.score) }
		}
		if breakFile {
			// A directory where stalls.jsonl goes: every append fails.
			if err := os.Mkdir(filepath.Join(fx.run.Dir(), "stalls.jsonl"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		var log bytes.Buffer
		fx.cfg.Log = &log
		reason, err := fx.start(t)
		if err != nil || reason != StopGenerations {
			t.Fatalf("Run = %q, %v\n%s", reason, err, log.String())
		}
		return fx, decisions(t, fx, reason), log.String()
	}

	fx, withStalls, log := run(t, true, false)
	got := readStallLines(t, fx.run)
	var keys []string
	for _, s := range got {
		keys = append(keys, stallKey(s))
	}
	want := []string{
		"burst gate-0-ek8s-0123456789abcdef  0",
		"baseline w-1 w-1 0", "stress w-1s w-1 0",
		"burst gate-1-ek8s-0123456789abcdef  1",
		"baseline w-2 w-2 0", "baseline w-2 w-2 1", "stress w-2s w-2 0", "stress w-2s w-2 1",
		"burst gate-2-ek8s-0123456789abcdef  2", // Stage 5 failed: still recorded
	}
	if !slices.Equal(keys, want) {
		t.Errorf("stalls.jsonl lines\n%s\nwant\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(log, "stalls:") {
		t.Errorf("log reports a stall write error:\n%s", log)
	}
	wantBurst := lineage.Stall{
		Kind: lineage.StallBurst, RunID: "gate-0-ek8s-0123456789abcdef", GenerationID: "0",
		Records: 30, OHT: 2, TailRun: 2, Longest: 2, LongestStartIndex: 28, LongestStartWall: burstWall, Threshold: 60,
	}
	wantStress := lineage.Stall{
		Kind: lineage.StallStress, RunID: "w-2s", WindowRunID: "w-2", GenerationID: "1",
		Records: 20, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 3, LongestStartWall: spanT0.Add(12 * time.Second).UTC(), Threshold: 4,
		SpanStart: spanT0.Add(11 * time.Second).UTC(), SpanEnd: spanT0.Add(21 * time.Second).UTC(),
	}
	wantBase := lineage.Stall{
		Kind: lineage.StallBaseline, RunID: "w-1", WindowRunID: "w-1", GenerationID: "0",
		Records: 20, LongestStartIndex: -1, Threshold: 20, SpanStart: spanT0.UTC(), SpanEnd: spanT0.Add(10 * time.Second).UTC(),
	}
	for _, c := range []struct {
		i    int
		want lineage.Stall
	}{{0, wantBurst}, {1, wantBase}, {7, wantStress}} {
		if c.i >= len(got) || !stallEqual(got[c.i], c.want) {
			t.Errorf("line %d = %+v, want %+v", c.i+1, got[min(c.i, len(got)-1)], c.want)
		}
	}

	// The same run with no counts in the windows, and with every write
	// failing, decides the same.
	_, without, _ := run(t, false, false)
	if without != withStalls {
		t.Errorf("decisions with window stall counts\n%s\ndiffer from without\n%s", withStalls, without)
	}
	_, broken, blog := run(t, true, true)
	if broken != withStalls {
		t.Errorf("decisions with failing stall writes\n%s\ndiffer from\n%s", broken, withStalls)
	}
	if n := strings.Count(blog, " stalls: "); n != len(want) {
		t.Errorf("%d stall write errors logged, want %d:\n%s", n, len(want), blog)
	}
	for _, line := range strings.Split(strings.TrimSpace(blog), "\n") {
		if strings.Contains(line, " stalls: ") && !strings.Contains(line, "not recorded") {
			t.Errorf("stall error line %q", line)
		}
	}
}

// stallEqual compares two lines, their times by instant.
func stallEqual(a, b lineage.Stall) bool {
	ok := a.LongestStartWall.Equal(b.LongestStartWall) && a.SpanStart.Equal(b.SpanStart) && a.SpanEnd.Equal(b.SpanEnd)
	a.LongestStartWall, a.SpanStart, a.SpanEnd = time.Time{}, time.Time{}, time.Time{}
	b.LongestStartWall, b.SpanStart, b.SpanEnd = time.Time{}, time.Time{}, time.Time{}
	return ok && a == b
}

// TestLoopStallFault: a Stage 5 fault after the burst was read still
// writes the burst line, before the run halts.
func TestLoopStallFault(t *testing.T) {
	fx := newLoopFixture(t, 1, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
		return edit(parent, "gen1", 3), nil
	})
	fx.win.score = map[string]float64{"0": 0.5}
	faultStage5["1"] = true
	reason, err := fx.start(t)
	if reason != StopFault || err == nil || !strings.Contains(err.Error(), "stress-ng pod gone") {
		t.Fatalf("Run = %q, %v; want a Stage 5 fault", reason, err)
	}
	got := readStallLines(t, fx.run)
	if len(got) == 0 || stallKey(got[len(got)-1]) != "burst gate-1-ek8s-0123456789abcdef  1" {
		var keys []string
		for _, s := range got {
			keys = append(keys, stallKey(s))
		}
		t.Errorf("stalls.jsonl lines %q, want the last for generation 1's burst", keys)
	}
}

// TestLoopStallsUnscored: a window that is not scored still writes the
// lines of every half that ran: an excluded window both halves', and a
// window restarted in its stress half the baseline half's, before the
// run halts on the restart.
func TestLoopStallsUnscored(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window func(fx *loopFixture, n int, live []string) WindowResult
		stop   string
		want   []string
	}{
		{"excluded", func(fx *loopFixture, n int, live []string) WindowResult {
			res := stallWindow(n, live, fx.win.score)
			if n == 1 {
				res.Excluded, res.StressExcluded = []string{"stress-shed"}, []string{"stress-shed"}
			}
			return res
		}, StopGenerations, []string{
			"burst gate-0-ek8s-0123456789abcdef  0",
			"baseline w-1 w-1 0", "stress w-1s w-1 0", // excluded, recorded
			"baseline w-2 w-2 0", "stress w-2s w-2 0",
		}},
		{"restart_in_stress", func(fx *loopFixture, n int, live []string) WindowResult {
			res := stallWindow(n, live, fx.win.score)
			// The stress half met a new boot_id: the baseline half was
			// judged, the stress half has no counts.
			fx.win.restarted = true
			res.Excluded, res.StressRunID, res.StressSpan = []string{ExcludedRestart}, "", Span{}
			for id, vr := range res.Variants {
				vr.StressStall, vr.Baseline, vr.Stress = nil, nil, nil
				res.Variants[id] = vr
			}
			return res
		}, StopRestart, []string{
			"burst gate-0-ek8s-0123456789abcdef  0",
			"baseline w-1 w-1 0",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLoopFixture(t, 0, nil)
			fx.win.score = map[string]float64{"0": 0.5}
			fx.win.window = func(n int, live []string) WindowResult { return tc.window(fx, n, live) }
			reason, err := fx.start(t)
			if err != nil || reason != tc.stop {
				t.Fatalf("Run = %q, %v; want %q", reason, err, tc.stop)
			}
			var keys []string
			for _, s := range readStallLines(t, fx.run) {
				keys = append(keys, stallKey(s))
			}
			if !slices.Equal(keys, tc.want) {
				t.Errorf("stalls.jsonl lines\n%s\nwant\n%s", strings.Join(keys, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
