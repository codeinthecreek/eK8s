package loadgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

func testPairs(n int) []Pair {
	pairs := make([]Pair, n)
	for i := range pairs {
		pairs[i] = Pair{Password: fmt.Sprintf("password-%06d", i), Hash: randomExactPHC(), Match: i%2 == 0}
	}
	return pairs
}

func within(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f +- %.4f", name, got, want, tol)
	}
}

// TestStreamMix checks the mix shares, the repeat share and the class
// rotation over one long window. Tolerances are at least 5 standard
// deviations, so a correct generator fails about once in a million runs.
func TestStreamMix(t *testing.T) {
	const n = 20000
	s := NewStream(testPairs(n))
	counts := map[Kind]int{}
	seen := map[string]int{} // fresh /hash password -> its index among them
	repeats := 0
	var repeatPos float64 // sum of each repeat's index / fresh count at the time
	var classes []NegativeClass
	for range n {
		r, err := s.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		counts[r.Kind]++
		switch r.Kind {
		case KindHash:
			checkAlphabet(t, r.Password)
			if len(r.Password) < minPasswordLen || len(r.Password) > maxPasswordLen {
				t.Fatalf("/hash password of %d characters", len(r.Password))
			}
			if i, ok := seen[r.Password]; ok {
				repeats++
				repeatPos += (float64(i) + 0.5) / float64(len(seen))
			} else {
				seen[r.Password] = len(seen)
			}
		case KindNegative:
			classes = append(classes, r.Class)
		}
	}
	within(t, "hash share", float64(counts[KindHash])/n, 0.50, 0.02)
	within(t, "verify share", float64(counts[KindVerify])/n, 0.48, 0.02)
	within(t, "negative share", float64(counts[KindNegative])/n, 0.02, 0.006)
	within(t, "repeat share of /hash", float64(repeats)/float64(counts[KindHash]), repeatShare, 0.005)
	// Uniform picks put a repeat's relative position at 0.5 on average.
	within(t, "mean relative position of repeated passwords", repeatPos/float64(repeats), 0.5, 0.15)
	start := slices.Index(negativeClasses[:], classes[0])
	for i, c := range classes {
		if want := negativeClasses[(start+i)%len(negativeClasses)]; c != want {
			t.Fatalf("negative %d has class %s, want %s (rotation)", i, c, want)
		}
	}
}

// TestStreamRotationStart checks that every class is the first negative
// of some Stream. A fixed start misses four classes; a uniform one misses
// any given class in 200 Streams with probability 0.8^200, about 4e-20.
func TestStreamRotationStart(t *testing.T) {
	first := map[NegativeClass]bool{}
	for range 200 {
		s := NewStream(nil)
		for {
			r, err := s.Next()
			if err == nil && r.Kind == KindNegative {
				first[r.Class] = true
				break
			}
		}
	}
	for _, c := range negativeClasses {
		if !first[c] {
			t.Errorf("class %s was never a Stream's first negative", c)
		}
	}
}

// TestStreamPairsOnce checks that each pair is sent once, as itself, and
// that an empty stream returns ErrOutOfPairs rather than reusing a pair
// or sending another kind.
func TestStreamPairsOnce(t *testing.T) {
	pairs := testPairs(200)
	byPassword := map[string]Pair{}
	for _, p := range pairs {
		byPassword[p.Password] = p
	}
	s := NewStream(pairs)
	sent := map[string]bool{}
	outs := 0
	for i := 0; outs < 50; i++ {
		if i > 100000 {
			t.Fatalf("only %d ErrOutOfPairs after %d calls", outs, i)
		}
		r, err := s.Next()
		if errors.Is(err, ErrOutOfPairs) {
			if len(sent) != len(pairs) {
				t.Fatalf("ErrOutOfPairs after %d of %d pairs", len(sent), len(pairs))
			}
			if r.Path != "" || r.Body != nil {
				t.Fatalf("ErrOutOfPairs with a request: %+v", r)
			}
			outs++
			continue
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if r.Kind != KindVerify {
			continue
		}
		p, ok := byPassword[r.Password]
		if !ok || sent[r.Password] {
			t.Fatalf("verify request for password %q: known %v, already sent %v", r.Password, ok, sent[r.Password])
		}
		sent[r.Password] = true
		var body struct{ Password, Hash string }
		if err := json.Unmarshal(r.Body, &body); err != nil || r.Path != VerifyPath || body.Password != p.Password || body.Hash != p.Hash || r.Match != p.Match {
			t.Fatalf("verify request %+v (body error %v) does not carry pair %+v", r, err, p)
		}
	}
	if _, err := NewStream(nil).nextVerify(); !errors.Is(err, ErrOutOfPairs) {
		t.Errorf("empty stream: err = %v, want ErrOutOfPairs", err)
	}
}

// nextVerify calls Next until it returns a /verify request or an error.
func (s *Stream) nextVerify() (Request, error) {
	for {
		r, err := s.Next()
		if err != nil || r.Kind == KindVerify {
			return r, err
		}
	}
}

func TestBurstCounts(t *testing.T) {
	for _, tc := range []struct{ n, hashes, verifies, negatives int }{
		{5, 0, 0, 5},
		{6, 1, 0, 5},
		{100, 48, 47, 5},
		{101, 48, 47, 6},
		{1000, 485, 465, 50},
	} {
		h, v, neg, err := BurstCounts(tc.n)
		if err != nil || h != tc.hashes || v != tc.verifies || neg != tc.negatives {
			t.Errorf("BurstCounts(%d) = %d, %d, %d, %v; want %d, %d, %d, nil", tc.n, h, v, neg, err, tc.hashes, tc.verifies, tc.negatives)
		}
	}
	for _, n := range []int{-1, 0, 4} {
		if _, _, _, err := BurstCounts(n); err == nil {
			t.Errorf("BurstCounts(%d): no error", n)
		}
	}
	for n := 5; n <= 3000; n++ {
		h, v, neg, err := BurstCounts(n)
		if err != nil {
			t.Fatalf("BurstCounts(%d): %v", n, err)
		}
		rest := n - neg
		if h+v != rest || neg != max(int(math.Ceil(float64(n)*0.05-1e-9)), 5) || math.Abs(float64(h)-float64(rest)*50/98) > 0.5 {
			t.Fatalf("BurstCounts(%d) = %d, %d, %d", n, h, v, neg)
		}
	}
}

// TestBurst checks the quotas, every class at least once, each pair at
// most once, no /hash password equal to a pair's, and that positions are
// shuffled. TestBurstRepeats checks the /hash repeats.
func TestBurst(t *testing.T) {
	for _, n := range []int{5, 7, 100, 333} {
		pairs := testPairs(n)
		reqs, err := Burst(n, pairs)
		if err != nil {
			t.Fatalf("Burst(%d): %v", n, err)
		}
		hashes, verifies, negatives, _ := BurstCounts(n)
		counts := map[Kind]int{}
		classes := map[NegativeClass]int{}
		known := map[string]bool{}
		for _, p := range pairs {
			known[p.Password] = true
		}
		verified := map[string]bool{}
		for _, r := range reqs {
			counts[r.Kind]++
			switch r.Kind {
			case KindNegative:
				classes[r.Class]++
			case KindHash:
				if known[r.Password] {
					t.Fatalf("Burst(%d): /hash password %q is a pair's", n, r.Password)
				}
			case KindVerify:
				if verified[r.Password] || !known[r.Password] {
					t.Fatalf("Burst(%d): /verify password %q: already sent %v, from a pair %v", n, r.Password, verified[r.Password], known[r.Password])
				}
				verified[r.Password] = true
			}
		}
		if len(reqs) != n || counts[KindHash] != hashes || counts[KindVerify] != verifies || counts[KindNegative] != negatives {
			t.Fatalf("Burst(%d): %d requests, kinds %v; want %d, %d, %d", n, len(reqs), counts, hashes, verifies, negatives)
		}
		for _, c := range negativeClasses {
			if k := classes[c]; k < negatives/len(negativeClasses) || k > negatives/len(negativeClasses)+1 {
				t.Errorf("Burst(%d): %d negatives of class %s out of %d", n, k, c, negatives)
			}
		}
	}

	// 10 negatives in 200 all landing first by chance: 1 in C(200, 10).
	reqs, err := Burst(200, testPairs(200))
	if err != nil {
		t.Fatal(err)
	}
	allFirst := true
	for _, r := range reqs[:10] {
		allFirst = allFirst && r.Kind == KindNegative
	}
	if allFirst {
		t.Error("Burst(200): the negatives are the first 10 requests; not shuffled")
	}

	if _, err := Burst(4, testPairs(4)); err == nil {
		t.Error("Burst(4): no error")
	}
	_, verifies, _, _ := BurstCounts(100)
	if _, err := Burst(100, testPairs(verifies)); err != nil {
		t.Errorf("Burst(100) with exactly %d pairs: %v", verifies, err)
	}
	if _, err := Burst(100, testPairs(verifies-1)); !errors.Is(err, ErrOutOfPairs) {
		t.Errorf("Burst(100) with %d pairs: err = %v, want ErrOutOfPairs", verifies-1, err)
	}
}

// matchesThenMismatches returns n matching pairs followed by n mismatching
// ones: a host batch in the worst order.
func matchesThenMismatches(n int) []Pair {
	pairs := testPairs(2 * n)
	for i := range pairs {
		pairs[i].Match = i < n
	}
	return pairs
}

// TestPairOrderIgnored checks that a batch of all matches followed by all
// mismatches comes out interleaved, from a Stream and from a Burst, and
// that neither reorders nor keeps the caller's slice. The match share of
// n pairs drawn from 2n has a standard deviation of about 0.025 here; the
// tolerances are 5 of them.
func TestPairOrderIgnored(t *testing.T) {
	const n = 200
	pairs := matchesThenMismatches(n)
	orig := slices.Clone(pairs)
	s := NewStream(pairs)
	if !slices.Equal(pairs, orig) {
		t.Fatal("NewStream reordered the caller's pairs")
	}
	for i := range pairs {
		pairs[i] = Pair{Password: "overwritten", Hash: "overwritten"}
	}
	matches := 0
	for range n {
		r, err := s.nextVerify()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if r.Password == "overwritten" {
			t.Fatal("Stream sent a pair the caller overwrote after NewStream: it aliases the caller's slice")
		}
		if r.Match {
			matches++
		}
	}
	within(t, "match share of a Stream's first half of verifies", float64(matches)/n, 0.5, 0.125)

	const burst = 400
	_, verifies, _, _ := BurstCounts(burst)
	pairs = matchesThenMismatches(verifies)
	orig = slices.Clone(pairs)
	reqs, err := Burst(burst, pairs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pairs, orig) {
		t.Fatal("Burst reordered the caller's pairs")
	}
	matches = 0
	for _, r := range reqs {
		if r.Kind == KindVerify && r.Match {
			matches++
		}
	}
	within(t, "match share of a Burst's verifies", float64(matches)/float64(verifies), 0.5, 0.13)
}

// TestBurstRepeats checks the /hash repeat share in a burst, at the same
// tolerance as TestStreamMix's (about 5 standard deviations here).
func TestBurstRepeats(t *testing.T) {
	const n = 20000
	hashes, verifies, _, _ := BurstCounts(n)
	reqs, err := Burst(n, testPairs(verifies))
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[string]bool{}
	for _, r := range reqs {
		if r.Kind == KindHash {
			checkAlphabet(t, r.Password)
			distinct[r.Password] = true
		}
	}
	within(t, "repeat share of burst /hash", float64(hashes-len(distinct))/float64(hashes), repeatShare, 0.005)
}
