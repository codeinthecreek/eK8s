package hashcore

import (
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
)

const wantPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

// forged builds a hash that differs from a valid one only in the params
// segment: correct prefix, a 16-byte salt and a 32-byte key in the
// library's encoding (unpadded standard base64). A rejection therefore
// comes from the params check, not the syntax check.
func forged(paramsSeg string) string {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	return "$argon2id$v=19$" + paramsSeg + "$" + salt + "$" + key
}

func mustHash(t *testing.T, c *Core, pw string) string {
	t.Helper()
	h, err := c.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return h
}

func TestRoundTrip(t *testing.T) {
	c := New()
	h := mustHash(t, c, "correct horse")
	if err := checkParams(h); err != nil {
		t.Fatalf("checkParams(%q) = %v, want nil", h, err)
	}
	ok, err := c.Verify("correct horse", h)
	if err != nil || !ok {
		t.Fatalf("Verify = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestHashPrefix(t *testing.T) {
	h := mustHash(t, New(), "pw")
	if !strings.HasPrefix(h, wantPrefix) {
		t.Fatalf("hash %q lacks prefix %q", h, wantPrefix)
	}
}

// The valid-syntax helper must itself pass the check at exact params,
// or the rejection tests below prove nothing.
func TestForgedAtExactParamsPassesCheck(t *testing.T) {
	if err := checkParams(forged("m=19456,t=2,p=1")); err != nil {
		t.Fatalf("checkParams(forged exact) = %v, want nil", err)
	}
}

// Pins the Verify error contract: a mismatch is (false, nil), never an
// error. The wrapper maps any Verify error to 400, so this must hold.
func TestVerifyMismatchIsFalseNil(t *testing.T) {
	c := New()
	h := mustHash(t, c, "right")
	ok, err := c.Verify("wrong", h)
	if ok || err != nil {
		t.Fatalf("Verify = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestVerifyRejectsT1(t *testing.T) {
	p := params()
	p.Iterations = 1
	h, err := argon2id.CreateHash("pw", p)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := New().Verify("pw", h)
	if ok || !errors.Is(err, ErrUnsupportedParams) {
		t.Fatalf("Verify(t=1) = (%v, %v), want (false, ErrUnsupportedParams)", ok, err)
	}
}

func TestVerifyRejectsHugeMemoryWithoutComputing(t *testing.T) {
	c := New()
	start := time.Now()
	mustHash(t, c, "pw")
	oneHash := time.Since(start)

	// m=4194304 KiB is 4 GiB: it must be rejected before any Argon2
	// computation, since computing it would allocate about 4 GiB.
	h := forged("m=4194304,t=2,p=1")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start = time.Now()
	ok, err := c.Verify("pw", h)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if ok || !errors.Is(err, ErrUnsupportedParams) {
		t.Fatalf("Verify(m=4GiB) = (%v, %v), want (false, ErrUnsupportedParams)", ok, err)
	}
	if elapsed > oneHash/10 {
		t.Errorf("rejection took %v, one real hash took %v; want under a tenth", elapsed, oneHash)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("rejection allocated %d bytes, want under 1 MiB", alloc)
	}
}

func TestVerifyRejectsNonExactParams(t *testing.T) {
	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	cases := map[string]string{
		"m too small":     forged("m=19455,t=2,p=1"),
		"t=3":             forged("m=19456,t=3,p=1"),
		"p=2":             forged("m=19456,t=2,p=2"),
		"non-canonical m": forged("m=019456,t=2,p=1"),
		"trailing params": forged("m=19456,t=2,p=1,x"),
		"argon2i variant": "$argon2i$v=19$m=19456,t=2,p=1$" + salt16 + "$" + key32,
		"argon2d variant": "$argon2d$v=19$m=19456,t=2,p=1$" + salt16 + "$" + key32,
		"version 16":      "$argon2id$v=16$m=19456,t=2,p=1$" + salt16 + "$" + key32,
		"8-byte salt":     wantPrefix + base64.RawStdEncoding.EncodeToString(make([]byte, 8)) + "$" + key32,
		"16-byte key":     wantPrefix + salt16 + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 16)),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := New().Verify("pw", h)
			if ok || !errors.Is(err, ErrUnsupportedParams) {
				t.Fatalf("Verify = (%v, %v), want (false, ErrUnsupportedParams)", ok, err)
			}
		})
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	cases := map[string]string{
		"empty":           "",
		"not a hash":      "not-a-hash",
		"too few fields":  wantPrefix + key32,
		"leading junk":    "x" + forged("m=19456,t=2,p=1"),
		"bad salt base64": wantPrefix + "!!!!$" + key32,
		"padded salt":     wantPrefix + base64.StdEncoding.EncodeToString(make([]byte, 16)) + "$" + key32,
		// base64 decoding ignores CR/LF, so these decode to valid bytes.
		"salt mid newline":     wantPrefix + salt16[:len(salt16)/2] + "\n" + salt16[len(salt16)/2:] + "$" + key32,
		"key trailing newline": wantPrefix + salt16 + "$" + key32 + "\n",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := New().Verify("pw", h)
			if ok || err == nil {
				t.Fatalf("Verify = (%v, %v), want an error", ok, err)
			}
			if errors.Is(err, ErrUnsupportedParams) {
				t.Fatalf("Verify error %v is ErrUnsupportedParams, want a syntax error", err)
			}
			if !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("Verify error %v is not ErrMalformedHash", err)
			}
		})
	}
}

// classify must map every error DecodeHash can return to a sentinel.
// The variant/version/Sscanf errors are unreachable through Verify, so
// they are exercised here by calling DecodeHash directly.
func TestClassifyDecodeHashErrors(t *testing.T) {
	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	cases := []struct {
		name, hash string
		libErr     error // nil: a non-sentinel fmt/base64 error
		want       error
	}{
		{"segment count", "not-a-hash", argon2id.ErrInvalidHash, ErrMalformedHash},
		{"argon2i", "$argon2i$v=19$m=19456,t=2,p=1$" + salt16 + "$" + key32, argon2id.ErrIncompatibleVariant, ErrUnsupportedParams},
		{"v=16", "$argon2id$v=16$m=19456,t=2,p=1$" + salt16 + "$" + key32, argon2id.ErrIncompatibleVersion, ErrUnsupportedParams},
		{"version syntax", "$argon2id$x$m=19456,t=2,p=1$" + salt16 + "$" + key32, nil, ErrMalformedHash},
		{"params syntax", "$argon2id$v=19$x$" + salt16 + "$" + key32, nil, ErrMalformedHash},
		{"salt base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!!$" + key32, nil, ErrMalformedHash},
		{"key base64", "$argon2id$v=19$m=19456,t=2,p=1$" + salt16 + "$!!!!", nil, ErrMalformedHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := argon2id.DecodeHash(tc.hash)
			if err == nil {
				t.Fatal("DecodeHash accepted the input")
			}
			if tc.libErr != nil && !errors.Is(err, tc.libErr) {
				t.Fatalf("DecodeHash error = %v, want %v", err, tc.libErr)
			}
			if got := classify(err); !errors.Is(got, tc.want) {
				t.Fatalf("classify(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}
