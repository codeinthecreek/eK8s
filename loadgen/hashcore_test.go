package loadgen_test

import (
	"encoding/json"
	"errors"
	"runtime"
	"testing"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen"
)

// Argon2 at the fixed parameters allocates its 19456 KiB of memory
// blocks on every call, so heap allocation shows whether a Verify call
// ran Argon2, deterministically and without timing: a call that ran it
// allocates at least argon2MinAlloc, one that stopped at the parameter
// check far less than noArgon2MaxAlloc.
const (
	argon2MinAlloc   = 16 << 20
	noArgon2MaxAlloc = 1 << 20
)

// allocDuring returns the bytes the heap allocated while f ran. Not
// meaningful with parallel tests running; none in this package are.
// Should that change, the bound still fails safe: TotalAlloc only grows,
// so concurrent allocation can only push a negative over
// noArgon2MaxAlloc (a false fail) or inflate the positive control, never
// bring a negative that ran Argon2 under the bound.
func allocDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestNegativeHashesAgreeWithHashcore checks every form of the two hash
// classes against hashcore: each gets its class's error, without Argon2.
func TestNegativeHashesAgreeWithHashcore(t *testing.T) {
	core := hashcore.New()

	// Control: the measurement sees Argon2 when it runs, and hashcore
	// takes loadgen's exact-parameters PHC as well-formed.
	var match bool
	var err error
	alloc := allocDuring(func() { match, err = core.Verify("control password", loadgen.RandomExactPHC()) })
	if err != nil || match {
		t.Fatalf("Verify(random exact PHC) = %v, %v; want false, nil", match, err)
	}
	if alloc < argon2MinAlloc {
		t.Fatalf("control Verify allocated %d bytes; want >= %d, or the bound below proves nothing", alloc, argon2MinAlloc)
	}

	for _, tc := range []struct {
		class loadgen.NegativeClass
		want  error
	}{
		{loadgen.MalformedHash, hashcore.ErrMalformedHash},
		{loadgen.NonExactParams, hashcore.ErrUnsupportedParams},
	} {
		forms := loadgen.Forms(tc.class)
		if len(forms) == 0 {
			t.Fatalf("%s: no forms", tc.class)
		}
		for _, f := range forms {
			for range 100 {
				r := f.Build()
				var body struct{ Password, Hash string }
				if err := json.Unmarshal(r.Body, &body); err != nil {
					t.Fatalf("%s/%s: body %q: %v", tc.class, f.Name, r.Body, err)
				}
				alloc := allocDuring(func() { match, err = core.Verify(body.Password, body.Hash) })
				if !errors.Is(err, tc.want) || match {
					t.Fatalf("%s/%s: Verify(hash %q) = %v, %v; want false, %v", tc.class, f.Name, body.Hash, match, err, tc.want)
				}
				if alloc >= noArgon2MaxAlloc {
					t.Fatalf("%s/%s: Verify(hash %q) allocated %d bytes: Argon2 ran", tc.class, f.Name, body.Hash, alloc)
				}
			}
		}
	}
}
