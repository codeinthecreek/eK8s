package classifier

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
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
		ReasonVerifyMismatch, ReasonVerifyError} {
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
	for _, o := range []Observation{
		{Kind: Hash, Status: 503, Header: retryAfter("1"), Body: hashBody(phc(6)), Latency: HangTimeout + 1},
		{Kind: Verify, Status: 200, Body: hashBody(phc(6)), Latency: HangTimeout + 1},
		{Kind: Hash, Status: 200, Body: []byte(`{"hash":"` + phc(6) + `","x":1}`), Latency: HangTimeout + 1},
	} {
		if _, reason := r.Classify(o); reason != ReasonOverHangTimeout {
			t.Fatalf("late %v status %d: reason = %q, want over_hang_timeout", o.Kind, o.Status, reason)
		}
	}
	if b, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(phc(6))}); b != Success {
		t.Fatalf("on time after non-recording late responses = (%v, %q), want success", b, reason)
	}
}

func TestSaltNotRecordedWhenNonCanonical(t *testing.T) {
	r := NewRun(nil)
	bad := withKey(b64(make([]byte, 31))) // canonical salt16, bad key
	if _, reason := r.Classify(Observation{Kind: Hash, Status: 200, Body: hashBody(bad)}); reason != ReasonNonCanonicalPHC {
		t.Fatalf("reason = %q, want non_canonical_phc", reason)
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
		"zero Kind":               {NewRun(nil), Observation{Status: 200, Body: hashBody(phc(1))}},
		"unknown Kind":            {NewRun(nil), Observation{Kind: 3, Status: 200}},
		"FullVerify, no Verifier": {NewRun(nil), Observation{Kind: Hash, Status: 503, FullVerify: true}},
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
		{Kind(0).String(), "Kind(0)"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
