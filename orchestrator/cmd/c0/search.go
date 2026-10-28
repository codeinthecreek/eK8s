package main

import (
	"errors"
	"fmt"
)

// Obs is what one measured window tells the search: whether the window
// counted (Valid) and, if it did, whether it passed the C0 rule. An
// invalid window is not measured: its Pass is ignored.
type Obs struct {
	Valid, Pass bool
}

// Step measures integer rate r. attempt is 1 for the first try at a
// window and 2 for its one rerun. A returned error is a fault or a
// disqualification and halts the search at once.
type Step func(r, attempt int) (Obs, error)

// Kind is how a search ended.
type Kind string

const (
	// Resolved: C0 = Verdict.C0, and the confirm windows at it all passed.
	Resolved Kind = "resolved"
	// AtLeast: every ladder rate passed, so C0 >= Verdict.C0 (the top
	// rate, normally loadgen's maxRate).
	AtLeast Kind = "at-least"
	// LadderTooHigh: the first ladder rate failed.
	LadderTooHigh Kind = "ladder-too-high"
	// InvalidTwice: a window was invalid on its rerun too.
	InvalidTwice Kind = "invalid-twice"
	// Unresolved: a confirm window at Verdict.Pass failed.
	Unresolved Kind = "unresolved"
)

// Verdict is a search's outcome. Pass is the last rate that passed and
// Fail the first rate above it that failed, where they are known.
type Verdict struct {
	Kind Kind
	// C0 is set for Resolved (= Pass) and AtLeast (the top ladder rate).
	C0 int
	// Pass and Fail bound C0 at the end of the walk and the bisection.
	Pass, Fail int
	// Rate is the rate whose windows were invalid twice (InvalidTwice) or
	// that failed its confirm window (Unresolved).
	Rate int
}

// Halted reports whether the search ended without a value for C0.
func (v Verdict) Halted() bool { return v.Kind != Resolved && v.Kind != AtLeast }

// Search walks ladder upward (strictly increasing, positive) until the
// first rate that fails. If that is the first, it halts (LadderTooHigh);
// if none fails, C0 >= the top rate (AtLeast). Otherwise it bisects the
// integer rates between the last pass L and the first fail F until
// F = L + 1, then runs confirm more windows at L, all of which must pass
// (else Unresolved; it does not keep searching). Rates are assumed
// monotone only as far as the bisection needs: a non-monotone step is
// taken at its word, and the confirm windows are what catch it.
//
// A window that is invalid is rerun once; a second invalid halts
// (InvalidTwice). A Step error is returned with the Verdict so far
// (Kind empty).
func Search(ladder []int, confirm int, step Step) (Verdict, error) {
	if len(ladder) == 0 {
		return Verdict{}, errors.New("c0: empty ladder")
	}
	for i, r := range ladder {
		if r < 1 || (i > 0 && r <= ladder[i-1]) {
			return Verdict{}, fmt.Errorf("c0: ladder %v is not positive and strictly increasing", ladder)
		}
	}
	if confirm < 0 {
		return Verdict{}, fmt.Errorf("c0: confirm %d", confirm)
	}

	// measure runs r, and its rerun if the first was invalid. ok is false
	// when both were invalid.
	measure := func(r int) (pass, ok bool, err error) {
		for attempt := 1; attempt <= 2; attempt++ {
			o, err := step(r, attempt)
			if err != nil {
				return false, false, err
			}
			if o.Valid {
				return o.Pass, true, nil
			}
		}
		return false, false, nil
	}

	var lo, hi int
	failed := false
	for i, r := range ladder {
		pass, ok, err := measure(r)
		if err != nil {
			return Verdict{}, err
		}
		if !ok {
			return Verdict{Kind: InvalidTwice, Rate: r, Pass: lo}, nil
		}
		if pass {
			lo = r
			continue
		}
		if i == 0 {
			return Verdict{Kind: LadderTooHigh, Fail: r}, nil
		}
		hi, failed = r, true
		break
	}
	if !failed {
		return Verdict{Kind: AtLeast, C0: lo, Pass: lo}, nil
	}

	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		pass, ok, err := measure(mid)
		if err != nil {
			return Verdict{}, err
		}
		if !ok {
			return Verdict{Kind: InvalidTwice, Rate: mid, Pass: lo, Fail: hi}, nil
		}
		if pass {
			lo = mid
		} else {
			hi = mid
		}
	}
	for range confirm {
		pass, ok, err := measure(lo)
		if err != nil {
			return Verdict{}, err
		}
		if !ok {
			return Verdict{Kind: InvalidTwice, Rate: lo, Pass: lo, Fail: hi}, nil
		}
		if !pass {
			return Verdict{Kind: Unresolved, Rate: lo, Pass: lo, Fail: hi}, nil
		}
	}
	return Verdict{Kind: Resolved, C0: lo, Pass: lo, Fail: hi}, nil
}
