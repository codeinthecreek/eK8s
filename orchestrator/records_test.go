package orchestrator

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

func TestCheckRecords(t *testing.T) {
	clean := func() []wire.Record {
		return []wire.Record{
			{Seq: 10, Target: "1", RunID: "w", Index: 1, Kind: kindVerify, Bucket: "success"},
			{Seq: 11, Target: "1", RunID: "w", Index: 0, Kind: kindHash, Bucket: "success", Pair: &wire.Pair{}},
			{Seq: 12, Target: "1", RunID: "w", Index: 2, Kind: kindNegative, Bucket: "graceful_shed"},
		}
	}
	if err := checkRecords(clean(), "1", "w", 3, true); err != nil {
		t.Fatalf("clean records: %v", err)
	}
	if err := checkRecords(clean()[:2], "1", "w", 3, false); err != nil {
		t.Errorf("an incomplete run's records: %v", err)
	}
	skipped := clean()
	skipped[0] = wire.Record{Seq: 10, Target: "1", RunID: "w", Index: 1, Kind: kindVerify, Skipped: true}
	if err := checkRecords(skipped, "1", "w", 3, true); err != nil {
		t.Errorf("a skipped record is the exclusion's, not a fault: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(r []wire.Record) []wire.Record
		want string
	}{
		{"short", func(r []wire.Record) []wire.Record { return r[:2] }, "2 records, want 3"},
		{"other target", func(r []wire.Record) []wire.Record { r[0].Target = "2"; return r }, "another target"},
		{"other run", func(r []wire.Record) []wire.Record { r[0].RunID = "x"; return r }, "another target or run"},
		{"index out of range", func(r []wire.Record) []wire.Record { r[0].Index = 3; return r }, "index 3, of 3"},
		{"index twice", func(r []wire.Record) []wire.Record { r[0].Index = 0; return r }, "index 0 is recorded twice"},
		{"pair on a verify", func(r []wire.Record) []wire.Record { r[0].Pair = &wire.Pair{}; return r }, "pair on a"},
		{"unknown bucket", func(r []wire.Record) []wire.Record { r[0].Bucket = "fine"; return r }, `bucket "fine"`},
		{"success hash, no pair", func(r []wire.Record) []wire.Record { r[1].Pair = nil; return r }, "no pair"},
	} {
		if err := checkRecords(tc.edit(clean()), "1", "w", 3, true); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadgenExclusions(t *testing.T) {
	s := &Session{t: timing{}.withDefaults()}
	lagged := func(lags ...time.Duration) []wire.Record {
		var out []wire.Record
		for i, l := range lags {
			out = append(out, wire.Record{Index: i, IntendedMonoNS: 1000, ActualMonoNS: 1000 + int64(l)})
		}
		return out
	}
	ms := time.Millisecond
	many := func(n int, l time.Duration) []time.Duration { return slices.Repeat([]time.Duration{l}, n) }
	for _, tc := range []struct {
		name string
		recs map[string][]wire.Record
		want []string
	}{
		{"clean", map[string][]wire.Record{"1": lagged(many(200, ms)...)}, nil},
		// 2 of 200 over 10 ms: the p99 (rank 198) is still 1 ms.
		{"p99 under", map[string][]wire.Record{"1": lagged(append(many(198, ms), 50*ms, 50*ms)...)}, nil},
		// 3 of 200: rank 198 is over.
		{"p99 over", map[string][]wire.Record{"1": lagged(append(many(197, ms), 50*ms, 50*ms, 50*ms)...)}, []string{ExcludedLagP99}},
		{"max over", map[string][]wire.Record{"1": lagged(append(many(199, ms), 101*ms)...)}, []string{ExcludedLagMax}},
		// Lag is loadgen-wide: one target's lag excludes for all.
		{"across targets", map[string][]wire.Record{"1": lagged(many(100, ms)...), "2": lagged(many(100, 20*ms)...)}, []string{ExcludedLagP99}},
		{"skipped and fault", map[string][]wire.Record{"1": {{Skipped: true}, {Fault: "boom"}, {Skipped: true}}}, []string{ExcludedSkipped, ExcludedFault}},
	} {
		got := s.loadgenExclusions(tc.recs)
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunStates(t *testing.T) {
	run := wire.Run{Kind: wire.KindWindow, Targets: map[string]wire.RunTarget{"1": {State: "done"}, "2": {State: "cancelled"}}}
	if st, err := runStates(run, []string{"1", "2"}); err != nil || st["2"] != "cancelled" {
		t.Errorf("runStates = %v, %v", st, err)
	}
	for _, bad := range []struct {
		run  wire.Run
		live []string
	}{
		{wire.Run{Kind: wire.KindBurst, Targets: run.Targets}, []string{"1", "2"}},
		{run, []string{"1"}},
		{run, []string{"1", "3"}},
		{wire.Run{Kind: wire.KindWindow, Targets: map[string]wire.RunTarget{"1": {State: "paused"}}}, []string{"1"}},
	} {
		if _, err := runStates(bad.run, bad.live); err == nil {
			t.Errorf("runStates(%+v, %v): want an error", bad.run, bad.live)
		}
	}
}

func TestGenerateBatches(t *testing.T) {
	bs, err := GenerateBatches(t.Context(), hashcore.New(), 2, 5, 0)
	if err != nil || len(bs) != 2 {
		t.Fatalf("GenerateBatches = %d, %v", len(bs), err)
	}
	for i, b := range bs {
		up := b.upload()
		matches := 0
		for _, p := range up.Pairs {
			if *p.Match {
				matches++
			}
			if m, ok := b.truth[pairKey{*p.Password, *p.Hash}]; !ok || m != *p.Match {
				t.Errorf("batch %d: pair without its truth", i)
			}
		}
		if b.Len() != 5 || len(up.Pairs) != 5 || matches != 3 {
			t.Errorf("batch %d: %d pairs, %d matches; want 5, 3", i, b.Len(), matches)
		}
	}
	if _, err := GenerateBatches(t.Context(), hashcore.New(), 0, 5, 0); err == nil {
		t.Error("no batches: want an error")
	}
}
