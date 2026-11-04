package stallrun

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// base is record 0's IntendedWall; record i's is i seconds later. It is
// not UTC, so the tests see Detect convert it.
var base = time.Date(2026, 11, 4, 12, 0, 0, 0, time.FixedZone("UTC+2", 2*3600))

func wall(i int) time.Time { return base.Add(time.Duration(i) * time.Second).UTC() }

// records builds one record per character of pattern, the character's
// position being its Index and its Seq: o an OHT record, s success,
// g graceful_shed, f failed with another reason, k Skipped, x a Fault,
// _ no record (a missing index).
func records(t *testing.T, pattern string) []wire.Record {
	t.Helper()
	var recs []wire.Record
	for i, c := range pattern {
		rec := wire.Record{Seq: uint64(i), Index: i, Kind: "verify", IntendedWall: base.Add(time.Duration(i) * time.Second)}
		switch c {
		case 'o':
			rec.Bucket, rec.Reason = classifier.Failed.String(), string(classifier.ReasonOverHangTimeout)
		case 's':
			rec.Bucket = classifier.Success.String()
		case 'g':
			rec.Bucket = classifier.GracefulShed.String()
		case 'f':
			rec.Bucket, rec.Reason = classifier.Failed.String(), string(classifier.ReasonTransportError)
		case 'k':
			rec.Skipped = true
		case 'x':
			rec.Fault = "send"
		case '_':
			continue
		default:
			t.Fatalf("pattern %q: unknown character %q", pattern, c)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestDetect(t *testing.T) {
	// rate 2 gives threshold ceil(2 x 2 s) = 4.
	tests := []struct {
		name    string
		pattern string
		rate    float64
		want    Stats
	}{
		{"empty input", "", 2,
			Stats{LongestStartIndex: -1, Threshold: 4}},
		{"no OHT record", "ssgfkx", 2,
			Stats{Records: 6, LongestStartIndex: -1, Threshold: 4}},
		{"a single OHT is a run of 1, no stall", "sos", 2,
			Stats{Records: 3, OHT: 1, Longest: 1, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"threshold - 1 is no stall", "sooos", 2,
			Stats{Records: 5, OHT: 3, Longest: 3, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"threshold is a stall", "soooos", 2,
			Stats{Records: 6, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"recovery makes a stall", "oooos", 2,
			Stats{Records: 5, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a run to the last index is the tail, not a stall", "soooo", 2,
			Stats{Records: 5, OHT: 4, TailRun: 4, Longest: 4, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"an all-OHT run is the tail", "oooooo", 2,
			Stats{Records: 6, OHT: 6, TailRun: 6, Longest: 6, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a short tail is still the tail", "sssso", 2,
			Stats{Records: 5, OHT: 1, TailRun: 1, Longest: 1, LongestStartIndex: 4, LongestStartWall: wall(4), Threshold: 4}},
		{"two stall runs, then a tail", "oooosgoooooso", 2,
			Stats{Records: 13, OHT: 10, StallRuns: 2, TailRun: 1, Longest: 5, LongestStartIndex: 6, LongestStartWall: wall(6), Threshold: 4}},
		{"a Skipped record breaks a run", "ookoos", 2,
			Stats{Records: 6, OHT: 4, Longest: 2, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a Fault record breaks a run", "ooxoos", 2,
			Stats{Records: 6, OHT: 4, Longest: 2, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"another failed reason breaks a run", "oofoos", 2,
			Stats{Records: 6, OHT: 4, Longest: 2, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"graceful_shed breaks a run", "oogoos", 2,
			Stats{Records: 6, OHT: 4, Longest: 2, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a missing index breaks a run", "oo_oos", 2,
			Stats{Records: 5, OHT: 4, Longest: 2, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a run ended by a missing index, then recovery, is a stall", "oooo_s", 2,
			Stats{Records: 5, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a run ended by a missing index with only OHT after is neither", "oooo_oo", 2,
			Stats{Records: 6, OHT: 6, TailRun: 2, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a later Skipped record is not recovery (D4): tail, not stall", "ooook", 2,
			Stats{Records: 5, OHT: 4, TailRun: 4, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a later Fault record is not recovery (D4): tail, not stall", "oooox", 2,
			Stats{Records: 5, OHT: 4, TailRun: 4, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a long OHT run, then only Skipped records, is the tail, not a stall", "s" + strings.Repeat("o", 10) + "kkkk", 2,
			Stats{Records: 15, OHT: 10, TailRun: 10, Longest: 10, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"Skipped and Fault records, then recovery, make the run a stall", "ooookxs", 2,
			Stats{Records: 7, OHT: 4, StallRuns: 1, Longest: 4, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"the tail is the last OHT run, short of the last index", "oooooookookk", 2,
			Stats{Records: 12, OHT: 9, TailRun: 2, Longest: 7, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"ties go to the earliest", "ooosooos", 2,
			Stats{Records: 8, OHT: 6, Longest: 3, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 4}},
		{"a tied tail loses to an earlier run", "sooosooo", 2,
			Stats{Records: 8, OHT: 6, TailRun: 3, Longest: 3, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 4}},
		{"fractional rate 15.5: 30 is no stall", "s" + strings.Repeat("o", 30) + "s", 15.5,
			Stats{Records: 32, OHT: 30, Longest: 30, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 31}},
		{"fractional rate 15.5: 31 is a stall", "s" + strings.Repeat("o", 31) + "s", 15.5,
			Stats{Records: 33, OHT: 31, StallRuns: 1, Longest: 31, LongestStartIndex: 1, LongestStartWall: wall(1), Threshold: 31}},
		{"rate 62: 123 is no stall", strings.Repeat("o", 123) + "s", 62,
			Stats{Records: 124, OHT: 123, Longest: 123, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 124}},
		{"rate 62: 124 is a stall", strings.Repeat("o", 124) + "s", 62,
			Stats{Records: 125, OHT: 124, StallRuns: 1, Longest: 124, LongestStartIndex: 0, LongestStartWall: wall(0), Threshold: 124}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(records(t, tt.pattern), tt.rate)
			if got != tt.want {
				t.Errorf("Detect(%q, %v)\n got  %+v\n want %+v", tt.pattern, tt.rate, got, tt.want)
			}
		})
	}
}

// TestDetectSeqOrder gives Detect the records in an order that is not
// Index order, as a window's arrive by Seq, and checks that it orders
// them by Index itself and leaves its input as it was.
func TestDetectSeqOrder(t *testing.T) {
	const pattern = "sooooso_oos"
	inOrder := records(t, pattern)
	want := Detect(inOrder, 2)
	if want.StallRuns != 1 || want.Longest != 4 || want.LongestStartIndex != 1 {
		t.Fatalf("in Index order: %+v, want 1 stall run, longest 4 from index 1", want)
	}
	shuffled := slices.Clone(inOrder)
	for i, j := range []int{7, 2, 9, 0, 5, 3, 8, 1, 6, 4} { // a fixed permutation of 0..9
		shuffled[i] = inOrder[j]
		shuffled[i].Seq = uint64(i) // Seq order is arrival order, not Index order
	}
	before := slices.Clone(shuffled)
	if got := Detect(shuffled, 2); got != want {
		t.Errorf("in Seq order\n got  %+v\n want %+v", got, want)
	}
	if !slices.EqualFunc(shuffled, before, func(a, b wire.Record) bool { return a.Seq == b.Seq && a.Index == b.Index }) {
		t.Errorf("Detect reordered its input")
	}
}

// TestDetectStartWallUTC checks that the longest run's start wall is its
// first record's IntendedWall, in UTC.
func TestDetectStartWallUTC(t *testing.T) {
	recs := records(t, "ssooos")
	recs[2].IntendedWall = time.Date(2026, 11, 4, 9, 30, 15, 123456789, time.FixedZone("UTC-5", -5*3600))
	got := Detect(recs, 2)
	want := time.Date(2026, 11, 4, 14, 30, 15, 123456789, time.UTC)
	if !got.LongestStartWall.Equal(want) || got.LongestStartWall.Location() != time.UTC {
		t.Errorf("LongestStartWall = %v, want %v (UTC)", got.LongestStartWall, want)
	}
}

func TestThreshold(t *testing.T) {
	tests := []struct {
		rate float64
		want int
	}{
		{15.5, 31}, {62, 124}, {11, 22}, {1.25, 3}, {0.1, 1},
		{0, 1}, {-3, 1}, {math.NaN(), 1}, {math.Inf(1), math.MaxInt32},
	}
	for _, tt := range tests {
		if got := Threshold(tt.rate); got != tt.want {
			t.Errorf("Threshold(%v) = %d, want %d", tt.rate, got, tt.want)
		}
	}
}
