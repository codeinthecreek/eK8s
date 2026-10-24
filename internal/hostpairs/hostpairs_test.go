package hostpairs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

// TestGenerateGroundTruth checks every pair against the real core: a
// match pair verifies true and a mismatch pair false, the counts are
// as asked, and the passwords are distinct.
func TestGenerateGroundTruth(t *testing.T) {
	core := hashcore.New()
	for _, c := range []struct{ matches, mismatches int }{{0, 0}, {1, 0}, {0, 1}, {7, 5}} {
		pairs, err := Generate(t.Context(), core, c.matches, c.mismatches, 0)
		if err != nil {
			t.Fatalf("Generate(%d, %d): %v", c.matches, c.mismatches, err)
		}
		if len(pairs) != c.matches+c.mismatches {
			t.Fatalf("Generate(%d, %d) returned %d pairs", c.matches, c.mismatches, len(pairs))
		}
		matches := 0
		seen := make(map[string]bool)
		for i, p := range pairs {
			if seen[p.Password] {
				t.Errorf("Generate(%d, %d): pair %d repeats password %q", c.matches, c.mismatches, i, p.Password)
			}
			seen[p.Password] = true
			if p.Match {
				matches++
			}
			ok, err := core.Verify(p.Password, p.Hash)
			if ok != p.Match || err != nil {
				t.Errorf("Generate(%d, %d): pair %d has Match %v, but Verify returned (%v, %v)", c.matches, c.mismatches, i, p.Match, ok, err)
			}
		}
		if matches != c.matches {
			t.Errorf("Generate(%d, %d) returned %d match pairs", c.matches, c.mismatches, matches)
		}
	}
	if _, err := Generate(t.Context(), core, -1, 1, 0); err == nil {
		t.Error("Generate(-1, 1) returned no error")
	}
	if _, err := Generate(t.Context(), core, 1, -1, 0); err == nil {
		t.Error("Generate(1, -1) returned no error")
	}
}

// TestGenerateOrder checks that the pairs are shuffled. Under a uniform
// shuffle, 64 matches and 64 mismatches come out sorted either way with
// probability 2/C(128, 64), about 8.4e-38, and the first pair's Match is
// the same across 20 calls with probability 2 x 2^-20, about 1.9e-6:
// the test's false-failure probability.
func TestGenerateOrder(t *testing.T) {
	core := hashcore.New()
	pairs, err := Generate(t.Context(), core, 64, 64, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sorted(pairs, true) || sorted(pairs, false) {
		t.Error("Generate(64, 64): the match flags are sorted; not shuffled")
	}
	first := make(map[bool]int)
	for range 20 {
		pairs, err := Generate(t.Context(), core, 1, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		first[pairs[0].Match]++
	}
	if len(first) != 2 {
		t.Errorf("Generate(1, 1): the first pair's Match was the same in 20 calls: %v", first)
	}
}

// sorted reports whether every pair with Match == head comes before
// every other pair.
func sorted(pairs []Pair, head bool) bool {
	i := 0
	for i < len(pairs) && pairs[i].Match == head {
		i++
	}
	for _, p := range pairs[i:] {
		if p.Match == head {
			return false
		}
	}
	return true
}

// TestVerify checks each kind of verdict, and that results keep input
// order with one worker and with four.
func TestVerify(t *testing.T) {
	core := hashcore.New()
	const pw = "correct horse"
	h, err := core.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	// Change the key's first character: the hash stays canonical, so
	// Verify compares and finds a mismatch.
	key := strings.LastIndexByte(h, '$') + 1
	swap := byte('A')
	if h[key] == swap {
		swap = 'B'
	}
	tampered := h[:key] + string(swap) + h[key+1:]
	const malformed = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA"

	type want struct {
		ok  bool
		err error // nil, or the hashcore sentinel Err must wrap
	}
	cases := []struct {
		name string
		item Item
		want want
	}{
		{"valid", Item{pw, h}, want{true, nil}},
		{"wrong password", Item{"wrong", h}, want{false, nil}},
		{"tampered key", Item{pw, tampered}, want{false, nil}},
		{"malformed", Item{pw, malformed}, want{false, hashcore.ErrMalformedHash}},
	}
	check := func(t *testing.T, name string, got Result, w want) {
		t.Helper()
		if got.OK != w.ok {
			t.Errorf("%s: OK = %v, want %v", name, got.OK, w.ok)
		}
		switch {
		case w.err == nil && got.Err != nil:
			t.Errorf("%s: Err = %v, want nil", name, got.Err)
		case w.err != nil && !errors.Is(got.Err, w.err):
			t.Errorf("%s: Err = %v, want %v", name, got.Err, w.err)
		}
	}
	for _, c := range cases {
		res, err := Verify(t.Context(), core, []Item{c.item}, 1)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(res) != 1 {
			t.Fatalf("%s: %d results, want 1", c.name, len(res))
		}
		check(t, c.name, res[0], c.want)
	}

	// 24 items cycling through the cases in a pattern no sort or
	// grouping preserves.
	var items []Item
	var wants []want
	var names []string
	for i := range 24 {
		c := cases[(i*i+i/3)%len(cases)]
		items = append(items, c.item)
		wants = append(wants, c.want)
		names = append(names, c.name)
	}
	for _, workers := range []int{1, 4} {
		res, err := Verify(t.Context(), core, items, workers)
		if err != nil {
			t.Fatalf("workers %d: %v", workers, err)
		}
		if len(res) != len(items) {
			t.Fatalf("workers %d: %d results, want %d", workers, len(res), len(items))
		}
		for i := range res {
			check(t, fmt.Sprintf("%s at %d with %d workers", names[i], i, workers), res[i], wants[i])
		}
	}
	if res, err := Verify(t.Context(), core, nil, 0); err != nil || len(res) != 0 {
		t.Errorf("Verify of no items = (%v, %v), want no results and no error", res, err)
	}
}

// cancelBound is how long a call may run on after its ctx is
// cancelled: each worker finishes its current Argon2 computation, tens
// of milliseconds, slower under the race detector.
const cancelBound = 2 * time.Second

// cancelDelay is how long TestCancel lets a call run before cancelling
// its ctx.
const cancelDelay = 200 * time.Millisecond

// TestCancel checks that both functions return ctx's error, and no
// results, promptly when ctx is cancelled mid-run, on far more work than
// they could finish in the time. Generate's 4,000 hashes, at tens of
// milliseconds each on 2 workers, take tens of seconds (minutes under
// -race), far longer than cancelDelay plus cancelBound, and its 6,000
// passwords are drawn in milliseconds, so hashing is running when the
// cancel lands. A call still running a second past that bound fails the
// test there, with its goroutine left running, rather than at the test
// timeout.
func TestCancel(t *testing.T) {
	core := hashcore.New()
	h, err := core.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	items := make([]Item, 100000)
	for i := range items {
		items[i] = Item{"pw", h}
	}
	for _, c := range []struct {
		name string
		call func(ctx context.Context) (int, error)
	}{
		{"Generate", func(ctx context.Context) (int, error) {
			pairs, err := Generate(ctx, core, 2000, 2000, 2)
			return len(pairs), err
		}},
		{"Verify", func(ctx context.Context) (int, error) {
			res, err := Verify(ctx, core, items, 2)
			return len(res), err
		}},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		cancelled := make(chan time.Time, 1)
		time.AfterFunc(cancelDelay, func() {
			cancelled <- time.Now()
			cancel()
		})
		type result struct {
			n        int
			err      error
			returned time.Time
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			n, err := c.call(ctx)
			done <- result{n, err, time.Now()}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(cancelDelay + cancelBound + time.Second):
			cancel()
			t.Fatalf("%s has run %v without returning", c.name, time.Since(start))
		}
		cancel()
		n, err, returned := r.n, r.err, r.returned
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: error %v, want context.Canceled", c.name, err)
		}
		if n != 0 {
			t.Errorf("%s: %d results with the error, want none", c.name, n)
		}
		select {
		case at := <-cancelled:
			if d := returned.Sub(at); d > cancelBound {
				t.Errorf("%s returned %v after its ctx was cancelled, over %v", c.name, d, cancelBound)
			}
		default:
			t.Errorf("%s returned before its ctx was cancelled", c.name)
		}
	}
}
