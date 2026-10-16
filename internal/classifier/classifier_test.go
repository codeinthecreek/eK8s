package classifier

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// phc hand-builds a canonical PHC string at the fixed parameters; no
// Argon2 runs, so the key is arbitrary.
func phc(saltByte byte) string {
	return "$argon2id$v=19$m=19456,t=2,p=1$" + b64(bytes.Repeat([]byte{saltByte}, 16)) + "$" + b64(bytes.Repeat([]byte{0xAB}, 32))
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

var (
	salt16 = b64(bytes.Repeat([]byte{1}, 16))
	key32  = b64(bytes.Repeat([]byte{0xAB}, 32))
)

// withSalt and withKey replace one segment of an otherwise canonical
// string.
func withSalt(s string) string { return "$argon2id$v=19$m=19456,t=2,p=1$" + s + "$" + key32 }
func withKey(k string) string  { return "$argon2id$v=19$m=19456,t=2,p=1$" + salt16 + "$" + k }

func hashBody(encoded string) []byte { return []byte(`{"hash":"` + encoded + `"}`) }

func retryAfter(vals ...string) http.Header {
	h := http.Header{}
	for _, v := range vals {
		h.Add("Retry-After", v)
	}
	return h
}

// fakeVerifier records calls and returns a fixed result.
type fakeVerifier struct {
	match bool
	err   error
	calls atomic.Int32
}

func (f *fakeVerifier) verify(string, string) (bool, error) {
	f.calls.Add(1)
	return f.match, f.err
}

func TestClassify(t *testing.T) {
	okHash := Observation{Kind: Hash, Password: "pw", Status: 200, Body: hashBody(phc(1))}
	okVerify := Observation{Kind: Verify, Password: "pw", WantMatch: true, Status: 200, Body: []byte(`{"match":true}`)}
	shed := func(h http.Header) Observation {
		return Observation{Kind: Hash, Status: 503, Header: h, Body: []byte("not json")}
	}
	negative := func(status int, h http.Header) Observation {
		return Observation{Kind: Negative, Password: "pw", Status: status, Header: h, Body: []byte(`{"match":true}`)}
	}
	with := func(o Observation, f func(*Observation)) Observation { f(&o); return o }

	tests := []struct {
		name         string
		prior        *Observation // classified first, on the same Run
		o            Observation
		verifier     *fakeVerifier
		wantBucket   Bucket
		wantReason   Reason
		wantVerifies int32
	}{
		// Rule 1: transport errors and the hang-timeout, for every bucket.
		{name: "transport error", o: with(okHash, func(o *Observation) { o.Err = errors.New("reset") }), wantBucket: Failed, wantReason: ReasonTransportError},
		{name: "transport error wins over latency", o: with(okHash, func(o *Observation) { o.Err = errors.New("reset"); o.Latency = time.Hour }), wantBucket: Failed, wantReason: ReasonTransportError},
		{name: "latency at ceiling", o: with(okHash, func(o *Observation) { o.Latency = HangTimeout }), wantBucket: Success},
		{name: "hash over ceiling", o: with(okHash, func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonOverHangTimeout},
		{name: "verify over ceiling", o: with(okVerify, func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonOverHangTimeout},
		{name: "shed over ceiling", o: with(shed(retryAfter("1")), func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonOverHangTimeout},

		// Rule 2: 503 needs a parseable Retry-After; the body is ignored.
		{name: "shed delay-seconds", o: shed(retryAfter("1")), wantBucket: GracefulShed},
		{name: "shed delay-seconds zero", o: shed(retryAfter("0")), wantBucket: GracefulShed},
		{name: "shed delay-seconds huge", o: shed(retryAfter("99999999999999999999999")), wantBucket: GracefulShed},
		{name: "shed IMF-fixdate", o: shed(retryAfter("Fri, 02 Oct 2026 09:00:00 GMT")), wantBucket: GracefulShed},
		{name: "shed RFC 850 date", o: shed(retryAfter("Friday, 02-Oct-26 09:00:00 GMT")), wantBucket: GracefulShed},
		{name: "shed asctime date", o: shed(retryAfter("Fri Oct  2 09:00:00 2026")), wantBucket: GracefulShed},
		{name: "shed on verify", o: with(shed(retryAfter("1")), func(o *Observation) { o.Kind = Verify }), wantBucket: GracefulShed},
		{name: "bare 503", o: shed(nil), wantBucket: Failed, wantReason: ReasonBare503},
		{name: "bare 503 other headers", o: shed(http.Header{"Content-Type": {"application/json"}}), wantBucket: Failed, wantReason: ReasonBare503},
		{name: "Retry-After empty", o: shed(retryAfter("")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After negative", o: shed(retryAfter("-1")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After fraction", o: shed(retryAfter("1.5")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After plus sign", o: shed(retryAfter("+1")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After garbage", o: shed(retryAfter("soon")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After twice", o: shed(retryAfter("1", "2")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},
		{name: "Retry-After list in one value", o: shed(retryAfter("1, 2")), wantBucket: Failed, wantReason: ReasonBadRetryAfter},

		// Rule 3: any other non-2xx.
		{name: "500 with Retry-After", o: with(okHash, func(o *Observation) { o.Status = 500; o.Header = retryAfter("1") }), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "400", o: with(okVerify, func(o *Observation) { o.Status = 400 }), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "302", o: with(okHash, func(o *Observation) { o.Status = 302 }), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "status 0", o: with(okHash, func(o *Observation) { o.Status = 0 }), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "204 empty body", o: with(okHash, func(o *Observation) { o.Status = 204; o.Body = nil }), wantBucket: Failed, wantReason: ReasonMalformedBody},

		// Rule 4: strict 2xx bodies.
		{name: "empty body", o: with(okVerify, func(o *Observation) { o.Body = nil }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "not JSON", o: with(okVerify, func(o *Observation) { o.Body = []byte("true") }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "JSON null", o: with(okVerify, func(o *Observation) { o.Body = []byte("null") }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "array", o: with(okVerify, func(o *Observation) { o.Body = []byte(`[{"match":true}]`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "missing field", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "wrong case field", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"Match":true}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "other endpoint's field", o: with(okVerify, func(o *Observation) { o.Body = hashBody(phc(1)) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "unknown field", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":true,"x":1}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "null match", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":null}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "string match", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":"true"}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "number hash", o: with(okHash, func(o *Observation) { o.Body = []byte(`{"hash":1}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "null hash", o: with(okHash, func(o *Observation) { o.Body = []byte(`{"hash":null}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "trailing data", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":true} {}`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "trailing garbage", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":true}x`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "truncated", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":tr`) }), wantBucket: Failed, wantReason: ReasonMalformedBody},
		{name: "trailing newline ok", o: with(okVerify, func(o *Observation) { o.Body = []byte("{\"match\":true}\n") }), wantBucket: Success},

		// Rule 5: verify ground truth.
		{name: "verify match wanted", o: okVerify, wantBucket: Success},
		{name: "verify reject wanted", o: with(okVerify, func(o *Observation) { o.WantMatch = false; o.Body = []byte(`{"match":false}`) }), wantBucket: Success},
		{name: "verify accepts should-reject", o: with(okVerify, func(o *Observation) { o.WantMatch = false }), wantBucket: Failed, wantReason: ReasonWrongMatch},
		{name: "verify rejects should-match", o: with(okVerify, func(o *Observation) { o.Body = []byte(`{"match":false}`) }), wantBucket: Failed, wantReason: ReasonWrongMatch},

		// Rule 6: canonical PHC.
		{name: "hash ok", o: okHash, wantBucket: Success},
		{name: "empty hash", o: with(okHash, func(o *Observation) { o.Body = hashBody("") }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "too few segments", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2id$v=19$m=19456,t=2,p=1$" + salt16) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "too many segments", o: with(okHash, func(o *Observation) { o.Body = hashBody(phc(1) + "$") }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "non-empty first segment", o: with(okHash, func(o *Observation) { o.Body = hashBody("x" + phc(1)) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "argon2i", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2i$v=19$m=19456,t=2,p=1$" + salt16 + "$" + key32) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "v=16", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2id$v=16$m=19456,t=2,p=1$" + salt16 + "$" + key32) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "t=1", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2id$v=19$m=19456,t=1,p=1$" + salt16 + "$" + key32) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "m=019456", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2id$v=19$m=019456,t=2,p=1$" + salt16 + "$" + key32) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "params reordered", o: with(okHash, func(o *Observation) { o.Body = hashBody("$argon2id$v=19$t=2,m=19456,p=1$" + salt16 + "$" + key32) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt 15 bytes", o: with(okHash, func(o *Observation) { o.Body = hashBody(withSalt(b64(make([]byte, 15)))) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt 17 bytes", o: with(okHash, func(o *Observation) { o.Body = hashBody(withSalt(b64(make([]byte, 17)))) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "key 31 bytes", o: with(okHash, func(o *Observation) { o.Body = hashBody(withKey(b64(make([]byte, 31)))) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt padded", o: with(okHash, func(o *Observation) { o.Body = hashBody(withSalt(base64.StdEncoding.EncodeToString(make([]byte, 16)))) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt URL alphabet", o: with(okHash, func(o *Observation) {
			o.Body = hashBody(withSalt(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xFF}, 16))))
		}), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt with escaped newline", o: with(okHash, func(o *Observation) { o.Body = hashBody(withSalt(salt16[:4] + `\n` + salt16[4:])) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		// salt16 ends in "Q"; "R" decodes to the same bytes plus non-zero
		// trailing bits the decoder ignores: only re-encoding catches it.
		{name: "salt non-zero trailing bits", o: with(okHash, func(o *Observation) { o.Body = hashBody(withSalt(salt16[:21] + "R")) }), wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},

		// Rule 6: full verification, only when asked.
		{name: "full verify match", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: true}, wantBucket: Success, wantVerifies: 1},
		{name: "full verify mismatch", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: false}, wantBucket: Failed, wantReason: ReasonVerifyMismatch, wantVerifies: 1},
		{name: "full verify error", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{err: errors.New("bad")}, wantBucket: Failed, wantReason: ReasonVerifyError, wantVerifies: 1},
		{name: "full verify match with error", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: true, err: errors.New("bad")}, wantBucket: Failed, wantReason: ReasonVerifyError, wantVerifies: 1},
		{name: "no full verify skips verifier", o: okHash, verifier: &fakeVerifier{match: false}, wantBucket: Success},
		{name: "non-canonical never verified", o: with(okHash, func(o *Observation) { o.FullVerify = true; o.Body = hashBody(withKey("x")) }), verifier: &fakeVerifier{match: true}, wantBucket: Failed, wantReason: ReasonNonCanonicalPHC},
		{name: "salt reused", prior: &okHash, o: okHash, wantBucket: Failed, wantReason: ReasonSaltReused},
		{name: "verify kind ignores verifier", o: with(okVerify, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: false}, wantBucket: Success},

		// Negative, in Classify's order.
		{name: "negative transport error", o: with(negative(400, nil), func(o *Observation) { o.Err = errors.New("reset") }), wantBucket: Failed, wantReason: ReasonTransportError},
		{name: "negative transport error wins over 2xx", o: with(negative(200, nil), func(o *Observation) { o.Err = errors.New("reset") }), wantBucket: Failed, wantReason: ReasonTransportError},
		{name: "negative 400 at ceiling", o: with(negative(400, nil), func(o *Observation) { o.Latency = HangTimeout }), wantBucket: Success},
		{name: "negative 400 over ceiling", o: with(negative(400, nil), func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonOverHangTimeout},
		{name: "negative shed at ceiling", o: with(negative(503, retryAfter("1")), func(o *Observation) { o.Latency = HangTimeout }), wantBucket: GracefulShed},
		{name: "negative shed over ceiling", o: with(negative(503, retryAfter("1")), func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonOverHangTimeout},
		{name: "negative 2xx at ceiling", o: with(negative(200, nil), func(o *Observation) { o.Latency = HangTimeout }), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 2xx over ceiling", o: with(negative(200, nil), func(o *Observation) { o.Latency = HangTimeout + 1 }), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative shed delay-seconds", o: negative(503, retryAfter("1")), wantBucket: GracefulShed},
		{name: "negative shed HTTP-date", o: negative(503, retryAfter("Fri, 02 Oct 2026 09:00:00 GMT")), wantBucket: GracefulShed},
		{name: "negative bare 503", o: negative(503, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative Retry-After twice", o: negative(503, retryAfter("1", "1")), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative Retry-After garbage", o: negative(503, retryAfter("soon")), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative Retry-After empty", o: negative(503, retryAfter("")), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative 400", o: negative(400, nil), wantBucket: Success},
		{name: "negative 400 with Retry-After", o: negative(400, retryAfter("1")), wantBucket: Success},
		{name: "negative 400 body ignored", o: with(negative(400, nil), func(o *Observation) { o.Body = []byte("garbage") }), wantBucket: Success},
		{name: "negative 200", o: negative(200, nil), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 200 match false", o: with(negative(200, nil), func(o *Observation) { o.Body = []byte(`{"match":false}`) }), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 204 empty body", o: with(negative(204, nil), func(o *Observation) { o.Body = nil }), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 299", o: negative(299, nil), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 2xx with Retry-After", o: negative(200, retryAfter("1")), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 300", o: negative(300, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative 199", o: negative(199, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative 401", o: negative(401, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative 422", o: negative(422, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative 500 with Retry-After", o: negative(500, retryAfter("1")), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative status 0", o: negative(0, nil), wantBucket: Failed, wantReason: ReasonBadStatus},
		{name: "negative ignores verifier", o: with(negative(400, nil), func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: false}, wantBucket: Success},
		{name: "negative 2xx ignores verifier", o: with(negative(200, nil), func(o *Observation) { o.FullVerify = true; o.Body = hashBody(phc(1)) }), verifier: &fakeVerifier{match: true}, wantBucket: Failed, wantReason: ReasonNegativeAccepted},
		{name: "negative 2xx with a hash body", prior: &okHash, o: with(negative(200, nil), func(o *Observation) { o.Body = hashBody(phc(1)) }), wantBucket: Failed, wantReason: ReasonNegativeAccepted},
	}
	covered := map[Reason]bool{}
	for _, tt := range tests {
		covered[tt.wantReason] = true
		t.Run(tt.name, func(t *testing.T) {
			v := tt.verifier
			if v == nil {
				v = &fakeVerifier{}
			}
			r := NewRun(v.verify)
			if tt.prior != nil {
				r.Classify(*tt.prior)
			}
			b, reason := r.Classify(tt.o)
			if b != tt.wantBucket || reason != tt.wantReason {
				t.Errorf("Classify = (%v, %q), want (%v, %q)", b, reason, tt.wantBucket, tt.wantReason)
			}
			if got := v.calls.Load(); got != tt.wantVerifies {
				t.Errorf("Verifier calls = %d, want %d", got, tt.wantVerifies)
			}
		})
	}
	for _, r := range []Reason{ReasonNone, ReasonTransportError, ReasonOverHangTimeout, ReasonBadStatus, ReasonBare503,
		ReasonBadRetryAfter, ReasonMalformedBody, ReasonWrongMatch, ReasonNonCanonicalPHC, ReasonSaltReused,
		ReasonVerifyMismatch, ReasonVerifyError, ReasonNegativeAccepted} {
		if !covered[r] {
			t.Errorf("no test case for reason %q", r)
		}
	}
}

func TestSaltReuse(t *testing.T) {
	v := &fakeVerifier{match: false}
	r := NewRun(v.verify)
	first := Observation{Kind: Hash, Status: 200, Body: hashBody(phc(7)), FullVerify: true}
	if b, reason := r.Classify(first); reason != ReasonVerifyMismatch {
		t.Fatalf("first = (%v, %q), want verify mismatch", b, reason)
	}
	// The salt was recorded although the first response failed later.
	second := Observation{Kind: Hash, Status: 200, Body: hashBody(phc(7))}
	if b, reason := r.Classify(second); b != Failed || reason != ReasonSaltReused {
		t.Fatalf("second = (%v, %q), want (failed, salt_reused)", b, reason)
	}
	// Same salt, different key: still reused.
	third := Observation{Kind: Hash, Status: 200, Body: hashBody("$argon2id$v=19$m=19456,t=2,p=1$" + b64(bytes.Repeat([]byte{7}, 16)) + "$" + b64(make([]byte, 32)))}
	if _, reason := r.Classify(third); reason != ReasonSaltReused {
		t.Fatalf("third reason = %q, want salt_reused", reason)
	}
	if b, _ := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(8))}); b != Success {
		t.Fatalf("new salt = %v, want success", b)
	}
	// Salts are per Run.
	if b, _ := NewRun(nil).Classify(second); b != Success {
		t.Fatalf("fresh Run = %v, want success", b)
	}
}

// TestFullVerifyFailureKeepsSalt: FullVerify is the one check that runs
// after the salt is recorded, so a hash that fails it still holds its
// salt.
func TestFullVerifyFailureKeepsSalt(t *testing.T) {
	tests := []struct {
		name       string
		verifier   *fakeVerifier
		wantReason Reason
	}{
		{"verify_mismatch", &fakeVerifier{match: false}, ReasonVerifyMismatch},
		{"verify_error", &fakeVerifier{err: errors.New("bad")}, ReasonVerifyError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRun(tt.verifier.verify)
			o := Observation{Kind: Hash, Password: "pw", Status: 200, Body: hashBody(phc(3)), FullVerify: true}
			if b, reason := r.Classify(o); b != Failed || reason != tt.wantReason {
				t.Fatalf("FullVerify = (%v, %q), want (failed, %q)", b, reason, tt.wantReason)
			}
			o.FullVerify = false
			if b, reason := r.Classify(o); b != Failed || reason != ReasonSaltReused {
				t.Fatalf("replay = (%v, %q), want (failed, salt_reused)", b, reason)
			}
		})
	}
}

func TestLateHashSaltRecorded(t *testing.T) {
	r := NewRun(nil)
	late := Observation{Kind: Hash, Status: 200, Body: hashBody(phc(5)), Latency: HangTimeout + 1}
	if b, reason := r.Classify(late); b != Failed || reason != ReasonOverHangTimeout {
		t.Fatalf("late = (%v, %q), want (failed, over_hang_timeout)", b, reason)
	}
	onTime := late
	onTime.Latency = 0
	if b, reason := r.Classify(onTime); b != Failed || reason != ReasonSaltReused {
		t.Fatalf("replayed on time = (%v, %q), want (failed, salt_reused)", b, reason)
	}

	// Late responses that are not 2xx canonical hash bodies record nothing.
	r = NewRun(nil)
	for _, tt := range []struct {
		o          Observation
		wantReason Reason
	}{
		{Observation{Kind: Hash, Status: 503, Header: retryAfter("1"), Body: hashBody(phc(6)), Latency: HangTimeout + 1}, ReasonOverHangTimeout},
		{Observation{Kind: Verify, Status: 200, Body: hashBody(phc(6)), Latency: HangTimeout + 1}, ReasonMalformedBody},
		{Observation{Kind: Negative, Status: 200, Body: hashBody(phc(6)), Latency: HangTimeout + 1}, ReasonNegativeAccepted},
		{Observation{Kind: Hash, Status: 200, Body: []byte(`{"hash":"` + phc(6) + `","x":1}`), Latency: HangTimeout + 1}, ReasonMalformedBody},
	} {
		if _, reason := r.Classify(tt.o); reason != tt.wantReason {
			t.Fatalf("late %v status %d: reason = %q, want %q", tt.o.Kind, tt.o.Status, reason, tt.wantReason)
		}
	}
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(6))}); b != Success {
		t.Fatalf("on time after non-recording late responses = (%v, %q), want success", b, reason)
	}
}

// TestIntegrityAtAnyLatency: every integrity reason fires the same at
// exactly HangTimeout and just over it; a late response that passes
// every integrity check is over_hang_timeout.
func TestIntegrityAtAnyLatency(t *testing.T) {
	okHash := Observation{Kind: Hash, Password: "pw", Status: 200, Body: hashBody(phc(1))}
	with := func(o Observation, f func(*Observation)) Observation { f(&o); return o }
	tests := []struct {
		name       string
		prior      *Observation // classified first, on time, on the same Run
		o          Observation
		verifier   *fakeVerifier
		wantReason Reason
	}{
		{name: "negative_accepted", o: Observation{Kind: Negative, Status: 200}, wantReason: ReasonNegativeAccepted},
		{name: "malformed_body verify", o: Observation{Kind: Verify, WantMatch: true, Status: 200, Body: []byte(`{"match":true,"x":1}`)}, wantReason: ReasonMalformedBody},
		{name: "malformed_body hash", o: with(okHash, func(o *Observation) { o.Body = []byte(`{"hash":1}`) }), wantReason: ReasonMalformedBody},
		{name: "wrong_match", o: Observation{Kind: Verify, WantMatch: false, Status: 200, Body: []byte(`{"match":true}`)}, wantReason: ReasonWrongMatch},
		{name: "non_canonical_phc", o: with(okHash, func(o *Observation) { o.Body = hashBody(withKey("x")) }), wantReason: ReasonNonCanonicalPHC},
		{name: "salt_reused", prior: &okHash, o: okHash, wantReason: ReasonSaltReused},
		{name: "verify_mismatch", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: false}, wantReason: ReasonVerifyMismatch},
		{name: "verify_error", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{err: errors.New("bad")}, wantReason: ReasonVerifyError},
		// Clean 2xx responses: latency decides.
		{name: "clean hash", o: with(okHash, func(o *Observation) { o.FullVerify = true }), verifier: &fakeVerifier{match: true}},
		{name: "clean verify", o: Observation{Kind: Verify, WantMatch: true, Status: 200, Body: []byte(`{"match":true}`)}},
	}
	covered := map[Reason]bool{}
	for _, tt := range tests {
		covered[tt.wantReason] = true
		for _, latency := range []time.Duration{HangTimeout, HangTimeout + 1} {
			t.Run(fmt.Sprintf("%s at %v", tt.name, latency), func(t *testing.T) {
				v := &fakeVerifier{}
				if tt.verifier != nil {
					v = &fakeVerifier{match: tt.verifier.match, err: tt.verifier.err}
				}
				r := NewRun(v.verify)
				if tt.prior != nil {
					r.Classify(*tt.prior)
				}
				o := tt.o
				o.Latency = latency
				want := tt.wantReason
				if want == ReasonNone && latency > HangTimeout {
					want = ReasonOverHangTimeout
				}
				wantBucket := Failed
				if want == ReasonNone {
					wantBucket = Success
				}
				if b, reason := r.Classify(o); b != wantBucket || reason != want {
					t.Errorf("Classify = (%v, %q), want (%v, %q)", b, reason, wantBucket, want)
				}
				if o.FullVerify && v.calls.Load() != 1 {
					t.Errorf("Verifier calls = %d, want 1", v.calls.Load())
				}
			})
		}
	}
	for r := range covered {
		if r != ReasonNone && !Integrity(r) {
			t.Errorf("%q is not an integrity reason", r)
		}
	}
	for _, r := range []Reason{ReasonVerifyMismatch, ReasonVerifyError, ReasonWrongMatch,
		ReasonNonCanonicalPHC, ReasonSaltReused, ReasonNegativeAccepted, ReasonMalformedBody} {
		if !covered[r] {
			t.Errorf("no test case for integrity reason %q", r)
		}
	}
}

// TestNegativeRecordsNoSalt: a negative's 2xx body is never read, so
// a hash in it records no salt.
func TestNegativeRecordsNoSalt(t *testing.T) {
	r := NewRun(nil)
	if _, reason := r.Classify(Observation{Kind: Negative, Status: 200, Body: hashBody(phc(6))}); reason != ReasonNegativeAccepted {
		t.Fatalf("reason = %q, want negative_accepted", reason)
	}
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(6))}); b != Success {
		t.Fatalf("hash after negative = (%v, %q), want success", b, reason)
	}
}

func TestSaltNotRecordedWhenNonCanonical(t *testing.T) {
	r := NewRun(nil)
	for _, bad := range []string{
		withKey(b64(make([]byte, 31))),                           // canonical salt16, bad key
		"$argon2id$v=19$m=19456,t=1,p=1$" + salt16 + "$" + key32, // non-exact params
	} {
		if _, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(bad)}); reason != ReasonNonCanonicalPHC {
			t.Fatalf("%q: reason = %q, want non_canonical_phc", bad, reason)
		}
	}
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(withSalt(salt16))}); b != Success {
		t.Fatalf("= (%v, %q), want success", b, reason)
	}
}

// TestConcurrentSaltReuse must pass under -race: of n concurrent
// identical hash responses exactly one is a success.
func TestConcurrentSaltReuse(t *testing.T) {
	const n = 64
	v := &fakeVerifier{match: true}
	r := NewRun(v.verify)
	o := Observation{Kind: Hash, Status: 200, Body: hashBody(phc(9)), FullVerify: true}
	var success, reused atomic.Int32
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			switch b, reason := r.Classify(o); {
			case b == Success:
				success.Add(1)
			case reason == ReasonSaltReused:
				reused.Add(1)
			default:
				t.Errorf("Classify = (%v, %q)", b, reason)
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 || reused.Load() != n-1 {
		t.Fatalf("success=%d reused=%d, want 1 and %d", success.Load(), reused.Load(), n-1)
	}
	if v.calls.Load() != 1 {
		t.Fatalf("Verifier calls = %d, want 1", v.calls.Load())
	}
}

// TestVerifierRunsOutsideLock fails if Verifier calls are serialized:
// each call blocks until all n are in flight at once.
func TestVerifierRunsOutsideLock(t *testing.T) {
	const n = 8
	var arrived sync.WaitGroup
	arrived.Add(n)
	allIn := make(chan struct{})
	go func() { arrived.Wait(); close(allIn) }()
	r := NewRun(func(string, string) (bool, error) {
		arrived.Done()
		select {
		case <-allIn:
			return true, nil
		case <-time.After(5 * time.Second):
			return false, errors.New("Verifier calls did not overlap")
		}
	})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			o := Observation{Kind: Hash, Status: 200, Body: hashBody(phc(byte(i))), FullVerify: true}
			if b, reason := r.Classify(o); b != Success {
				t.Errorf("Classify = (%v, %q), want success", b, reason)
			}
		})
	}
	wg.Wait()
}

func TestClassifyPanics(t *testing.T) {
	tests := map[string]struct {
		r *Run
		o Observation
	}{
		"zero Kind":                        {NewRun(nil), Observation{Status: 200, Body: hashBody(phc(1))}},
		"unknown Kind":                     {NewRun(nil), Observation{Kind: Negative + 1, Status: 400}},
		"FullVerify negative, no Verifier": {NewRun(nil), Observation{Kind: Negative, Status: 400, FullVerify: true}},
		"FullVerify, no Verifier":          {NewRun(nil), Observation{Kind: Hash, Status: 503, FullVerify: true}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			tt.r.Classify(tt.o)
		})
	}
}

func TestZeroBucketIsFailed(t *testing.T) {
	var b Bucket
	if b != Failed {
		t.Fatalf("zero Bucket = %v, want failed", b)
	}
}

func TestStrings(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{Failed.String(), "failed"},
		{Success.String(), "success"},
		{GracefulShed.String(), "graceful_shed"},
		{Bucket(9).String(), "Bucket(9)"},
		{Hash.String(), "hash"},
		{Verify.String(), "verify"},
		{Negative.String(), "negative"},
		{Kind(0).String(), "Kind(0)"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

// TestHashVerifyUnchanged pins the Hash and Verify answers to the
// statuses Negative treats specially, so Negative's rules cannot leak
// into them.
func TestHashVerifyUnchanged(t *testing.T) {
	for _, k := range []Kind{Hash, Verify} {
		for _, tt := range []struct {
			status     int
			h          http.Header
			wantBucket Bucket
			wantReason Reason
		}{
			{400, nil, Failed, ReasonBadStatus},
			{503, nil, Failed, ReasonBare503},
			{503, retryAfter("1", "1"), Failed, ReasonBadRetryAfter},
			{503, retryAfter("soon"), Failed, ReasonBadRetryAfter},
			{503, retryAfter("1"), GracefulShed, ReasonNone},
			{200, nil, Failed, ReasonMalformedBody}, // empty body
		} {
			o := Observation{Kind: k, Status: tt.status, Header: tt.h}
			if b, reason := NewRun(nil).Classify(o); b != tt.wantBucket || reason != tt.wantReason {
				t.Errorf("%v status %d %v: Classify = (%v, %q), want (%v, %q)", k, tt.status, tt.h, b, reason, tt.wantBucket, tt.wantReason)
			}
		}
	}
	r := NewRun(nil)
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(1))}); b != Success {
		t.Errorf("hash 200 = (%v, %q), want success", b, reason)
	}
	if b, reason := r.Classify(Observation{Kind: Verify, WantMatch: false, Status: 200, Body: []byte(`{"match":false}`)}); b != Success {
		t.Errorf("verify 200 = (%v, %q), want success", b, reason)
	}
}

func TestIntegrity(t *testing.T) {
	want := map[Reason]bool{
		ReasonVerifyMismatch:   true,
		ReasonVerifyError:      true,
		ReasonWrongMatch:       true,
		ReasonNonCanonicalPHC:  true,
		ReasonSaltReused:       true,
		ReasonNegativeAccepted: true,
		ReasonMalformedBody:    true,

		ReasonNone:            false,
		ReasonTransportError:  false,
		ReasonOverHangTimeout: false,
		ReasonBadStatus:       false,
		ReasonBare503:         false,
		ReasonBadRetryAfter:   false,
		Reason("unknown"):     false,
	}
	for reason, w := range want {
		if got := Integrity(reason); got != w {
			t.Errorf("Integrity(%q) = %v, want %v", reason, got, w)
		}
	}
}

// TestBodyReadContract checks both sides of Observation.Body's contract
// on one cut-short body: passed with Err set (as a caller whose read
// failed must) it is an ordinary failure; passed as if read cleanly it
// is malformed_body, an integrity failure.
func TestBodyReadContract(t *testing.T) {
	cut := []byte(`{"match":tr`)
	for _, tt := range []struct {
		name          string
		err           error
		wantReason    Reason
		wantIntegrity bool
	}{
		{"read error set", errors.New("unexpected EOF"), ReasonTransportError, false},
		{"clean EOF", nil, ReasonMalformedBody, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := Observation{Kind: Verify, WantMatch: true, Status: 200, Body: cut, Err: tt.err}
			b, reason := NewRun(nil).Classify(o)
			if b != Failed || reason != tt.wantReason {
				t.Fatalf("Classify = (%v, %q), want (failed, %q)", b, reason, tt.wantReason)
			}
			if got := Integrity(reason); got != tt.wantIntegrity {
				t.Fatalf("Integrity(%q) = %v, want %v", reason, got, tt.wantIntegrity)
			}
		})
	}
}

// TestStrictBody: a 2xx body is one JSON object with exactly one member,
// compared as JSON (escapes and surrounding whitespace allowed), never
// as bytes; every other shape is malformed_body.
func TestStrictBody(t *testing.T) {
	p1, p2 := phc(1), phc(2)
	hash := func(body string) Observation { return Observation{Kind: Hash, Status: 200, Body: []byte(body)} }
	verify := func(want bool, body string) Observation {
		return Observation{Kind: Verify, WantMatch: want, Status: 200, Body: []byte(body)}
	}
	// A canonical PHC string whose salt has '/' in it, with every '$'
	// and '/' JSON-escaped.
	slashy := withSalt(b64(bytes.Repeat([]byte{0xFF}, 16)))
	escaped := strings.NewReplacer("$", `\u0024`, "/", `\/`).Replace(slashy)

	tests := []struct {
		name       string
		o          Observation
		wantReason Reason // ReasonNone means Success
	}{
		// Duplicate keys, both orders, for both endpoints: no value of
		// several is ever picked.
		{"hash dup both valid", hash(`{"hash":"` + p1 + `","hash":"` + p2 + `"}`), ReasonMalformedBody},
		{"hash dup same value", hash(`{"hash":"` + p1 + `","hash":"` + p1 + `"}`), ReasonMalformedBody},
		{"hash dup null first", hash(`{"hash":null,"hash":"` + p1 + `"}`), ReasonMalformedBody},
		{"hash dup number first", hash(`{"hash":1,"hash":"` + p1 + `"}`), ReasonMalformedBody},
		{"hash dup null second", hash(`{"hash":"` + p1 + `","hash":null}`), ReasonMalformedBody},
		{"hash dup number second", hash(`{"hash":"` + p1 + `","hash":1}`), ReasonMalformedBody},
		{"hash other member first", hash(`{"x":1,"hash":"` + p1 + `"}`), ReasonMalformedBody},
		{"match dup same value", verify(true, `{"match":true,"match":true}`), ReasonMalformedBody},
		{"match dup false then true", verify(true, `{"match":false,"match":true}`), ReasonMalformedBody},
		{"match dup true then false", verify(false, `{"match":true,"match":false}`), ReasonMalformedBody},
		{"match dup null first", verify(true, `{"match":null,"match":true}`), ReasonMalformedBody},
		{"match dup number first", verify(true, `{"match":0,"match":true}`), ReasonMalformedBody},
		{"match dup null second", verify(true, `{"match":true,"match":null}`), ReasonMalformedBody},
		{"match dup number second", verify(true, `{"match":true,"match":0}`), ReasonMalformedBody},
		{"match other member first", verify(true, `{"x":1,"match":true}`), ReasonMalformedBody},

		// JSON-equivalent bodies are accepted.
		{"escaped hash key", hash(`{"\u0068\u0061\u0073\u0068":"` + p1 + `"}`), ReasonNone},
		{"escaped match key", verify(true, `{"m\u0061tch":true}`), ReasonNone},
		{"escaped PHC characters", hash(`{"hash":"` + escaped + `"}`), ReasonNone},
		{"hash surrounding whitespace", hash(" \t\r\n{ \"hash\" :\t\"" + p1 + "\" }\r\n\t "), ReasonNone},
		{"match surrounding whitespace", verify(true, " \t\r\n{ \"match\" :\ttrue }\r\n\t "), ReasonNone},
		{"hash trailing newline", hash(`{"hash":"` + p1 + "\"}\n"), ReasonNone},
		{"match trailing newline", verify(false, "{\"match\":false}\n"), ReasonNone},

		// Invalid UTF-8 anywhere, even where a lenient decoder would
		// discard it, and a BOM.
		{"invalid UTF-8 in hash value", hash(`{"hash":"` + p1 + "\xff\"}"), ReasonMalformedBody},
		{"invalid UTF-8 in hash key", hash("{\"hash\xff\":\"" + p1 + `"}`), ReasonMalformedBody},
		{"invalid UTF-8 in match key", verify(true, "{\"match\xff\":true}"), ReasonMalformedBody},
		{"invalid UTF-8 in discarded earlier value", hash("{\"hash\":\"\xff\",\"hash\":\"" + p1 + `"}`), ReasonMalformedBody},
		{"invalid UTF-8 in other member", hash("{\"x\":\"\xff\",\"hash\":\"" + p1 + `"}`), ReasonMalformedBody},
		{"hash BOM", hash("\xef\xbb\xbf" + `{"hash":"` + p1 + `"}`), ReasonMalformedBody},
		{"match BOM", verify(true, "\xef\xbb\xbf{\"match\":true}"), ReasonMalformedBody},
		// Valid UTF-8 JSON, decoded to U+FFFD: a PHC check failure.
		{"lone surrogate in hash", hash(`{"hash":"` + p1 + `\ud800"}`), ReasonNonCanonicalPHC},

		// match must be a JSON boolean; WantMatch is what a coercing
		// decoder would read, so leniency would show as Success.
		{"match string", verify(true, `{"match":"true"}`), ReasonMalformedBody},
		{"match 1", verify(true, `{"match":1}`), ReasonMalformedBody},
		{"match 0", verify(false, `{"match":0}`), ReasonMalformedBody},
		{"match null", verify(false, `{"match":null}`), ReasonMalformedBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantBucket := Failed
			if tt.wantReason == ReasonNone {
				wantBucket = Success
			}
			if b, reason := NewRun(nil).Classify(tt.o); b != wantBucket || reason != tt.wantReason {
				t.Errorf("Classify(%q) = (%v, %q), want (%v, %q)", tt.o.Body, b, reason, wantBucket, tt.wantReason)
			}
		})
	}

	// The escaped hash records the unescaped salt.
	r := NewRun(nil)
	if b, reason := r.Classify(hash(`{"hash":"` + escaped + `"}`)); b != Success {
		t.Fatalf("escaped = (%v, %q), want success", b, reason)
	}
	if _, reason := r.Classify(hash(`{"hash":"` + slashy + `"}`)); reason != ReasonSaltReused {
		t.Fatalf("unescaped replay: reason = %q, want salt_reused", reason)
	}
}

// TestGeneration0Bytes pins the exact bytes generation 0 writes
// (internal/wrapper/server.go writeJSON: json.Encoder, so a compact
// object and one "\n") as Success.
func TestGeneration0Bytes(t *testing.T) {
	encode := func(v any) []byte {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	p := phc(1)
	for _, tt := range []struct {
		o    Observation
		v    any
		want string
	}{
		{Observation{Kind: Hash}, struct {
			Hash string `json:"hash"`
		}{p}, `{"hash":"` + p + "\"}\n"},
		{Observation{Kind: Verify, WantMatch: true}, struct {
			Match bool `json:"match"`
		}{true}, "{\"match\":true}\n"},
		{Observation{Kind: Verify, WantMatch: false}, struct {
			Match bool `json:"match"`
		}{false}, "{\"match\":false}\n"},
	} {
		if got := encode(tt.v); string(got) != tt.want {
			t.Fatalf("encoder wrote %q, want %q", got, tt.want)
		}
		o := tt.o
		o.Status = 200
		o.Body = []byte(tt.want)
		if b, reason := NewRun(nil).Classify(o); b != Success {
			t.Errorf("%v %q = (%v, %q), want success", o.Kind, o.Body, b, reason)
		}
	}
}

// TestNon200Success: a 2xx other than 200 passes through every
// integrity check, its hash salt recorded, and then latency, before it
// is bad_status.
func TestNon200Success(t *testing.T) {
	hash := func(status int, saltByte byte) Observation {
		return Observation{Kind: Hash, Status: status, Body: hashBody(phc(saltByte))}
	}
	verify := Observation{Kind: Verify, WantMatch: true, Body: []byte(`{"match":true}`)}
	for _, status := range []int{201, 202, 203, 204, 206, 299} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r := NewRun(nil)
			if b, reason := r.Classify(hash(status, 1)); b != Failed || reason != ReasonBadStatus {
				t.Fatalf("hash = (%v, %q), want (failed, bad_status)", b, reason)
			}
			if b, reason := r.Classify(hash(200, 1)); b != Failed || reason != ReasonSaltReused {
				t.Fatalf("200 after it, same salt = (%v, %q), want (failed, salt_reused)", b, reason)
			}
			v := verify
			v.Status = status
			if b, reason := r.Classify(v); b != Failed || reason != ReasonBadStatus {
				t.Fatalf("verify = (%v, %q), want (failed, bad_status)", b, reason)
			}
		})
	}

	late := hash(201, 3)
	late.Latency = HangTimeout + 1
	lateVerify := verify
	lateVerify.Status, lateVerify.Latency = 201, HangTimeout+1
	for _, o := range []Observation{late, lateVerify} {
		if b, reason := NewRun(nil).Classify(o); b != Failed || reason != ReasonOverHangTimeout {
			t.Errorf("late 201 %v = (%v, %q), want (failed, over_hang_timeout)", o.Kind, b, reason)
		}
	}

	r := NewRun(nil)
	if b, reason := r.Classify(hash(200, 4)); b != Success {
		t.Fatalf("200 = (%v, %q), want success", b, reason)
	}
	if b, reason := r.Classify(hash(201, 4)); b != Failed || reason != ReasonSaltReused {
		t.Fatalf("201 with reused salt = (%v, %q), want (failed, salt_reused)", b, reason)
	}

	badHash := hash(201, 5)
	badHash.Body = []byte(`{"hash":"` + phc(5) + `","x":1}`)
	badVerify := verify
	badVerify.Status, badVerify.Body = 201, []byte(`{"match":1}`)
	for _, o := range []Observation{badHash, badVerify} {
		if b, reason := NewRun(nil).Classify(o); b != Failed || reason != ReasonMalformedBody {
			t.Errorf("201 bad body %v = (%v, %q), want (failed, malformed_body)", o.Kind, b, reason)
		}
	}
}

// TestMaxBodyBytes: a 2xx hash or verify body may be exactly
// MaxBodyBytes long, not one byte more; other bodies are not
// size-checked.
func TestMaxBodyBytes(t *testing.T) {
	pad := func(body []byte, n int) []byte {
		return append(bytes.Clone(body), bytes.Repeat([]byte(" "), n-len(body))...)
	}
	for _, tt := range []struct {
		o          Observation
		n          int
		wantBucket Bucket
		wantReason Reason
	}{
		{Observation{Kind: Hash, Status: 200, Body: hashBody(phc(1))}, MaxBodyBytes, Success, ReasonNone},
		{Observation{Kind: Hash, Status: 200, Body: hashBody(phc(1))}, MaxBodyBytes + 1, Failed, ReasonMalformedBody},
		{Observation{Kind: Verify, WantMatch: true, Status: 200, Body: []byte(`{"match":true}`)}, MaxBodyBytes, Success, ReasonNone},
		{Observation{Kind: Verify, WantMatch: true, Status: 200, Body: []byte(`{"match":true}`)}, MaxBodyBytes + 1, Failed, ReasonMalformedBody},
		{Observation{Kind: Hash, Status: 503, Header: retryAfter("1"), Body: []byte("{}")}, MaxBodyBytes + 1, GracefulShed, ReasonNone},
		{Observation{Kind: Verify, Status: 503, Header: retryAfter("1"), Body: []byte("{}")}, 4 * MaxBodyBytes, GracefulShed, ReasonNone},
		{Observation{Kind: Hash, Status: 500, Body: []byte("{}")}, MaxBodyBytes + 1, Failed, ReasonBadStatus},
		{Observation{Kind: Negative, Status: 400, Body: []byte("{}")}, MaxBodyBytes + 1, Success, ReasonNone},
		{Observation{Kind: Negative, Status: 200, Body: []byte("{}")}, MaxBodyBytes + 1, Failed, ReasonNegativeAccepted},
	} {
		o := tt.o
		o.Body = pad(o.Body, tt.n)
		if b, reason := NewRun(nil).Classify(o); b != tt.wantBucket || reason != tt.wantReason {
			t.Errorf("%v %d, %d-byte body: Classify = (%v, %q), want (%v, %q)", o.Kind, o.Status, len(o.Body), b, reason, tt.wantBucket, tt.wantReason)
		}
	}

	// The cap comes first among a 2xx's checks: an over-cap body is
	// malformed_body late or non-200, is never decoded (its salt is not
	// recorded) and never reaches the Verifier.
	over := Observation{Kind: Hash, Status: 200, Body: pad(hashBody(phc(2)), MaxBodyBytes+1), FullVerify: true}
	late := over
	late.Latency = HangTimeout + 1
	created := over
	created.Status = 201
	v := &fakeVerifier{match: true}
	r := NewRun(v.verify)
	for _, o := range []Observation{over, late, created} {
		if b, reason := r.Classify(o); b != Failed || reason != ReasonMalformedBody {
			t.Errorf("over cap, status %d, latency %v: Classify = (%v, %q), want (failed, malformed_body)", o.Status, o.Latency, b, reason)
		}
	}
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(2))}); b != Success {
		t.Errorf("salt of an over-cap body = (%v, %q), want success", b, reason)
	}
	if v.calls.Load() != 0 {
		t.Errorf("Verifier calls = %d, want 0", v.calls.Load())
	}
}
