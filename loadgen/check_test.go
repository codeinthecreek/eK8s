package loadgen

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

const (
	onTime = 10 * time.Millisecond
	late   = classifier.HangTimeout + time.Millisecond
)

// hashJSON returns a /hash response body carrying hash.
func hashJSON(hash string) []byte { return mustJSON(map[string]string{"hash": hash}) }

// withSalt returns a canonical PHC string at the fixed parameters with
// salt and a fresh random key: no Argon2 runs, as in randomExactPHC.
func withSalt(salt string) string { return exactPHC(salt, b64(randomBytes(keyLen))) }

func newSalt() string { return b64(randomBytes(saltLen)) }

func ok(body []byte, latency time.Duration) Response {
	return Response{Status: http.StatusOK, Body: body, Latency: latency}
}

func verdict(r classifier.Reason) Verdict {
	if r == classifier.ReasonNone {
		return Verdict{classifier.Success, r}
	}
	return Verdict{classifier.Failed, r}
}

// check runs c.Check and fails the test on an error, an unexpected
// verdict or an unexpected pair (nil for none).
func check(t *testing.T, c *Checker, req Request, resp Response, want Verdict, wantPair *HashPair) {
	t.Helper()
	got, pair, err := c.Check(req, resp)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != want {
		t.Errorf("Check = %v %q, want %v %q", got.Bucket, got.Reason, want.Bucket, want.Reason)
	}
	if !samePair(pair, wantPair) {
		t.Errorf("Check pair = %+v, want %+v", pair, wantPair)
	}
}

func samePair(a, b *HashPair) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestClassifierKind(t *testing.T) {
	for _, tc := range []struct {
		in      Kind
		want    classifier.Kind
		wantErr bool
	}{
		{KindHash, classifier.Hash, false},
		{KindVerify, classifier.Verify, false},
		{KindNegative, classifier.Negative, false},
		{"", 0, true},
		{"Hash", 0, true},
	} {
		got, err := classifierKind(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("classifierKind(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestCheckUnknownKind checks that a request of an unknown kind errors
// and never reaches the classifier: it does not panic (Classify panics
// on an invalid kind), and the salt of its valid 2xx hash body is not
// recorded, so the same body then passes as a /hash response, with its
// pair.
func TestCheckUnknownKind(t *testing.T) {
	c := NewChecker()
	hash := randomExactPHC()
	body := hashJSON(hash)
	req := hashRequest(newPassword())
	bad := req
	bad.Kind = "bogus"
	if v, p, err := c.Check(bad, ok(body, onTime)); err == nil || v != (Verdict{}) || p != nil {
		t.Fatalf("Check(kind %q) = %v, %+v, %v; want zero Verdict, nil pair and an error", bad.Kind, v, p, err)
	}
	check(t, c, req, ok(body, onTime), verdict(classifier.ReasonNone), &HashPair{req.Password, hash})
}

// burstNegatives returns one negative of each class, taken from a real
// Burst.
func burstNegatives(t *testing.T) map[NegativeClass]Request {
	t.Helper()
	_, verifies, _, err := BurstCounts(100)
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := Burst(100, testPairs(verifies))
	if err != nil {
		t.Fatal(err)
	}
	negs := map[NegativeClass]Request{}
	for _, r := range reqs {
		if r.Kind == KindNegative {
			negs[r.Class] = r
		}
	}
	if len(negs) != len(negativeClasses) {
		t.Fatalf("burst has negatives of %d classes, want %d", len(negs), len(negativeClasses))
	}
	return negs
}

func TestCheckNegatives(t *testing.T) {
	for class, req := range burstNegatives(t) {
		t.Run(string(class), func(t *testing.T) {
			// The 2xx body is a valid hash response, so a negative that
			// was checked as /hash would pass and record it.
			for _, tc := range []struct {
				name string
				resp Response
				want Verdict
			}{
				{"400", Response{Status: http.StatusBadRequest, Latency: onTime}, verdict(classifier.ReasonNone)},
				{"2xx", ok(hashJSON(randomExactPHC()), onTime), verdict(classifier.ReasonNegativeAccepted)},
				{"late 2xx", ok(hashJSON(randomExactPHC()), late), verdict(classifier.ReasonNegativeAccepted)},
				{"late 400", Response{Status: http.StatusBadRequest, Latency: late}, verdict(classifier.ReasonOverHangTimeout)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					check(t, NewChecker(), req, tc.resp, tc.want, nil)
				})
			}
		})
	}
}

func TestCheckVerify(t *testing.T) {
	for _, match := range []bool{true, false} {
		req := verifyRequest(Pair{Password: newPassword(), Hash: randomExactPHC(), Match: match})
		for _, tc := range []struct {
			name   string
			status int
			body   []byte
			want   Verdict
		}{
			{"right match", http.StatusOK, mustJSON(map[string]bool{"match": match}), verdict(classifier.ReasonNone)},
			{"wrong match", http.StatusOK, mustJSON(map[string]bool{"match": !match}), verdict(classifier.ReasonWrongMatch)},
			{"hash body", http.StatusOK, hashJSON(randomExactPHC()), verdict(classifier.ReasonMalformedBody)},
			{"201 right match", http.StatusCreated, mustJSON(map[string]bool{"match": match}), verdict(classifier.ReasonBadStatus)},
		} {
			t.Run(fmt.Sprintf("match=%v/%s", match, tc.name), func(t *testing.T) {
				check(t, NewChecker(), req, Response{Status: tc.status, Body: tc.body, Latency: onTime}, tc.want, nil)
			})
		}
	}
}

// TestCheckHashRecords checks that a valid hash returns its pair at any
// 2xx status, on time or late (the host verifies those hashes too). A
// 2xx other than 200 is bad_status, or over_hang_timeout when late, but
// only after the integrity checks pass, so its salt is recorded too.
func TestCheckHashRecords(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent, 299} {
		for _, latency := range []time.Duration{onTime, late} {
			t.Run(fmt.Sprintf("%d/%v", status, latency), func(t *testing.T) {
				want := verdict(classifier.ReasonNone)
				switch {
				case latency > classifier.HangTimeout:
					want = verdict(classifier.ReasonOverHangTimeout)
				case status != http.StatusOK:
					want = verdict(classifier.ReasonBadStatus)
				}
				c := NewChecker()
				req := hashRequest(newPassword())
				hash := randomExactPHC()
				check(t, c, req, Response{Status: status, Body: hashJSON(hash), Latency: latency}, want, &HashPair{req.Password, hash})
				check(t, c, hashRequest(newPassword()), ok(hashJSON(hash), onTime), verdict(classifier.ReasonSaltReused), nil)
			})
		}
	}
}

// TestCheckHashSaltReused checks the salt set at any latency: the second
// response with a salt is salt_reused and has no pair, whichever of the
// two is late.
func TestCheckHashSaltReused(t *testing.T) {
	for _, tc := range []struct{ first, second time.Duration }{
		{onTime, onTime}, {onTime, late}, {late, onTime}, {late, late},
	} {
		t.Run(fmt.Sprintf("%v_then_%v", tc.first, tc.second), func(t *testing.T) {
			c := NewChecker()
			salt := newSalt()
			req1, req2 := hashRequest(newPassword()), hashRequest(newPassword())
			hash1 := withSalt(salt)
			want1 := verdict(classifier.ReasonNone)
			if tc.first == late {
				want1 = verdict(classifier.ReasonOverHangTimeout)
			}
			check(t, c, req1, ok(hashJSON(hash1), tc.first), want1, &HashPair{req1.Password, hash1})
			check(t, c, req2, ok(hashJSON(withSalt(salt)), tc.second), verdict(classifier.ReasonSaltReused), nil)
		})
	}
}

// TestCheckHashSaltPerChecker checks that salt sets are per variant: the
// same salt from two Checkers passes in both.
func TestCheckHashSaltPerChecker(t *testing.T) {
	salt := newSalt()
	for range 2 {
		c := NewChecker()
		req, hash := hashRequest(newPassword()), withSalt(salt)
		check(t, c, req, ok(hashJSON(hash), onTime), verdict(classifier.ReasonNone), &HashPair{req.Password, hash})
	}
}

// TestCheckHashNoRecord covers the /hash responses that have no pair.
// Where a body carries a valid hash, returning it would be wrong, so a
// missing status or reason clause in the record condition shows up as a
// returned pair.
func TestCheckHashNoRecord(t *testing.T) {
	retry := http.Header{"Retry-After": {"1"}}
	for _, tc := range []struct {
		name string
		resp Response
		want Verdict
	}{
		{"malformed: not JSON", ok([]byte("not json"), onTime), verdict(classifier.ReasonMalformedBody)},
		{"malformed: hash not a string", ok([]byte(`{"hash":5}`), onTime), verdict(classifier.ReasonMalformedBody)},
		{"malformed: extra key", ok(mustJSON(map[string]string{"hash": randomExactPHC(), "x": "y"}), onTime), verdict(classifier.ReasonMalformedBody)},
		{"late malformed", ok([]byte("{}"), late), verdict(classifier.ReasonMalformedBody)},
		{"malformed: over MaxBodyBytes", ok(padTo(hashJSON(randomExactPHC()), classifier.MaxBodyBytes+1), onTime), verdict(classifier.ReasonMalformedBody)},
		{"non-canonical: unused bits", ok(hashJSON(nonCanonicalPHC(setUnusedBits)), onTime), verdict(classifier.ReasonNonCanonicalPHC)},
		{"non-canonical: padded", ok(hashJSON(nonCanonicalPHC(paddedB64)), onTime), verdict(classifier.ReasonNonCanonicalPHC)},
		{"non-canonical: t=1", ok(hashJSON(phc(exactVariant, exactVersion, "m=19456,t=1,p=1", newSalt(), b64(randomBytes(keyLen)))), onTime), verdict(classifier.ReasonNonCanonicalPHC)},
		{"late non-canonical", ok(hashJSON(nonCanonicalPHC(insertNewline)), late), verdict(classifier.ReasonNonCanonicalPHC)},
		{"transport error", Response{Status: http.StatusOK, Body: hashJSON(randomExactPHC()), Latency: onTime, Err: errors.New("connection reset")}, verdict(classifier.ReasonTransportError)},
		{"503 shed", Response{Status: http.StatusServiceUnavailable, Header: retry, Body: hashJSON(randomExactPHC()), Latency: onTime}, Verdict{classifier.GracefulShed, classifier.ReasonNone}},
		{"late 503", Response{Status: http.StatusServiceUnavailable, Header: retry, Body: hashJSON(randomExactPHC()), Latency: late}, verdict(classifier.ReasonOverHangTimeout)},
		{"late 1xx", Response{Status: 199, Body: hashJSON(randomExactPHC()), Latency: late}, verdict(classifier.ReasonOverHangTimeout)},
		{"late 300", Response{Status: http.StatusMultipleChoices, Body: hashJSON(randomExactPHC()), Latency: late}, verdict(classifier.ReasonOverHangTimeout)},
		{"500", Response{Status: http.StatusInternalServerError, Body: hashJSON(randomExactPHC()), Latency: onTime}, verdict(classifier.ReasonBadStatus)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check(t, NewChecker(), hashRequest(newPassword()), tc.resp, tc.want, nil)
		})
	}
}

func paddedB64(b []byte) string { return b64(b) + "==" }

// padTo pads body with trailing JSON whitespace to n bytes.
func padTo(body []byte, n int) []byte {
	return append(body, strings.Repeat(" ", n-len(body))...)
}

// TestCheckHashBodyCap checks that a valid hash body padded to exactly
// classifier.MaxBodyBytes returns its pair (one byte more is
// TestCheckHashNoRecord's "over MaxBodyBytes" case).
func TestCheckHashBodyCap(t *testing.T) {
	c := NewChecker()
	req, hash := hashRequest(newPassword()), randomExactPHC()
	check(t, c, req, ok(padTo(hashJSON(hash), classifier.MaxBodyBytes), onTime), verdict(classifier.ReasonNone), &HashPair{req.Password, hash})
}

// TestCheckHashTransportErrorNoSalt checks that a transport error leaves
// the salt set alone: the same body then passes.
func TestCheckHashTransportErrorNoSalt(t *testing.T) {
	c := NewChecker()
	req, hash := hashRequest(newPassword()), randomExactPHC()
	resp := ok(hashJSON(hash), onTime)
	failed := resp
	failed.Err = errors.New("read: connection reset")
	check(t, c, req, failed, verdict(classifier.ReasonTransportError), nil)
	check(t, c, req, resp, verdict(classifier.ReasonNone), &HashPair{req.Password, hash})
}

// TestCheckHashDuplicateKey checks that a body with a duplicate "hash"
// key is malformed_body, whatever the first value holds, and has no
// pair and records no salt, so the last hash then passes alone.
func TestCheckHashDuplicateKey(t *testing.T) {
	for _, first := range []string{`"` + randomExactPHC() + `"`, `5`} {
		c := NewChecker()
		req, last := hashRequest(newPassword()), randomExactPHC()
		body := []byte(`{"hash":` + first + `,"hash":"` + last + `"}`)
		check(t, c, req, ok(body, onTime), verdict(classifier.ReasonMalformedBody), nil)
		req2 := hashRequest(newPassword())
		check(t, c, req2, ok(hashJSON(last), onTime), verdict(classifier.ReasonNone), &HashPair{req2.Password, last})
	}
}

func TestHashField(t *testing.T) {
	hash := randomExactPHC()
	if got, err := hashField(hashJSON(hash)); err != nil || got != hash {
		t.Errorf("hashField(valid) = %q, %v; want %q, nil", got, err, hash)
	}
	for _, body := range []string{"", "not json", "[]", `{}`, `{"match":true}`, `{"hash":5}`} {
		if got, err := hashField([]byte(body)); err == nil {
			t.Errorf("hashField(%q) = %q, nil; want an error", body, got)
		}
	}
}

// TestCheckConcurrent runs many goroutines on one Checker: each of
// nSalts salts is answered nRepeats times, by different goroutines, with
// /verify and negative responses interleaved. Exactly one response per
// salt returns a pair and the rest are salt_reused. Run it under -race.
func TestCheckConcurrent(t *testing.T) {
	const (
		nSalts     = 200
		nRepeats   = 4
		goroutines = 16
	)
	type job struct {
		req  Request
		resp Response
	}
	var jobs []job
	for range nSalts {
		salt := newSalt()
		for range nRepeats {
			jobs = append(jobs, job{hashRequest(newPassword()), ok(hashJSON(withSalt(salt)), onTime)})
		}
		jobs = append(jobs,
			job{verifyRequest(Pair{newPassword(), randomExactPHC(), true}), ok([]byte(`{"match":true}`), onTime)},
			job{newNegative(InvalidJSON), ok(hashJSON(withSalt(salt)), onTime)})
	}
	c := NewChecker()
	ch := make(chan job)
	counts := make([]map[classifier.Reason]int, goroutines)
	gPairs := make([][]HashPair, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		counts[g] = map[classifier.Reason]int{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				v, p, err := c.Check(j.req, j.resp)
				if err != nil {
					t.Error(err)
					continue
				}
				if j.req.Kind == KindHash {
					counts[g][v.Reason]++
				}
				if (p != nil) != (j.req.Kind == KindHash && v.Reason == classifier.ReasonNone) {
					t.Errorf("%s %q: pair %+v", j.req.Kind, v.Reason, p)
				}
				if p != nil {
					if p.Password != j.req.Password {
						t.Errorf("pair password %q, want the request's %q", p.Password, j.req.Password)
					}
					gPairs[g] = append(gPairs[g], *p)
				}
			}
		}()
	}
	for _, i := range mrand.Perm(len(jobs)) {
		ch <- jobs[i]
	}
	close(ch)
	wg.Wait()

	total := map[classifier.Reason]int{}
	for _, m := range counts {
		for r, n := range m {
			total[r] += n
		}
	}
	var pairs []HashPair
	for _, ps := range gPairs {
		pairs = append(pairs, ps...)
	}
	if total[classifier.ReasonNone] != nSalts || total[classifier.ReasonSaltReused] != nSalts*(nRepeats-1) || len(total) != 2 {
		t.Errorf("/hash reasons = %v, want %d success and %d salt_reused", total, nSalts, nSalts*(nRepeats-1))
	}
	if len(pairs) != total[classifier.ReasonNone] {
		t.Errorf("%d pairs returned, want one per success, %d", len(pairs), total[classifier.ReasonNone])
	}
	salts := map[string]bool{}
	for _, p := range pairs {
		salts[strings.Split(p.Hash, "$")[4]] = true
	}
	if len(salts) != nSalts {
		t.Errorf("pairs cover %d distinct salts, want %d", len(salts), nSalts)
	}
}
