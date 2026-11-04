package lineage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func burstStall(id string) Stall {
	return Stall{
		Kind: StallBurst, RunID: "gate-" + id + "-ek8s-0123456789abcdef", GenerationID: id,
		Records: 40, OHT: 6, StallRuns: 1, Longest: 6, LongestStartIndex: 3,
		LongestStartWall: time.Date(2026, 11, 4, 1, 2, 3, 4, time.FixedZone("x", 3600)), Threshold: 5,
	}
}

func halfStall(kind, id string) Stall {
	start := time.Date(2026, 11, 4, 2, 0, 0, 500, time.FixedZone("y", -7200))
	return Stall{
		Kind: kind, RunID: "w-00000000000000a" + id, WindowRunID: "w-0000000000000000", GenerationID: id,
		Records: 20, LongestStartIndex: -1, Threshold: 20,
		SpanStart: start, SpanEnd: start.Add(12 * time.Second),
	}
}

// readStalls decodes stalls.jsonl strictly, one Stall per line.
func readStalls(t *testing.T, r *Run) []Stall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, stallsFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []Stall
	for n, line := range bytes.SplitAfter(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var s Stall
		if err := strictDecode(line, &s); err != nil {
			t.Fatalf("line %d: %v: %s", n+1, err, line)
		}
		out = append(out, s)
	}
	return out
}

func TestStallRoundTrip(t *testing.T) {
	r := newRun(t)
	if _, err := os.Stat(filepath.Join(r.dir, stallsFile)); !os.IsNotExist(err) {
		t.Fatalf("a new run has %s: %v", stallsFile, err)
	}
	in := []Stall{burstStall("0"), halfStall(StallBaseline, "0"), halfStall(StallStress, "1")}
	for _, s := range in {
		if err := r.AppendStall(s); err != nil {
			t.Fatalf("AppendStall(%+v): %v", s, err)
		}
	}
	if m := mode(t, filepath.Join(r.dir, stallsFile)); m != 0o600 {
		t.Errorf("%s mode %v, want 0600", stallsFile, m)
	}
	got := readStalls(t, r)
	if len(got) != len(in) {
		t.Fatalf("%d lines, want %d", len(got), len(in))
	}
	for i := range in {
		want := in[i]
		want.LongestStartWall, want.SpanStart, want.SpanEnd = utc(want.LongestStartWall), utc(want.SpanStart), utc(want.SpanEnd)
		g := got[i]
		if g != want {
			t.Errorf("line %d = %+v, want %+v", i+1, g, want)
		}
		for _, tm := range []time.Time{g.LongestStartWall, g.SpanStart, g.SpanEnd} {
			if !tm.IsZero() && tm.Location() != time.UTC {
				t.Errorf("line %d: time %v is not UTC", i+1, tm)
			}
		}
	}

	b, err := os.ReadFile(filepath.Join(r.dir, stallsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for _, want := range []string{`"kind":"burst"`, `"longest_start_wall":"2026-11-04T00:02:03.000000004Z"`, `"longest_start_index":3`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("burst line %s lacks %s", lines[0], want)
		}
	}
	for _, absent := range []string{"window_run_id", "span_start", "span_end"} {
		if strings.Contains(lines[0], absent) {
			t.Errorf("burst line %s has %s", lines[0], absent)
		}
	}
	for _, want := range []string{`"window_run_id":"w-0000000000000000"`, `"span_start":"2026-11-04T04:00:00.0000005Z"`, `"span_end":"2026-11-04T04:00:12.0000005Z"`, `"longest_start_index":-1`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("baseline line %s lacks %s", lines[1], want)
		}
	}
	if strings.Contains(lines[1], "longest_start_wall") {
		t.Errorf("baseline line %s has a longest_start_wall with no OHT record", lines[1])
	}
	// The run's attempts are untouched.
	if a, err := r.Attempts(); err != nil || len(a) != 0 {
		t.Errorf("Attempts = %v, %v", a, err)
	}
}

func TestStallRefusals(t *testing.T) {
	r := newRun(t)
	for name, f := range map[string]func(*Stall){
		"kind":              func(s *Stall) { s.Kind = "window" },
		"empty kind":        func(s *Stall) { s.Kind = "" },
		"run_id":            func(s *Stall) { s.RunID = "../x" },
		"empty run_id":      func(s *Stall) { s.RunID = "" },
		"generation_id":     func(s *Stall) { s.GenerationID = "01" },
		"burst window id":   func(s *Stall) { s.WindowRunID = "w-0000000000000000" },
		"records":           func(s *Stall) { s.Records = -1 },
		"oht":               func(s *Stall) { s.OHT = -1 },
		"stall_runs":        func(s *Stall) { s.StallRuns = -1 },
		"tail_run":          func(s *Stall) { s.TailRun = -1 },
		"longest":           func(s *Stall) { s.Longest = -1 },
		"threshold":         func(s *Stall) { s.Threshold = -1 },
		"longest_start_idx": func(s *Stall) { s.LongestStartIndex = -2 },
	} {
		s := burstStall("1")
		f(&s)
		if err := r.AppendStall(s); err == nil {
			t.Errorf("%s: AppendStall(%+v) = nil", name, s)
		}
	}
	for name, f := range map[string]func(*Stall){
		"no window id":  func(s *Stall) { s.WindowRunID = "" },
		"bad window id": func(s *Stall) { s.WindowRunID = "w 1" },
	} {
		s := halfStall(StallStress, "1")
		f(&s)
		if err := r.AppendStall(s); err == nil {
			t.Errorf("%s: AppendStall(%+v) = nil", name, s)
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, stallsFile)); !os.IsNotExist(err) {
		t.Errorf("refused lines made %s: %v", stallsFile, err)
	}
}

// TestStallAppendOnly checks that an append keeps every earlier line as
// written, and that a torn tail refuses the next append.
func TestStallAppendOnly(t *testing.T) {
	r := newRun(t)
	if err := r.AppendStall(burstStall("1")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, stallsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AppendStall(halfStall(StallBaseline, "1")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) || bytes.Count(after, []byte("\n")) != 2 {
		t.Errorf("second append changed the first line or did not add one:\n%s\nthen\n%s", before, after)
	}
	// A run reopened later appends to the same file.
	r2, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.AppendStall(halfStall(StallStress, "1")); err != nil {
		t.Fatal(err)
	}
	if n := len(readStalls(t, r2)); n != 3 {
		t.Errorf("%d lines after reopening, want 3", n)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"kind":"burst"`)
	f.Close()
	if err := r.AppendStall(burstStall("2")); err == nil || !strings.Contains(err.Error(), "torn") {
		t.Errorf("AppendStall after a torn tail = %v", err)
	}
	// A torn stalls.jsonl is not the attempts file's concern.
	if err := r.AppendAttempt(passed("2", "")); err != nil {
		t.Errorf("AppendAttempt with a torn stalls.jsonl = %v", err)
	}
}
