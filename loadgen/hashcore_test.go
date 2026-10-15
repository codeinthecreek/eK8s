package loadgen_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// TestNegativeHashesAgreeWithHashcore checks the two hash classes
// against hashcore: each hash gets its class's error, without Argon2. It
// checks every form once as built, then enumerates every variable part of
// the malformed forms instead of sampling them: each truncation cut, and
// for both salt and key each newline position, each unused-bit value and
// the padded encoding. The random bytes do not change any outcome.
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

	check := func(name, password, hash string, want error) {
		t.Helper()
		alloc := allocDuring(func() { match, err = core.Verify(password, hash) })
		if !errors.Is(err, want) || match {
			t.Errorf("%s: Verify(hash %q) = %v, %v; want false, %v", name, hash, match, err, want)
		}
		if alloc >= noArgon2MaxAlloc {
			t.Errorf("%s: Verify(hash %q) allocated %d bytes: Argon2 ran", name, hash, alloc)
		}
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
			r := f.Build()
			var body struct{ Password, Hash string }
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatalf("%s/%s: body %q: %v", tc.class, f.Name, r.Body, err)
			}
			check(fmt.Sprintf("%s/%s", tc.class, f.Name), body.Password, body.Hash, tc.want)
		}
	}

	const pw = "negative password"
	salt, key := randomBytes(loadgen.SaltLen), randomBytes(loadgen.KeyLen)
	s := loadgen.ExactPHC(loadgen.B64(salt), loadgen.B64(key))
	for k := 1; k <= loadgen.LastCut(s); k++ {
		check(fmt.Sprintf("truncated at %d of %d", k, len(s)), pw, s[:k], hashcore.ErrMalformedHash)
	}
	for _, part := range []struct {
		name   string
		b      []byte
		phc    func(enc string) string // the PHC string with this part encoded as enc
		unused int                     // non-zero values of its 4 or 2 unused bits
	}{
		{"salt", salt, func(enc string) string { return loadgen.ExactPHC(enc, loadgen.B64(key)) }, 15},
		{"key", key, func(enc string) string { return loadgen.ExactPHC(loadgen.B64(salt), enc) }, 3},
	} {
		enc := loadgen.B64(part.b)
		if got := loadgen.UnusedBitValues(part.b); got != part.unused {
			t.Errorf("%s: %d unused-bit values, want %d", part.name, got, part.unused)
		}
		// Every position, both ends included: a superset of insertNewline's.
		for i := 0; i <= len(enc); i++ {
			check(fmt.Sprintf("%s newline at %d", part.name, i), pw, part.phc(loadgen.InsertNewlineAt(enc, i)), hashcore.ErrMalformedHash)
		}
		for v := 1; v <= loadgen.UnusedBitValues(part.b); v++ {
			check(fmt.Sprintf("%s unused bits %d", part.name, v), pw, part.phc(loadgen.SetUnusedBitsTo(part.b, v)), hashcore.ErrMalformedHash)
		}
		check(part.name+" padded", pw, part.phc(base64.StdEncoding.EncodeToString(part.b)), hashcore.ErrMalformedHash)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}
