package loadgen

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"slices"
)

// The request mix, in percent: /hash, /verify and contract negatives
// (DESIGN.md sec 10). Starting guesses (DESIGN.md sec 15).
const (
	hashWeight     = 50
	verifyWeight   = 48
	negativeWeight = 2
)

// repeatShare is the share of a window's /hash requests that repeat a
// password already sent in it (DESIGN.md sec 8). A starting guess
// (DESIGN.md sec 15).
const repeatShare = 0.01

// Stage 5 burst negatives: max(ceil(5% of n), 5) (DESIGN.md sec 10). The
// minimum is one per class, so every class is in every burst.
const (
	burstNegativePercent = 5
	burstMinNegatives    = len(negativeClasses)
)

// ErrOutOfPairs means a /verify request was due but every pair has been
// used. No pair is ever reused, and no other kind is sent in its place.
var ErrOutOfPairs = errors.New("loadgen: out of /verify pairs")

// Stream generates one variant's sustained-mode requests for one window.
// It is not safe for concurrent use.
type Stream struct {
	pairs     []Pair
	used      int      // pairs[:used] have been handed out
	hashed    []string // fresh /hash passwords handed out, for repeats
	negatives int      // negatives handed out, for class rotation
}

// NewStream returns a Stream that sends each of pairs once, in random
// order, so the host's order never decides where matches and mismatches
// fall in the window. It keeps its own copy of pairs.
func NewStream(pairs []Pair) *Stream {
	return &Stream{pairs: shuffled(pairs)}
}

// shuffled returns a copy of pairs in random order.
func shuffled(pairs []Pair) []Pair {
	p := slices.Clone(pairs)
	mrand.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
	return p
}

// Next returns the next request, its kind drawn by the request mix. When
// the draw is /verify and every pair has been used, it returns
// ErrOutOfPairs and a zero Request; later calls draw afresh.
func (s *Stream) Next() (Request, error) {
	switch n := mrand.IntN(hashWeight + verifyWeight + negativeWeight); {
	case n < hashWeight:
		return hashRequest(s.hashPassword()), nil
	case n < hashWeight+verifyWeight:
		if s.used == len(s.pairs) {
			return Request{}, ErrOutOfPairs
		}
		s.used++
		return verifyRequest(s.pairs[s.used-1]), nil
	default:
		c := negativeClasses[s.negatives%len(negativeClasses)]
		s.negatives++
		return newNegative(c), nil
	}
}

// hashPassword returns a fresh password, or with probability repeatShare
// one already sent on /hash in this window, picked uniformly from them.
func (s *Stream) hashPassword() string {
	if len(s.hashed) > 0 && mrand.Float64() < repeatShare {
		return s.hashed[mrand.IntN(len(s.hashed))]
	}
	pw := newPassword()
	s.hashed = append(s.hashed, pw)
	return pw
}

// BurstCounts returns how many /hash, /verify and negative requests a
// Stage 5 burst of n holds: max(ceil(5% of n), 5) negatives, and the rest
// split 50:48, rounded to nearest. It errors if n cannot hold the
// negatives.
func BurstCounts(n int) (hashes, verifies, negatives int, err error) {
	negatives = max((n*burstNegativePercent+99)/100, burstMinNegatives)
	if n < negatives {
		return 0, 0, 0, fmt.Errorf("loadgen: a burst of %d cannot hold its %d negatives", n, negatives)
	}
	rest := n - negatives
	hashes = (2*rest*hashWeight + hashWeight + verifyWeight) / (2 * (hashWeight + verifyWeight))
	return hashes, rest - hashes, negatives, nil
}

// Burst returns a Stage 5 burst of n requests in shuffled order: the
// negatives cycle through the classes, the /hash passwords are all fresh,
// and the /verify requests use BurstCounts(n) verifies of pairs, drawn at
// random, each once. It does not modify pairs. It errors if n cannot hold
// the negatives, and wraps ErrOutOfPairs if pairs is too short.
func Burst(n int, pairs []Pair) ([]Request, error) {
	hashes, verifies, negatives, err := BurstCounts(n)
	if err != nil {
		return nil, err
	}
	if len(pairs) < verifies {
		return nil, fmt.Errorf("%w: a burst of %d needs %d, got %d", ErrOutOfPairs, n, verifies, len(pairs))
	}
	reqs := make([]Request, 0, n)
	for i := range negatives {
		reqs = append(reqs, newNegative(negativeClasses[i%len(negativeClasses)]))
	}
	for range hashes {
		reqs = append(reqs, hashRequest(newPassword()))
	}
	for _, p := range shuffled(pairs)[:verifies] {
		reqs = append(reqs, verifyRequest(p))
	}
	mrand.Shuffle(len(reqs), func(i, j int) { reqs[i], reqs[j] = reqs[j], reqs[i] })
	return reqs, nil
}
