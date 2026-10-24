// Package hostpairs is the host's side of the /verify ground truth
// (DESIGN.md sec 8, sec 10): it generates the pairs loadgen sends before
// a Stage 5 burst or a window, and fully verifies the /hash answers
// after it, both with the fixed core.
//
// Each Argon2 computation costs about 19 MiB and tens of milliseconds of
// CPU, so a call's peak memory is about workers x 19 MiB. Callers must
// never overlap a call with a burst or a window (DESIGN.md sec 11): this
// package runs when called and does not check.
package hostpairs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

// Pair is a /verify pair with its ground truth: Match reports whether
// Hash is a hash of Password.
type Pair struct {
	Password, Hash string
	Match          bool
}

// Generate returns matches match pairs and mismatches mismatch pairs, in
// a uniformly random order drawn from crypto/rand, whatever order the
// caller or loadgen later uses. A match pair is a fresh password with
// core.Hash of itself; a mismatch pair is a fresh password with
// core.Hash of another fresh password. Every password the call draws,
// hashed or not, is distinct. It hashes on workers goroutines
// (runtime.GOMAXPROCS(0) when workers <= 0) and checks ctx between
// hashes. It returns ctx's error, and no pairs, if ctx ends first, or
// core.Hash's error if one fails.
func Generate(ctx context.Context, core *hashcore.Core, matches, mismatches, workers int) ([]Pair, error) {
	if matches < 0 || mismatches < 0 {
		return nil, fmt.Errorf("hostpairs: negative pair count (%d matches, %d mismatches)", matches, mismatches)
	}
	n := matches + mismatches
	// pw[:n] are the pairs' passwords; pw[n:] the passwords the
	// mismatch hashes are of.
	pw := freshPasswords(n + mismatches)
	hashOf := func(i int) string {
		if i < matches {
			return pw[i]
		}
		return pw[n+i-matches]
	}
	pairs := make([]Pair, n)
	errs := make([]error, n)
	if err := each(ctx, n, workers, func(i int) {
		h, err := core.Hash(hashOf(i))
		pairs[i] = Pair{Password: pw[i], Hash: h, Match: i < matches}
		errs[i] = err
	}); err != nil {
		return nil, err
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("hostpairs: %w", err)
	}
	if err := shuffle(pairs); err != nil {
		return nil, err
	}
	return pairs, nil
}

// freshPasswords returns n distinct passwords from crypto/rand.Text, as
// the gate's freshPassword draws them. A repeat is about as likely as
// guessing a 128-bit key, but is redrawn anyway.
func freshPasswords(n int) []string {
	seen := make(map[string]bool, n)
	pw := make([]string, 0, n)
	for len(pw) < n {
		if p := rand.Text(); !seen[p] {
			seen[p] = true
			pw = append(pw, p)
		}
	}
	return pw
}

// shuffle puts pairs in a uniformly random order: a Fisher-Yates shuffle
// whose draws come from crypto/rand, so the order cannot be predicted
// from earlier output as a math/rand order could.
func shuffle(pairs []Pair) error {
	for i := len(pairs) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fmt.Errorf("hostpairs: shuffling: %w", err)
		}
		pairs[i], pairs[j.Int64()] = pairs[j.Int64()], pairs[i]
	}
	return nil
}

// Item is a /hash answer to verify: Hash is what a variant returned for
// Password.
type Item struct {
	Password, Hash string
}

// Result is one Item's verdict. OK is true only when core.Verify
// returned (true, nil). Err is core.Verify's error: a malformed hash or
// one not at exactly the fixed parameters. A wrong hash is not OK with
// a nil Err.
type Result struct {
	OK  bool
	Err error
}

// Verify checks every item with core.Verify and returns one Result per
// item, in input order. It verifies on workers goroutines
// (runtime.GOMAXPROCS(0) when workers <= 0) and checks ctx between
// items. Its error is only ever ctx's, returned with no results, when
// ctx ends before every item is verified.
func Verify(ctx context.Context, core *hashcore.Core, items []Item, workers int) ([]Result, error) {
	res := make([]Result, len(items))
	if err := each(ctx, len(items), workers, func(i int) {
		ok, err := core.Verify(items[i].Password, items[i].Hash)
		res[i] = Result{OK: ok && err == nil, Err: err}
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// each calls f(0) to f(n-1), each once, on a pool of workers goroutines
// (runtime.GOMAXPROCS(0) when workers <= 0, and never more than n) that
// take the next index from a shared counter. Each worker checks ctx
// before taking an index, so once ctx ends, every worker stops after
// its current call. each returns ctx's error if any index was never
// taken, and nil once all n calls have returned. f must only write
// state of its own index.
func each(ctx context.Context, n, workers int, f func(i int)) error {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				f(i)
			}
		})
	}
	wg.Wait()
	// A worker stops short of n only when ctx has ended, and ctx.Err
	// stays non-nil once set.
	if next.Load() < int64(n) {
		return ctx.Err()
	}
	return nil
}
