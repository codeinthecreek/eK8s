package driver

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/lineage"
)

func testParams() fitness.Params { return fitness.DefaultParams() }

var digest = strings.Repeat("a", 64)

// gen0Attempt is a scored generation 0: latency is its weakest component.
func gen0Attempt() lineage.Attempt {
	return lineage.Attempt{
		GenerationID: "0", Time: time.Unix(1, 0), Gate: lineage.Gate{Passed: true}, GenomeSHA256: digest,
		Fitness: &lineage.Fitness{Score: 0.6, Baseline: &lineage.Half{SuccessRate: 0.98, P95LatencyMS: 1500, CPUPerSuccess: 0.1}},
		Action:  lineage.ActionPromoted, SelectionMode: lineage.ModeTournament, EverLive: true, CurrentlyLive: true,
	}
}

func scored(id, parent string, score float64, note string) lineage.Attempt {
	a := gen0Attempt()
	a.GenerationID, a.ParentID, a.Rationale = id, parent, note
	a.Fitness = &lineage.Fitness{Score: score, Baseline: &lineage.Half{SuccessRate: 0.99, P95LatencyMS: 300, CPUPerSuccess: 0.4}}
	return a
}

func failed(id, parent string, stage int, check, note string) lineage.Attempt {
	return lineage.Attempt{
		GenerationID: id, ParentID: parent, Time: time.Unix(2, 0),
		Gate:   lineage.Gate{FailStage: stage, FailCheck: check},
		Action: lineage.ActionDiscarded, SelectionMode: lineage.ModeTournament, Rationale: note,
	}
}

func ids(as []lineage.Attempt) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.GenerationID)
	}
	return out
}

// TestRecent: the lineage is the attempts made from the parent or any
// ancestor, the one that made the parent included, in file order; an
// attempt from another line (5, from 9) is not in 3's.
func TestRecent(t *testing.T) {
	// Lines: 0 -> 1 -> 3, and 0 -> 9 -> 5. 2, 4 and 7 failed the gate.
	all := []lineage.Attempt{
		gen0Attempt(),
		scored("1", "0", 0.6, "one"),
		failed("2", "0", 1, "imports", "two"),
		scored("3", "1", 0.7, "three"),
		failed("4", "1", 4, "race-report", "four"),
		scored("9", "0", 0.5, "nine"),
		scored("5", "9", 0.4, "five"),
		failed("7", "3", 2, "contract", "seven"),
	}
	for _, tc := range []struct {
		parent string
		n      int
		want   []string
	}{
		{"0", 3, []string{"1", "2", "9"}},
		{"3", 3, []string{"4", "9", "7"}},
		{"3", 10, []string{"1", "2", "3", "4", "9", "7"}},
		{"9", 10, []string{"1", "2", "9", "5"}},
		{"3", 0, nil},
	} {
		got, err := Recent(all, tc.parent, tc.n)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), tc.want) {
			t.Errorf("Recent(%s, %d) = %q, want %q", tc.parent, tc.n, ids(got), tc.want)
		}
	}
	if _, err := Recent(all, "42", 3); err == nil {
		t.Error("missing parent passed")
	}
	if _, err := Recent([]lineage.Attempt{scored("1", "8", 0.5, "")}, "1", 3); err == nil {
		t.Error("missing ancestor passed")
	}
	if _, err := Recent([]lineage.Attempt{scored("1", "2", 0.5, ""), scored("2", "1", 0.5, "")}, "1", 3); err == nil {
		t.Error("a loop passed")
	}
}

// TestBuildPrompt: the prompt carries the non-negotiable line, the
// rules, the hang timeout, the parent's components with the weakest
// named, and the recent attempts with their outcomes and notes.
func TestBuildPrompt(t *testing.T) {
	all := []lineage.Attempt{
		gen0Attempt(),
		failed("1", "0", 1, "imports", "Tried\na \"quoted\" note."),
		scored("2", "0", 0.7, "Raised capacity."),
	}
	all[2].Action = lineage.ActionArchived
	p, err := BuildPrompt(all, "2", testParams())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		NonNegotiable,
		"hasher.go is the fixed core's calling interface and is pinned",
		"Only edits to the existing files are kept: a new file is ignored",
		"github.com/codeinthecreek/eK8s/internal/classifier.",
		"slower than " + classifier.HangTimeout.String(),
		"Parent: generation 2, fitness 0.700.",
		"- success_rate 0.990: normalized 0.990, weight 0.40.",
		"- p95_latency 300ms: normalized 0.872, weight 0.30. 50ms scores 1, 2s scores 0.",
		"- cpu_per_success 0.400 CPU-seconds: normalized 0.222, weight 0.30.",
		"Weakest component (largest weighted shortfall): cpu_per_success.",
		`- Generation 1 (from 0): failed the gate at stage 1 (build and boot), check imports. Note: "Tried a quoted note."`,
		`- Generation 2 (from 0): passed the gate and scored 0.700 (its parent 0.600); archived. Note: "Raised capacity."`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Stress half") {
		t.Error("a stress half with no stress record")
	}

	g, err := BuildPrompt(all[:1], "0", testParams())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- None yet.", "Weakest component (largest weighted shortfall): p95_latency."} {
		if !strings.Contains(g, want) {
			t.Errorf("gen-0 prompt lacks %q", want)
		}
	}
}

// TestBuildPromptHalves: a stress half is shown separately, an excluded
// half is named as such, and a disqualified attempt says so.
func TestBuildPromptHalves(t *testing.T) {
	a := gen0Attempt()
	a.Fitness.Stress = &lineage.Half{Excluded: true}
	dq := scored("1", "0", 0.9, "")
	dq.Action, dq.Disqualified, dq.EverLive, dq.CurrentlyLive = lineage.ActionArchived, "salt_reused", true, false
	p, err := BuildPrompt([]lineage.Attempt{a, dq}, "0", testParams())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Stress half (CPU contention): excluded, not scored.", "culled for an integrity failure (salt_reused)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

// TestBuildPromptRefusals: a parent that is missing, gate-failed,
// unscored or disqualified has no prompt, nor do invalid parameters.
func TestBuildPromptRefusals(t *testing.T) {
	unscored := gen0Attempt()
	unscored.Fitness = nil
	dq := gen0Attempt()
	dq.Disqualified, dq.Action, dq.CurrentlyLive = "wrong_match", lineage.ActionArchived, false
	for name, all := range map[string][]lineage.Attempt{
		"missing":      {scored("1", "0", 0.5, "")},
		"gate failed":  {gen0Attempt(), failed("1", "0", 1, "imports", "")},
		"unscored":     {unscored},
		"disqualified": {dq},
	} {
		parent := "1"
		if name == "unscored" || name == "disqualified" {
			parent = "0"
		}
		if _, err := BuildPrompt(all, parent, testParams()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	excluded := gen0Attempt()
	excluded.Fitness.Baseline.Excluded = true
	if _, err := BuildPrompt([]lineage.Attempt{excluded}, "0", testParams()); err == nil {
		t.Error("a parent with no scored half passed")
	}
	bad := testParams()
	bad.WeightCPU = 2
	if _, err := BuildPrompt([]lineage.Attempt{gen0Attempt()}, "0", bad); err == nil {
		t.Error("invalid params passed")
	}
}

// TestNote: a note is one line without quotes or control characters,
// cut to noteBytes.
func TestNote(t *testing.T) {
	if got := note("a\n\tb \"c\"\x00 \xff d"); got != "a b c d" {
		t.Errorf("note = %q", got)
	}
	if s := strings.Repeat("x", noteBytes); note(s) != s {
		t.Error("a note of noteBytes was cut")
	}
	long := note(strings.Repeat("é", noteBytes))
	if len(long) > noteBytes || len(long) < noteBytes-4 || !strings.HasSuffix(long, "...") {
		t.Errorf("long note is %d bytes", len(long))
	}
}
