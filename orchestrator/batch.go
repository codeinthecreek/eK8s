package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostpairs"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Batch is one stream's /verify pairs for one window, made on the host
// (DESIGN.md sec 10). It names no target: a batch is bound to a variant
// only when RunWindow uploads it, so batches can be made before the gate
// decides whether a newcomer joins the window.
type Batch struct {
	pairs []hostpairs.Pair
	// truth is each pair's ground truth by password and hash, for
	// confirming a wrong_match on the host.
	truth map[pairKey]bool
}

type pairKey struct{ password, hash string }

// Len is how many pairs b holds.
func (b Batch) Len() int { return len(b.pairs) }

// upload is b as the control API takes it.
func (b Batch) upload() wire.Upload {
	u := wire.Upload{Pairs: make([]wire.UploadPair, len(b.pairs))}
	for i := range b.pairs {
		p := &b.pairs[i]
		u.Pairs[i] = wire.UploadPair{Password: &p.Password, Hash: &p.Hash, Match: &p.Match}
	}
	return u
}

// GenerateBatches makes n batches of pairs pairs each, half of them,
// rounded up, match pairs (the burst's split, gate/burstrun.go), each
// batch in a crypto/rand order (hostpairs.Generate), with the fixed core on
// workers goroutines (GOMAXPROCS when <= 0). pairs is GET /config's
// window_pairs. The caller makes one batch per live variant plus one for
// a newcomer, in the gap and never during a window or a gate run
// (DESIGN.md sec 11: host Argon2 must not overlap them).
func GenerateBatches(ctx context.Context, core *hashcore.Core, n, pairs, workers int) ([]Batch, error) {
	if n < 1 || pairs < 1 {
		return nil, fmt.Errorf("orchestrator: %d batches of %d pairs", n, pairs)
	}
	if core == nil {
		return nil, errors.New("orchestrator: no hashcore")
	}
	matches := (pairs + 1) / 2
	out := make([]Batch, n)
	for i := range out {
		ps, err := hostpairs.Generate(ctx, core, matches, pairs-matches, workers)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: generating pairs: %w", err)
		}
		b := Batch{pairs: ps, truth: make(map[pairKey]bool, pairs)}
		for _, p := range ps {
			b.truth[pairKey{p.Password, p.Hash}] = p.Match
		}
		out[i] = b
	}
	return out, nil
}
