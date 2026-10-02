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
	ReasonBadStatus       Reason = "bad_status"        // neither 2xx nor 503
	ReasonBare503         Reason = "bare_503"          // 503 without Retry-After
	ReasonBadRetryAfter   Reason = "bad_retry_after"   // 503 with an unparseable Retry-After
	ReasonMalformedBody   Reason = "malformed_body"    // 2xx body breaks the HTTP contract
	ReasonWrongMatch      Reason = "wrong_match"       // verify: match != WantMatch
	ReasonNonCanonicalPHC Reason = "non_canonical_phc" // hash: not the canonical encoding
	ReasonSaltReused      Reason = "salt_reused"       // hash: salt already seen in this Run
	ReasonVerifyMismatch  Reason = "verify_mismatch"   // hash: Verifier returned (false, nil)
	ReasonVerifyError     Reason = "verify_error"      // hash: Verifier returned an error
)

// Kind is the endpoint an Observation came from. The zero value is not a
// valid Kind, so a forgotten Kind panics instead of being misclassified.
type Kind int

const (
	Hash   Kind = iota + 1 // POST /hash
	Verify                 // POST /verify
)

func (k Kind) String() string {
	switch k {
	case Hash:
		return "hash"
	case Verify:
		return "verify"
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
	Body   []byte

	// Latency is measured by the caller, from send to full body read;
	// the classifier times nothing.
	Latency time.Duration

	// Err is any transport error (connection refused/reset, timeout,
	// body read failure).
	Err error
}

// Verifier reports whether password matches encoded. gate and loadgen
// pass the fixed core's Verify; this package cannot import it.
type Verifier func(password, encoded string) (bool, error)

// Run classifies the responses of one run. It remembers every canonical
// salt seen in a 2xx hash body, on time or late, so a reused salt
// classifies as Failed. A run spans a variant's live lifetime, from
// promotion to cull, not one observation window, so hashes cached in an
// earlier window cannot be replayed later (DESIGN.md sec 8). Stage 5's
// burst uses its own Run. A Run is safe for concurrent use.
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
func (r *Run) Classify(o Observation) (Bucket, Reason) {
	if o.Kind != Hash && o.Kind != Verify {
		panic(fmt.Sprintf("classifier: invalid %v", o.Kind))
	}
	if o.FullVerify && r.verify == nil {
		panic("classifier: FullVerify set on a Run with a nil Verifier")
	}

	if o.Err != nil {
		return Failed, ReasonTransportError
	}
	if o.Latency > HangTimeout {
		// Record a late hash's salt, so it cannot be replayed on time.
		if o.Kind == Hash && is2xx(o.Status) {
			var encoded string
			if decodeField(o.Body, "hash", &encoded) {
				if salt, ok := canonicalSalt(encoded); ok {
					r.recordSalt(salt)
				}
			}
		}
		return Failed, ReasonOverHangTimeout
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
	if !is2xx(o.Status) {
		return Failed, ReasonBadStatus
	}

	if o.Kind == Verify {
		var match bool
		if !decodeField(o.Body, "match", &match) {
			return Failed, ReasonMalformedBody
		}
		if match != o.WantMatch {
			return Failed, ReasonWrongMatch
		}
		return Success, ReasonNone
	}

	var encoded string
	if !decodeField(o.Body, "hash", &encoded) {
		return Failed, ReasonMalformedBody
	}
	salt, ok := canonicalSalt(encoded)
	if !ok {
		return Failed, ReasonNonCanonicalPHC
	}
	if !r.recordSalt(salt) {
		return Failed, ReasonSaltReused
	}
	if o.FullVerify {
		// Outside the mutex: a Verifier call costs one Argon2 run.
		match, err := r.verify(o.Password, encoded)
		if err != nil {
			return Failed, ReasonVerifyError
		}
		if !match {
			return Failed, ReasonVerifyMismatch
		}
	}
	return Success, ReasonNone
}

func is2xx(status int) bool { return status >= 200 && status <= 299 }

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

// decodeField decodes body as exactly one JSON object whose only key is
// name (matched case-sensitively, unlike encoding/json's struct fields),
// with a non-null value of v's type, and no trailing data.
func decodeField[T any](body []byte, name string, v *T) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || len(obj) != 1 {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	raw, ok := obj[name]
	if !ok {
		return false
	}
	var p *T
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
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
