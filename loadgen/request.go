package loadgen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	mrand "math/rand/v2"
	"strings"
)

// The contract endpoints (DESIGN.md sec 8).
const (
	HashPath   = "/hash"
	VerifyPath = "/verify"
)

// Password lengths, in characters. Passwords are base32 text, so
// characters are also bytes (DESIGN.md sec 10). The uniform 16 to 1024
// distribution is a starting guess (DESIGN.md sec 15).
const (
	minPasswordLen = 16
	maxPasswordLen = 1024 // the HTTP contract's cap (DESIGN.md sec 8)
)

// The fixed parameters as hashcore accepts them (DESIGN.md sec 6). They
// are restated here because loadgen cannot import hashcore; the hashcore
// agreement test catches any drift.
const (
	exactVariant = "argon2id"
	exactVersion = "v=19"
	exactParams  = "m=19456,t=2,p=1"
	saltLen      = 16
	keyLen       = 32
)

// Kind is what a request is for. Mapping it onto internal/classifier's
// kinds is left to the response checks.
type Kind string

const (
	KindHash     Kind = "hash"     // a /hash request with a valid password
	KindVerify   Kind = "verify"   // a /verify request from a host pair
	KindNegative Kind = "negative" // a contract negative (DESIGN.md sec 8), /hash or /verify
)

// String returns k as the control API's records carry it.
func (k Kind) String() string { return string(k) }

// Pair is one host-generated /verify pair (DESIGN.md sec 10).
type Pair struct {
	Password string
	Hash     string
	Match    bool // ground truth: whether Hash is a hash of Password
}

// Request is one request to send, with what its verdict needs.
type Request struct {
	Path string
	Body []byte
	Kind Kind

	// Class is set only when Kind is KindNegative.
	Class NegativeClass

	// Password is the password the request was built with, "" for none.
	// For a /hash request it is the one the response's hash must verify
	// against; for a negative it is informational only.
	Password string

	// Match is the expected /verify result; set only when Kind is
	// KindVerify.
	Match bool
}

func hashRequest(password string) Request {
	return Request{
		Path:     HashPath,
		Body:     hashBody(password),
		Kind:     KindHash,
		Password: password,
	}
}

func verifyRequest(p Pair) Request {
	return Request{
		Path:     VerifyPath,
		Body:     verifyBody(p.Password, p.Hash),
		Kind:     KindVerify,
		Password: p.Password,
		Match:    p.Match,
	}
}

func hashBody(password string) []byte {
	return mustJSON(struct {
		Password string `json:"password"`
	}{password})
}

func verifyBody(password, hash string) []byte {
	return mustJSON(struct {
		Password string `json:"password"`
		Hash     string `json:"hash"`
	}{password, hash})
}

// mustJSON marshals v, which is only ever made of strings, numbers,
// bools, slices and maps of them, so encoding cannot fail.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// newPassword returns a fresh password of a length drawn uniformly from
// minPasswordLen to maxPasswordLen inclusive.
func newPassword() string { return passwordOfLen(passwordLen()) }

func passwordLen() int {
	return minPasswordLen + mrand.IntN(maxPasswordLen-minPasswordLen+1)
}

// passwordOfLen returns n characters of crypto/rand.Text output (RFC 4648
// base32: A-Z and 2-7). It loops rather than assuming Text's length,
// since its doc allows longer output in future.
func passwordOfLen(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(rand.Text())
	}
	return b.String()[:n]
}

// randomBytes returns n bytes from crypto/rand. crypto/rand.Read never
// returns an error: since Go 1.24 it crashes the program instead.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

// phc formats a PHC string from its segments.
func phc(variant, version, params, salt, key string) string {
	return "$" + variant + "$" + version + "$" + params + "$" + salt + "$" + key
}

// exactPHC formats a PHC string at the fixed parameters.
func exactPHC(salt, key string) string {
	return phc(exactVariant, exactVersion, exactParams, salt, key)
}

// randomExactPHC returns a canonical PHC string at the fixed parameters
// with a random salt and key: well-formed, but no Argon2 ran, so it is a
// hash of no known password.
func randomExactPHC() string {
	return exactPHC(b64(randomBytes(saltLen)), b64(randomBytes(keyLen)))
}
