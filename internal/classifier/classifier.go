package classifier

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bucket is a response's classification (DESIGN.md sec 8). The zero
// value is Failed, so an unset Bucket fails closed.
type Bucket int

const (
	Failed Bucket = iota
	Success
	GracefulShed
)

func (b Bucket) String() string {
	switch b {
	case Failed:
		return "failed"
	case Success:
		return "success"
	case GracefulShed:
		return "graceful_shed"
	}
	return fmt.Sprintf("Bucket(%d)", int(b))
}

// Reason says why a response classified as Failed; it is ReasonNone
// ("") for Success and GracefulShed.
type Reason string

const (
	ReasonNone Reason = ""

	ReasonTransportError  Reason = "transport_error"   // Observation.Err != nil
	ReasonOverHangTimeout Reason = "over_hang_timeout" // Latency > HangTimeout
	ReasonBadStatus       Reason = "bad_status"        // neither 200 nor 503 (negative: see Classify)
	ReasonBare503         Reason = "bare_503"          // 503 without Retry-After
	ReasonBadRetryAfter   Reason = "bad_retry_after"   // 503 with an unparseable Retry-After
	ReasonMalformedBody   Reason = "malformed_body"    // 2xx body breaks the HTTP contract or MaxBodyBytes
	ReasonWrongMatch      Reason = "wrong_match"       // verify: match != WantMatch
	ReasonNonCanonicalPHC Reason = "non_canonical_phc" // hash: not the canonical encoding
	ReasonSaltReused      Reason = "salt_reused"       // hash: salt already seen in this Run
	ReasonVerifyMismatch  Reason = "verify_mismatch"   // hash: Verifier returned (false, nil)
	ReasonVerifyError     Reason = "verify_error"      // hash: Verifier returned an error

	ReasonNegativeAccepted Reason = "negative_accepted" // negative: any 2xx (fail-open)
)

// Integrity reports whether reason is an integrity failure (DESIGN.md
// sec 9): evidence that the wrapper is faking results, not one failed
// request. The first one, re-checked on the host, disqualifies the
// variant.
//
// ReasonMalformedBody is on the list because, under the caller's
// contract on Observation.Body, Classify sees it only for a body the
// caller read without error: a caller whose body read failed sets
// Observation.Err, which classifies as ReasonTransportError. Classify
// cannot check that contract. It does check MaxBodyBytes: a 2xx body
// over it is the wrapper's doing, not the transport's, so it is
// malformed_body.
func Integrity(reason Reason) bool {
	switch reason {
	case ReasonVerifyMismatch, ReasonVerifyError, ReasonWrongMatch,
		ReasonNonCanonicalPHC, ReasonSaltReused, ReasonNegativeAccepted,
		ReasonMalformedBody:
		return true
	}
	return false
}

// Kind is the endpoint an Observation came from. The zero value is not a
// valid Kind, so a forgotten Kind panics instead of being misclassified.
type Kind int

const (
	Hash     Kind = iota + 1 // POST /hash
	Verify                   // POST /verify
	Negative                 // POST /verify that must be rejected (a contract negative)
)

func (k Kind) String() string {
	switch k {
	case Hash:
		return "hash"
	case Verify:
		return "verify"
	case Negative:
		return "negative"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Observation is one request and what the client saw in response.
type Observation struct {
	Kind Kind

	// Password is the password sent; the Verifier checks a hash
	// response against it.
	Password string

	// WantMatch is the ground truth for a Verify request.
	WantMatch bool

	// FullVerify asks for a hash response to be checked with the Run's
	// Verifier (always in Stage 5, sampled under sustained load).
	FullVerify bool

	Status int
	Header http.Header

	// Body is the response body. The caller reads at most
	// MaxBodyBytes+1 bytes and passes what it read with Err nil,
	// whether or not it hit that cap: a 2xx body longer than
	// MaxBodyBytes is malformed_body (an integrity failure, see
	// Integrity); a non-2xx body is not size-checked. A caller whose
	// read failed must set Err instead of passing what it got: a body
	// cut short by a failed read is a transport error, never
	// malformed_body.
	Body []byte

	// Latency is measured by the caller, from send to full body read;
	// the classifier times nothing.
	Latency time.Duration

	// Err is any transport error (connection refused/reset, timeout,
	// body read failure). Reaching the MaxBodyBytes+1 read cap is not
	// one.
	Err error
}

// Verifier reports whether password matches encoded. gate and loadgen
// pass the fixed core's Verify; this package cannot import it.
type Verifier func(password, encoded string) (bool, error)

// Run classifies the responses of one run. It remembers the salt of
// each 2xx /hash answer whose body passes the body checks and whose
// hash is canonical, on time or late and whatever the later checks
// find, so a second response with that salt is salt_reused. The salt
// set spans every response the Run classifies; the caller sets the
// Run's lifetime (loadgen keeps one per variant for as long as it
// serves it, DESIGN.md sec 8, 10). A Run is safe for concurrent use.
type Run struct {
	verify Verifier

	mu    sync.Mutex
	salts map[string]struct{}
}

// NewRun returns a Run that uses verify for FullVerify observations.
// verify may be nil if no observation sets FullVerify.
func NewRun(verify Verifier) *Run {
	return &Run{verify: verify, salts: make(map[string]struct{})}
}

// Classify returns o's bucket and, for Failed, the reason. It panics if
// o.Kind is invalid, or if o.FullVerify is set and the Run has no
// Verifier: both are caller bugs, not response properties.
//
// The order is: a transport error; then, for a 2xx, the checks that
// can yield an integrity reason (see Integrity), at any latency, since
// a late fail-open or faked result is still a bypass: the MaxBodyBytes
// cap, then the body; then latency over HangTimeout; then the ordinary
// buckets. HangTimeout decides only whether a response that passes
// them can be Success or GracefulShed. Only 200 is Success: any other
// 2xx that passes the checks before it is bad_status. A 2xx hash's
// canonical salt is recorded whatever its status or latency, so a late
// or non-200 hash cannot be replayed as an on-time 200.
//
// A Negative's body is never read: any 2xx is negative_accepted; past
// the latency check, a 503 with exactly one valid Retry-After is
// GracefulShed, a 400 is Success (the negative passed), and anything
// else, a 503 without a valid Retry-After included, is bad_status.
// FullVerify has no effect on a Negative beyond the nil-Verifier
// panic. Negatives are outside success_rate, p95 and cpu_per_success
// (DESIGN.md sec 9): the caller keeps them apart.
func (r *Run) Classify(o Observation) (Bucket, Reason) {
	if o.Kind != Hash && o.Kind != Verify && o.Kind != Negative {
		panic(fmt.Sprintf("classifier: invalid %v", o.Kind))
	}
	if o.FullVerify && r.verify == nil {
		panic("classifier: FullVerify set on a Run with a nil Verifier")
	}

	if o.Err != nil {
		return Failed, ReasonTransportError
	}
	if is2xx(o.Status) {
		if reason := r.integrity2xx(o); reason != ReasonNone {
			return Failed, reason
		}
	}
	if o.Latency > HangTimeout {
		return Failed, ReasonOverHangTimeout
	}

	if o.Kind == Negative {
		switch {
		case o.Status == http.StatusServiceUnavailable && singleValidRetryAfter(o.Header):
			return GracefulShed, ReasonNone
		case o.Status == http.StatusBadRequest:
			return Success, ReasonNone
		}
		return Failed, ReasonBadStatus
	}

	if o.Status == http.StatusServiceUnavailable {
		vals := o.Header.Values("Retry-After")
		switch {
		case len(vals) == 0:
			return Failed, ReasonBare503
		case len(vals) > 1 || !validRetryAfter(vals[0]):
			return Failed, ReasonBadRetryAfter
		}
		return GracefulShed, ReasonNone
	}
	if o.Status != http.StatusOK {
		return Failed, ReasonBadStatus
	}
	return Success, ReasonNone
}

// integrity2xx runs every check of a 2xx response that can yield an
// integrity reason, whatever its status or latency, and returns the
// first that fires, or ReasonNone. For a hash it records the salt.
func (r *Run) integrity2xx(o Observation) Reason {
	if o.Kind == Negative {
		return ReasonNegativeAccepted
	}
	if len(o.Body) > MaxBodyBytes {
		return ReasonMalformedBody
	}
	if o.Kind == Verify {
		var match bool
		if !decodeField(o.Body, "match", &match) {
			return ReasonMalformedBody
		}
		if match != o.WantMatch {
			return ReasonWrongMatch
		}
		return ReasonNone
	}

	var encoded string
	if !decodeField(o.Body, "hash", &encoded) {
		return ReasonMalformedBody
	}
	salt, ok := canonicalSalt(encoded)
	if !ok {
		return ReasonNonCanonicalPHC
	}
	if !r.recordSalt(salt) {
		return ReasonSaltReused
	}
	if o.FullVerify {
		// Outside the mutex: a Verifier call costs one Argon2 run.
		match, err := r.verify(o.Password, encoded)
		if err != nil {
			return ReasonVerifyError
		}
		if !match {
			return ReasonVerifyMismatch
		}
	}
	return ReasonNone
}

func is2xx(status int) bool { return status >= 200 && status <= 299 }

// singleValidRetryAfter reports whether h holds exactly one Retry-After
// value and it is valid.
func singleValidRetryAfter(h http.Header) bool {
	vals := h.Values("Retry-After")
	return len(vals) == 1 && validRetryAfter(vals[0])
}

// recordSalt adds salt to the Run's set and reports whether it was new.
func (r *Run) recordSalt(salt string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.salts[salt]; seen {
		return false
	}
	r.salts[salt] = struct{}{}
	return true
}

// validRetryAfter reports whether v is delay-seconds (one or more
// digits) or an HTTP-date (RFC 9110 sec 10.2.3).
func validRetryAfter(v string) bool {
	if v != "" && strings.Trim(v, "0123456789") == "" {
		return true
	}
	_, err := http.ParseTime(v)
	return err == nil
}

// decodeField decodes body as exactly one JSON object with exactly one
// member, whose key is name (after unescaping, matched case-sensitively,
// unlike encoding/json's struct fields) and whose value is non-null and
// of v's type, with nothing but JSON whitespace around it. A second
// member, a duplicate of name included, is rejected: the walk never
// keeps one value of several. The body must be valid UTF-8, which
// encoding/json would otherwise quietly replace with U+FFFD.
func decodeField[T any](body []byte, name string, v *T) bool {
	if !utf8.Valid(body) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	if tok, err := dec.Token(); err != nil || tok != name {
		return false
	}
	var p *T
	if err := dec.Decode(&p); err != nil || p == nil {
		return false
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	*v = *p
	return true
}

// The canonical encoding of the DESIGN.md sec 6 parameters. Kept in
// step with internal/hashcore's checkParams by an agreement test, not
// by import (this package must not import hashcore).
const (
	variantSegment = "argon2id"
	versionSegment = "v=19"
	paramsSegment  = "m=19456,t=2,p=1"
	saltLength     = 16
	keyLength      = 32
)

// canonicalSalt returns encoded's salt segment if encoded is the
// canonical PHC string at exactly the fixed parameters.
func canonicalSalt(encoded string) (string, bool) {
	segs := strings.Split(encoded, "$")
	if len(segs) != 6 || segs[0] != "" ||
		segs[1] != variantSegment || segs[2] != versionSegment || segs[3] != paramsSegment {
		return "", false
	}
	if !canonicalBase64(segs[4], saltLength) || !canonicalBase64(segs[5], keyLength) {
		return "", false
	}
	return segs[4], true
}

// canonicalBase64 reports whether s is the unpadded standard base64
// encoding of exactly n bytes. Re-encoding catches what decoding
// tolerates: ignored CR/LF and non-zero trailing bits.
func canonicalBase64(s string, n int) bool {
	b, err := base64.RawStdEncoding.DecodeString(s)
	return err == nil && len(b) == n && base64.RawStdEncoding.EncodeToString(b) == s
}
