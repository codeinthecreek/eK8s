package hashcore

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/alexedwards/argon2id"
)

// The only accepted encoding of the DESIGN.md sec 6 parameters. Verify
// compares these segments as exact strings, so a non-canonical spelling
// of the same numbers (e.g. "m=019456") is rejected too.
const (
	variantSegment = "argon2id"
	versionSegment = "v=19"
	paramsSegment  = "m=19456,t=2,p=1"
	saltLength     = 16
	keyLength      = 32
)

var (
	// ErrUnsupportedParams is returned by Verify for a hash whose
	// variant, version or parameters are not exactly the fixed ones.
	ErrUnsupportedParams = errors.New("hashcore: unsupported hash parameters")

	// ErrMalformedHash is returned by Verify for a hash that is not a
	// syntactically valid PHC string.
	ErrMalformedHash = errors.New("hashcore: malformed hash")
)

// params returns a fresh copy, so no caller can mutate shared state.
// Never argon2id.DefaultParams: its Iterations is 1.
func params() *argon2id.Params {
	return &argon2id.Params{
		Memory:      19456,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  saltLength,
		KeyLength:   keyLength,
	}
}

// Core hashes and verifies passwords with Argon2id at the fixed
// parameters. The zero value is ready to use and safe for concurrent use.
type Core struct{}

// New returns a Core.
func New() *Core { return &Core{} }

// Hash returns a PHC-encoded Argon2id hash of password with a fresh
// random salt. It errors only if randomness cannot be obtained; since
// Go 1.24 crypto/rand.Read crashes the program instead of returning an
// error, so in practice the error is always nil.
func (*Core) Hash(password string) (string, error) {
	return argon2id.CreateHash(password, params())
}

// Verify reports whether password matches encoded, using the library's
// constant-time comparison. A mismatch returns (false, nil). A non-nil
// error means only that encoded is malformed (ErrMalformedHash) or not
// at exactly the fixed parameters (ErrUnsupportedParams).
//
// The parameters are checked before any Argon2 computation: the library
// takes them from the hash string itself, so this blocks both downgrade
// and memory exhaustion via a huge m (DESIGN.md sec 6).
func (*Core) Verify(password, encoded string) (bool, error) {
	if err := checkParams(encoded); err != nil {
		return false, err
	}
	match, err := argon2id.ComparePasswordAndHash(password, encoded)
	if err != nil {
		// Unreachable after checkParams decoded the same string; still
		// classified, so no unclassified error can escape.
		return false, classify(err)
	}
	return match, nil
}

// checkParams validates encoded without computing anything. It checks
// the variant, version and params segments itself, before DecodeHash,
// so DecodeHash's own variant/version/Sscanf errors are unreachable
// here; classify still maps them should that ever change.
func checkParams(encoded string) error {
	segs := strings.Split(encoded, "$")
	if len(segs) != 6 || segs[0] != "" {
		return ErrMalformedHash
	}
	if segs[1] != variantSegment || segs[2] != versionSegment || segs[3] != paramsSegment {
		return ErrUnsupportedParams
	}
	// DecodeHash only parses and base64-decodes; it runs no Argon2.
	_, salt, key, err := argon2id.DecodeHash(encoded)
	if err != nil {
		return classify(err)
	}
	// go doc base64.Encoding.Strict: input is still malleable (CR/LF ignored).
	if base64.RawStdEncoding.EncodeToString(salt) != segs[4] || base64.RawStdEncoding.EncodeToString(key) != segs[5] {
		return ErrMalformedHash
	}
	if len(salt) != saltLength || len(key) != keyLength {
		return ErrUnsupportedParams
	}
	return nil
}

// classify maps every error argon2id.DecodeHash can return (v1.0.0) to
// one of the two sentinels: a wrong variant or version is a non-exact
// algorithm configuration; everything else (ErrInvalidHash, Sscanf and
// base64 errors) is bad syntax.
func classify(err error) error {
	if errors.Is(err, argon2id.ErrIncompatibleVariant) || errors.Is(err, argon2id.ErrIncompatibleVersion) {
		return fmt.Errorf("%w: %v", ErrUnsupportedParams, err)
	}
	return fmt.Errorf("%w: %v", ErrMalformedHash, err)
}
