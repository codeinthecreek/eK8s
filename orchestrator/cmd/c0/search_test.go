package main

import (
	"errors"
	"slices"
	"testing"
)

// call is one Step call: the rate and the attempt.
type call struct{ r, attempt int }

// script answers Step: pass(r) for a valid window; invalid lists the
// (rate, attempt) pairs that come back invalid; byCall overrides pass for
// the n-th call (1-based) of a rate.
type script struct {
	pass    func(r int) bool
	invalid map[call]bool
	byCall  map[call]bool // keyed by {rate, nth call of that rate}
	calls   []call
	seen    map[int]int
}

func (s *script) step(r, attempt int) (Obs, error) {
	s.calls = append(s.calls, call{r, attempt})
	if s.seen == nil {
		s.seen = make(map[int]int)
	}
	s.seen[r]++
	if s.invalid[call{r, attempt}] {
		return Obs{}, nil
	}
	if p, ok := s.byCall[call{r, s.seen[r]}]; ok {
		return Obs{Valid: true, Pass: p}, nil
	}
	return Obs{Valid: true, Pass: s.pass(r)}, nil
}

func below(t int) func(int) bool { return func(r int) bool { return r <= t } }

var defaultLadder = []int{6, 9, 12, 15, 18, 19}

func rates(cs []call) []int {
	var out []int
	for _, c := range cs {
		out = append(out, c.r)
	}
	return out
}

func TestSearch(t *testing.T) {
	tests := []struct {
		name    string
		ladder  []int
		confirm int
		s       script
		want    Verdict
		rates   []int // the rates asked, in order
	}{
		{
			name: "monotone threshold bisects to the boundary", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(13)},
			want:  Verdict{Kind: Resolved, C0: 13, Pass: 13, Fail: 14},
			rates: []int{6, 9, 12, 15, 13, 14, 13, 13},
		},
		{
			name: "all pass is C0 at least the top rate", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(100)},
			want:  Verdict{Kind: AtLeast, C0: 19, Pass: 19},
			rates: []int{6, 9, 12, 15, 18, 19, 19, 19},
		},
		{
			name: "a top-rate confirm window that fails leaves C0 unresolved", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(100), byCall: map[call]bool{{19, 3}: false}},
			want:  Verdict{Kind: Unresolved, Rate: 19, Pass: 18},
			rates: []int{6, 9, 12, 15, 18, 19, 19, 19},
		},
		{
			name: "first rate fails halts", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(5)},
			want:  Verdict{Kind: LadderTooHigh, Fail: 6},
			rates: []int{6},
		},
		{
			name: "fail right after a pass needs no bisection", ladder: []int{6, 7}, confirm: 1,
			s:     script{pass: below(6)},
			want:  Verdict{Kind: Resolved, C0: 6, Pass: 6, Fail: 7},
			rates: []int{6, 7, 6},
		},
		{
			name: "invalid then valid reruns once", ladder: defaultLadder, confirm: 0,
			s: script{pass: below(10), invalid: map[call]bool{{9, 1}: true}},
			// 9 invalid, 9 valid pass, 12 fail, bisect 10 pass, 11 fail.
			want:  Verdict{Kind: Resolved, C0: 10, Pass: 10, Fail: 11},
			rates: []int{6, 9, 9, 12, 10, 11},
		},
		{
			name: "invalid twice halts", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(100), invalid: map[call]bool{{12, 1}: true, {12, 2}: true}},
			want:  Verdict{Kind: InvalidTwice, Rate: 12, Pass: 9},
			rates: []int{6, 9, 12, 12},
		},
		{
			name: "invalid twice while bisecting keeps the bounds", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(13), invalid: map[call]bool{{13, 1}: true, {13, 2}: true}},
			want:  Verdict{Kind: InvalidTwice, Rate: 13, Pass: 12, Fail: 15},
			rates: []int{6, 9, 12, 15, 13, 13},
		},
		{
			name: "a confirm window that fails leaves C0 unresolved", ladder: defaultLadder, confirm: 2,
			s:     script{pass: below(13), byCall: map[call]bool{{13, 3}: false}},
			want:  Verdict{Kind: Unresolved, Rate: 13, Pass: 13, Fail: 14},
			rates: []int{6, 9, 12, 15, 13, 14, 13, 13},
		},
		{
			name: "the first confirm window failing stops at once", ladder: defaultLadder, confirm: 3,
			s:     script{pass: below(13), byCall: map[call]bool{{13, 2}: false}},
			want:  Verdict{Kind: Unresolved, Rate: 13, Pass: 13, Fail: 14},
			rates: []int{6, 9, 12, 15, 13, 14, 13},
		},
		{
			name: "non-monotone data is taken at its word", ladder: defaultLadder, confirm: 1,
			// 6 passes, 9 fails, 7 passes, 8 fails; 12 would pass but is never run.
			s:     script{pass: func(r int) bool { return r == 6 || r == 7 || r >= 12 }},
			want:  Verdict{Kind: Resolved, C0: 7, Pass: 7, Fail: 8},
			rates: []int{6, 9, 7, 8, 7},
		},
		{
			name: "non-monotone data above the boundary is caught by the confirm window", ladder: defaultLadder, confirm: 1,
			// 12 passes by luck once; 15 fails; 13 fails; so L = 12 and F = 13;
			// the confirm at 12 fails.
			s:     script{pass: below(11), byCall: map[call]bool{{12, 1}: true}},
			want:  Verdict{Kind: Unresolved, Rate: 12, Pass: 12, Fail: 13},
			rates: []int{6, 9, 12, 15, 13, 12},
		},
		{
			name: "no confirm windows", ladder: defaultLadder, confirm: 0,
			s:     script{pass: below(13)},
			want:  Verdict{Kind: Resolved, C0: 13, Pass: 13, Fail: 14},
			rates: []int{6, 9, 12, 15, 13, 14},
		},
		{
			name: "a one-rate ladder that passes", ladder: []int{19}, confirm: 2,
			s:     script{pass: below(19)},
			want:  Verdict{Kind: AtLeast, C0: 19, Pass: 19},
			rates: []int{19, 19, 19},
		},
		{
			name: "a one-rate ladder whose confirm window fails", ladder: []int{19}, confirm: 2,
			s:     script{pass: below(100), byCall: map[call]bool{{19, 2}: false}},
			want:  Verdict{Kind: Unresolved, Rate: 19},
			rates: []int{19, 19},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Search(tt.ladder, tt.confirm, tt.s.step)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("verdict %+v, want %+v", got, tt.want)
			}
			if r := rates(tt.s.calls); !slices.Equal(r, tt.rates) {
				t.Errorf("rates asked %v, want %v", r, tt.rates)
			}
			if got.Halted() != (got.Kind != Resolved && got.Kind != AtLeast) {
				t.Errorf("Halted() = %v for %s", got.Halted(), got.Kind)
			}
		})
	}
}

// TestSearchInvalidConfirm: an invalid window in the confirm phase is
// rerun like any other. 13 is asked first by the bisection (valid), then
// invalid once in its confirm (rerun valid), then passes.
func TestSearchInvalidConfirm(t *testing.T) {
	var s script
	n := 0
	step := func(r, attempt int) (Obs, error) {
		s.calls = append(s.calls, call{r, attempt})
		if r == 13 {
			n++
			if n == 2 && attempt == 1 { // first confirm window, first try
				return Obs{}, nil
			}
		}
		return Obs{Valid: true, Pass: r <= 13}, nil
	}
	got, err := Search(defaultLadder, 1, step)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Verdict{Kind: Resolved, C0: 13, Pass: 13, Fail: 14}); got != want {
		t.Errorf("verdict %+v, want %+v", got, want)
	}
	want := []call{{6, 1}, {9, 1}, {12, 1}, {15, 1}, {13, 1}, {14, 1}, {13, 1}, {13, 2}}
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls %v, want %v", s.calls, want)
	}
}

// TestSearchInvalidTwiceInConfirm: both tries of a confirm window invalid
// halts, with the bounds the bisection found.
func TestSearchInvalidTwiceInConfirm(t *testing.T) {
	n := 0
	got, err := Search(defaultLadder, 2, func(r, attempt int) (Obs, error) {
		if r == 13 {
			n++
			if n >= 2 { // the first confirm window, both tries
				return Obs{}, nil
			}
		}
		return Obs{Valid: true, Pass: r <= 13}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Verdict{Kind: InvalidTwice, Rate: 13, Pass: 13, Fail: 14}); got != want {
		t.Errorf("verdict %+v, want %+v", got, want)
	}
}

// TestSearchInvalidTwiceInTopConfirm: the ladder's own window at the top
// rate is valid; the confirm window there is invalid on both tries.
func TestSearchInvalidTwiceInTopConfirm(t *testing.T) {
	n := 0
	got, err := Search(defaultLadder, 1, func(r, attempt int) (Obs, error) {
		if r == 19 {
			n++
			if n >= 2 {
				return Obs{}, nil
			}
		}
		return Obs{Valid: true, Pass: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Verdict{Kind: InvalidTwice, Rate: 19, Pass: 18}); got != want {
		t.Errorf("verdict %+v, want %+v", got, want)
	}
}

func TestSearchStepError(t *testing.T) {
	boom := errors.New("disqualified")
	calls := 0
	_, err := Search(defaultLadder, 2, func(r, attempt int) (Obs, error) {
		calls++
		if r == 9 {
			return Obs{}, boom
		}
		return Obs{Valid: true, Pass: true}, nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("error %v, want %v", err, boom)
	}
	if calls != 2 {
		t.Errorf("%d calls, want 2: an error halts at once", calls)
	}
}

func TestSearchBadInput(t *testing.T) {
	step := func(r, attempt int) (Obs, error) { return Obs{Valid: true, Pass: true}, nil }
	for name, tt := range map[string]struct {
		ladder  []int
		confirm int
	}{
		"empty":       {nil, 2},
		"unsorted":    {[]int{6, 12, 9}, 2},
		"repeated":    {[]int{6, 6}, 2},
		"zero rate":   {[]int{0, 6}, 2},
		"negative":    {[]int{-1, 6}, 2},
		"bad confirm": {defaultLadder, -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Search(tt.ladder, tt.confirm, step); err == nil {
				t.Error("no error")
			}
		})
	}
}
