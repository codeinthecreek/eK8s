package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// unparseableURL is a base URL no request can be built from: its IPv6
// host has no closing ']'.
const unparseableURL = "http://[::1"

// The window most tests run: 50 sends, one every 5 ms.
const (
	testRate   = 200
	testWindow = 250 * time.Millisecond
	testSends  = 50
	testStep   = 5 * time.Millisecond
)

// stub is a stand-in variant. It decodes each request body strictly and
// answers 400 for invalid JSON, an empty or missing password, or a
// password over 1024 bytes. A valid /hash gets answer's response (okHash
// unless the test overrides it). A valid /verify carrying one of its pairs
// gets 200 {"match": the pair's Match}, and any other /verify a 400, so
// the /verify negatives, which carry fresh passwords, get 400.
type stub struct {
	url    string
	pairs  map[string]Pair // by password
	answer func(st *stub, w http.ResponseWriter, password string)
	seen   atomic.Int64 // requests received

	mu       sync.Mutex
	hashes   map[string]string // hash okHash answered -> its request's password
	verifies int               // /verify requests answered from pairs
}

// newStub starts a stub holding pairs. answer, if not nil, answers each
// valid /hash request in place of okHash.
func newStub(t *testing.T, pairs []Pair, answer func(st *stub, w http.ResponseWriter, password string)) *stub {
	t.Helper()
	if answer == nil {
		answer = (*stub).okHash
	}
	st := &stub{pairs: map[string]Pair{}, answer: answer, hashes: map[string]string{}}
	for _, p := range pairs {
		st.pairs[p.Password] = p
	}
	st.url = serve(t, st.serveHTTP).URL
	return st
}

func (st *stub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	st.seen.Add(1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	password, hash, ok := decodeStrict(body)
	switch {
	case !ok || r.Method != http.MethodPost:
		http.Error(w, "bad request", http.StatusBadRequest)
	case r.URL.Path == HashPath:
		st.answer(st, w, password)
	case r.URL.Path == VerifyPath:
		p, known := st.pairs[password]
		if !known || hash == nil || *hash != p.Hash {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		st.verifies++
		st.mu.Unlock()
		w.Write(mustJSON(map[string]bool{"match": p.Match}))
	default:
		http.NotFound(w, r)
	}
}

// okHash answers 200 with a canonical fake PHC string carrying a fresh
// random salt, and remembers which password it answered.
func (st *stub) okHash(w http.ResponseWriter, password string) {
	hash := withSalt(newSalt())
	st.mu.Lock()
	st.hashes[hash] = password
	st.mu.Unlock()
	w.Write(hashJSON(hash))
}

// decodeStrict decodes one request object with no unknown fields and
// nothing after it, and returns its password, nonempty and at most 1024
// bytes, and its hash, nil if absent.
func decodeStrict(body []byte) (password string, hash *string, ok bool) {
	var req struct {
		Password *string `json:"password"`
		Hash     *string `json:"hash"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return "", nil, false
	}
	if req.Password == nil || *req.Password == "" || len(*req.Password) > maxPasswordLen {
		return "", nil, false
	}
	return *req.Password, req.Hash, true
}

func newTarget(t *testing.T, id, url string, capacity int) Target {
	t.Helper()
	ring, err := NewRing(capacity)
	if err != nil {
		t.Fatal(err)
	}
	return Target{ID: id, URL: url, Checker: NewChecker(), Ring: ring}
}

// readFrom returns r's records from since on, failing the test on a gap.
func readFrom(t *testing.T, r *Ring, since uint64) []Record {
	t.Helper()
	recs, gap := r.Read(since)
	if gap {
		t.Fatalf("Read(%d): gap", since)
	}
	return recs
}

// checkSchedule checks one call's records, read in Seq order from seq0:
// their Seqs run on from seq0, each Index from 0 to len(recs)-1 appears
// once, Target and RunID are the call's, and the intended times are
// exactly start + Index x step, IntendedWall in UTC with no monotonic
// reading.
func checkSchedule(t *testing.T, s *Sender, recs []Record, tg Target, runID string, start time.Time, step time.Duration, seq0 uint64) {
	t.Helper()
	seen := make([]bool, len(recs))
	for j, rec := range recs {
		if rec.Seq != seq0+uint64(j) {
			t.Errorf("record %d: Seq %d, want %d", j, rec.Seq, seq0+uint64(j))
		}
		if rec.Target != tg.ID || rec.RunID != runID {
			t.Errorf("record %d: Target %q RunID %q, want %q %q", j, rec.Target, rec.RunID, tg.ID, runID)
		}
		if rec.Index < 0 || rec.Index >= len(recs) || seen[rec.Index] {
			t.Errorf("record %d: Index %d out of range or repeated", j, rec.Index)
			continue
		}
		seen[rec.Index] = true
		d := time.Duration(rec.Index) * step
		if want := start.Sub(s.base) + d; rec.IntendedMono != want {
			t.Errorf("Index %d: IntendedMono %v, want %v", rec.Index, rec.IntendedMono, want)
		}
		want := time.Unix(0, start.UnixNano()).UTC().Add(d)
		if got := rec.IntendedWall; !got.Equal(want) || got.Location() != time.UTC || got != got.Round(0) {
			t.Errorf("Index %d: IntendedWall %v, want %v in UTC with no monotonic reading", rec.Index, got, want)
		}
	}
}

// runWindow runs one test window to tg and returns its records, failing
// the test on an error or a record count other than testSends.
func runWindow(t *testing.T, s *Sender, tg Target, runID string, pairs []Pair) (start time.Time, recs []Record) {
	t.Helper()
	since, _ := tg.Ring.Read(0)
	start = time.Now()
	if err := s.RunWindow(context.Background(), tg, runID, start, testRate, testWindow, pairs); err != nil {
		t.Fatalf("RunWindow: %v", err)
	}
	recs = readFrom(t, tg.Ring, uint64(len(since)))
	if len(recs) != testSends {
		t.Fatalf("RunWindow wrote %d records, want %d", len(recs), testSends)
	}
	checkSchedule(t, s, recs, tg, runID, start, testStep, uint64(len(since)))
	return start, recs
}

// checkSuccess fails the test for a record that is skipped, a fault, or
// not a success.
func checkSuccess(t *testing.T, rec Record) {
	t.Helper()
	if rec.Skipped || rec.Fault != "" || rec.Verdict != verdict(classifier.ReasonNone) {
		t.Errorf("Index %d (%s %s): Skipped %v, Fault %q, Verdict %v %q; want a success", rec.Index, rec.Kind, rec.Class, rec.Skipped, rec.Fault, rec.Verdict.Bucket, rec.Verdict.Reason)
	}
}

func TestWindowSendsTestWindow(t *testing.T) {
	if n, err := WindowSends(testRate, testWindow); n != testSends || err != nil {
		t.Fatalf("WindowSends(%v, %v) = %d, %v; want %d", testRate, testWindow, n, err, testSends)
	}
}

// TestRunWindow checks one window: every send recorded once at its
// intended times, all of them successes, and a pair on exactly the
// /hash records, each with its request's password and the hash the stub
// answered it with.
func TestRunWindow(t *testing.T) {
	pairs := testPairs(testSends)
	st := newStub(t, pairs, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, testSends)
	_, recs := runWindow(t, s, tg, "run1", pairs)
	st.mu.Lock()
	defer st.mu.Unlock()
	hashes := 0
	for _, rec := range recs {
		checkSuccess(t, rec)
		if rec.ActualMono < rec.IntendedMono || rec.Latency <= 0 {
			t.Errorf("Index %d: ActualMono %v before IntendedMono %v, or Latency %v not positive", rec.Index, rec.ActualMono, rec.IntendedMono, rec.Latency)
		}
		if rec.Kind == "" || (rec.Kind == KindNegative) != (rec.Class != "") || rec.Evidence != nil {
			t.Errorf("Index %d: Kind %q Class %q Evidence %v", rec.Index, rec.Kind, rec.Class, rec.Evidence)
		}
		if rec.Kind != KindHash {
			if rec.Pair != nil {
				t.Errorf("Index %d: %s record has pair %+v", rec.Index, rec.Kind, rec.Pair)
			}
			continue
		}
		hashes++
		if rec.Pair == nil {
			t.Errorf("Index %d: /hash record has no pair", rec.Index)
		} else if pw, ok := st.hashes[rec.Pair.Hash]; !ok || pw != rec.Pair.Password {
			t.Errorf("Index %d: pair %+v, but the stub answered that hash to password %q (found %v)", rec.Index, rec.Pair, pw, ok)
		}
	}
	if hashes != len(st.hashes) {
		t.Errorf("%d /hash records, but the stub answered %d", hashes, len(st.hashes))
	}
}

// TestRunWindowOpenLoop checks that a slow variant cannot hold sends back:
// every /hash answer takes 300 ms, longer than the whole window, and every
// send still goes out within 50 ms of its intended time, never before.
func TestRunWindowOpenLoop(t *testing.T) {
	const slow, maxLag = 300 * time.Millisecond, 50 * time.Millisecond
	pairs := testPairs(testSends)
	st := newStub(t, pairs, func(st *stub, w http.ResponseWriter, password string) {
		time.Sleep(slow)
		st.okHash(w, password)
	})
	s := NewSender()
	tg := newTarget(t, "v1", st.url, testSends)
	_, recs := runWindow(t, s, tg, "run1", pairs)
	slowed := 0
	for _, rec := range recs {
		checkSuccess(t, rec)
		if lag := rec.ActualMono - rec.IntendedMono; lag < 0 || lag >= maxLag {
			t.Errorf("Index %d: lag %v, want 0 to %v", rec.Index, lag, maxLag)
		}
		if rec.Kind == KindHash && rec.Latency >= slow {
			slowed++
		}
	}
	if slowed == 0 {
		t.Error("no /hash record took the stub's delay")
	}
}

// TestRunWindowSkipped checks a window with too few pairs: each /verify
// drawn beyond them is a Skipped record and is never sent.
func TestRunWindowSkipped(t *testing.T) {
	const few = 5
	pairs := testPairs(few)
	st := newStub(t, pairs, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, testSends)
	_, recs := runWindow(t, s, tg, "run1", pairs)
	draws, skipped := 0, 0
	for _, rec := range recs {
		if rec.Kind == KindVerify {
			draws++
		}
		if !rec.Skipped {
			checkSuccess(t, rec)
			continue
		}
		skipped++
		if rec.Kind != KindVerify || rec.ActualMono != 0 || rec.Latency != 0 || rec.Verdict != (Verdict{}) || rec.Fault != "" || rec.Pair != nil || rec.Evidence != nil {
			t.Errorf("Index %d: skipped record %+v, want Kind verify and nothing else set", rec.Index, rec)
		}
	}
	if draws <= few {
		t.Fatalf("%d /verify draws in %d sends, want more than %d pairs", draws, testSends, few)
	}
	if skipped != draws-few {
		t.Errorf("%d skipped, want %d /verify draws - %d pairs", skipped, draws, few)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if seen := st.seen.Load(); st.verifies != few || seen != int64(testSends-skipped) {
		t.Errorf("stub saw %d requests, %d of them pair verifies; want %d and %d", seen, st.verifies, testSends-skipped, few)
	}
}

// overCap returns the stub's over-cap /hash answer for hash: its hash
// body padded to MaxBodyBytes+100 bytes.
func overCap(hash string) []byte { return padTo(hashJSON(hash), classifier.MaxBodyBytes+100) }

// TestRunWindowEvidence checks the evidence cap: every /hash answer is a
// 200 body over MaxBodyBytes (malformed_body, an integrity reason), and
// exactly 8 records per window carry Evidence, again for a second runID
// on the same Target. Each answer starts with its own fresh hash, so the
// Evidence is matched to the request and answer of its own send.
func TestRunWindowEvidence(t *testing.T) {
	pairs := testPairs(2 * testSends)
	st := newStub(t, pairs, func(st *stub, w http.ResponseWriter, password string) {
		hash := withSalt(newSalt())
		st.mu.Lock()
		st.hashes[hash] = password
		st.mu.Unlock()
		w.Write(overCap(hash))
	})
	s := NewSender()
	tg := newTarget(t, "v1", st.url, 2*testSends)
	for k, runID := range []string{"run1", "run2"} {
		_, recs := runWindow(t, s, tg, runID, pairs[k*testSends:(k+1)*testSends])
		integrity, evidence := 0, 0
		for _, rec := range recs {
			if rec.Kind != KindHash {
				checkSuccess(t, rec)
				if rec.Evidence != nil {
					t.Errorf("Index %d: %s success carries Evidence", rec.Index, rec.Kind)
				}
				continue
			}
			if rec.Verdict != verdict(classifier.ReasonMalformedBody) || rec.Pair != nil {
				t.Errorf("Index %d: /hash Verdict %q, pair %+v; want malformed_body, no pair", rec.Index, rec.Verdict.Reason, rec.Pair)
			}
			integrity++
			ev := rec.Evidence
			if ev == nil {
				continue
			}
			evidence++
			hash, err := hashField(ev.Body) // the cut falls in the padding
			st.mu.Lock()
			password, answered := st.hashes[hash]
			st.mu.Unlock()
			if err != nil || !answered || !bytes.Equal(ev.RequestBody, hashBody(password)) || ev.Status != http.StatusOK ||
				!bytes.Equal(ev.Body, overCap(hash)[:classifier.MaxBodyBytes+1]) {
				t.Errorf("Index %d: Evidence status %d, %d body bytes, request body %.40q; want 200, the first %d bytes of the answer and its own request's body",
					rec.Index, ev.Status, len(ev.Body), ev.RequestBody, classifier.MaxBodyBytes+1)
			}
		}
		if integrity <= 8 {
			t.Fatalf("%s: %d integrity records, want more than 8", runID, integrity)
		}
		if evidence != 8 {
			t.Errorf("%s: %d records carry Evidence, want 8", runID, evidence)
		}
	}
}

// TestRunWindowNoEvidence checks that failures that are not integrity
// reasons carry no Evidence: every /hash answer is a 500 (bad_status).
func TestRunWindowNoEvidence(t *testing.T) {
	pairs := testPairs(testSends)
	st := newStub(t, pairs, func(_ *stub, w http.ResponseWriter, _ string) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	s := NewSender()
	tg := newTarget(t, "v1", st.url, testSends)
	_, recs := runWindow(t, s, tg, "run1", pairs)
	failed := 0
	for _, rec := range recs {
		if rec.Kind == KindHash {
			if rec.Verdict != verdict(classifier.ReasonBadStatus) {
				t.Errorf("Index %d: /hash Verdict %q, want bad_status", rec.Index, rec.Verdict.Reason)
			}
			failed++
		}
		if rec.Evidence != nil {
			t.Errorf("Index %d: %s %q record carries Evidence", rec.Index, rec.Kind, rec.Verdict.Reason)
		}
	}
	if failed == 0 {
		t.Fatal("no /hash records")
	}
}

// TestRunWindowBuildFault checks a Target whose URL no request can be
// built from: every record is a Fault with its Kind, a zero Verdict (so
// no verdict from Check) and nothing sent.
func TestRunWindowBuildFault(t *testing.T) {
	s := NewSender()
	tg := newTarget(t, "v1", unparseableURL, testSends)
	_, recs := runWindow(t, s, tg, "run1", testPairs(testSends))
	for _, rec := range recs {
		if rec.Fault == "" || rec.Kind == "" || rec.Skipped || rec.Verdict != (Verdict{}) || rec.Pair != nil || rec.Evidence != nil || rec.ActualMono != 0 || rec.Latency != 0 {
			t.Errorf("Index %d: record %+v, want a Fault with its Kind and nothing sent", rec.Index, rec)
		}
	}
}

// TestScheduleFaults checks both faults next and Check can cause: a next
// error other than ErrOutOfPairs is a Fault record and is not sent, and a
// request of unknown kind is sent and then a Fault record, with a zero
// Verdict and no pair, for Check's error.
func TestScheduleFaults(t *testing.T) {
	const n = 20
	boom := errors.New("boom")
	st := newStub(t, nil, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	next := func(i int) (Request, error) {
		if i%2 == 1 {
			return Request{}, boom
		}
		req := hashRequest(newPassword())
		req.Kind = "bogus"
		return req, nil
	}
	start := time.Now()
	if err := s.schedule(context.Background(), tg, "run1", start, testRate, n, next); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	recs := readFrom(t, tg.Ring, 0)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	checkSchedule(t, s, recs, tg, "run1", start, testStep, 0)
	for _, rec := range recs {
		if rec.Verdict != (Verdict{}) || rec.Pair != nil || rec.Evidence != nil || rec.Skipped {
			t.Errorf("Index %d: record %+v, want a Fault and nothing else", rec.Index, rec)
		}
		if rec.Index%2 == 1 {
			if rec.Fault != boom.Error() || rec.Kind != "" || rec.ActualMono != 0 {
				t.Errorf("Index %d: Fault %q Kind %q ActualMono %v; want %q, none, 0", rec.Index, rec.Fault, rec.Kind, rec.ActualMono, boom)
			}
		} else if !strings.Contains(rec.Fault, "unknown request kind") || rec.Kind != "bogus" || rec.ActualMono <= 0 {
			t.Errorf("Index %d: Fault %q Kind %q ActualMono %v; want Check's unknown kind error, bogus, sent", rec.Index, rec.Fault, rec.Kind, rec.ActualMono)
		}
	}
	if seen := st.seen.Load(); seen != n/2 {
		t.Errorf("stub saw %d requests, want %d", seen, n/2)
	}
}

// TestScheduleLate checks a schedule whose start is a second in the
// past: no send is skipped, each goes out at once, and ActualMono is
// when it went out, not its intended time.
func TestScheduleLate(t *testing.T) {
	const n, behind = 20, time.Second
	st := newStub(t, nil, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	start := time.Now().Add(-behind)
	called := time.Since(s.base)
	err := s.schedule(context.Background(), tg, "run1", start, testRate, n, func(int) (Request, error) {
		return hashRequest(newPassword()), nil
	})
	returned := time.Since(s.base)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	recs := readFrom(t, tg.Ring, 0)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	checkSchedule(t, s, recs, tg, "run1", start, testStep, 0)
	for _, rec := range recs {
		checkSuccess(t, rec)
		if rec.ActualMono < called || rec.ActualMono > returned {
			t.Errorf("Index %d: ActualMono %v, want between the call %v and its return %v", rec.Index, rec.ActualMono, called, returned)
		}
	}
	if took := returned - called; took >= behind/2 {
		t.Errorf("a schedule %v behind took %v, want its sends at once", behind, took)
	}
}

// TestScheduleSlowNext checks that next's time is not lag: next takes
// 30 ms of each 50 ms step, and every send still goes out within 20 ms
// of its intended time. That holds only if each request is built before
// its wait and each wait runs to start + i/rate, not for a fixed period.
func TestScheduleSlowNext(t *testing.T) {
	const n, rate, slow, maxLag = 10, 20, 30 * time.Millisecond, 20 * time.Millisecond
	st := newStub(t, nil, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	start := time.Now().Add(2 * slow)
	err := s.schedule(context.Background(), tg, "run1", start, rate, n, func(int) (Request, error) {
		time.Sleep(slow)
		return hashRequest(newPassword()), nil
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	recs := readFrom(t, tg.Ring, 0)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	checkSchedule(t, s, recs, tg, "run1", start, time.Second/rate, 0)
	for _, rec := range recs {
		checkSuccess(t, rec)
		if lag := rec.ActualMono - rec.IntendedMono; lag < 0 || lag >= maxLag {
			t.Errorf("Index %d: lag %v, want 0 to %v", rec.Index, lag, maxLag)
		}
	}
}

// TestScheduleCatchUp checks that a stalled scheduler catches up rather
// than shifting the rest of the schedule: next(3) takes 70 ms at a 20 ms
// step, so send 3 goes out about 50 ms late and sends 4 and 5 at once,
// and from send 6 on every send is back within 25 ms of its intended
// time. A timer reset for one period after each wake keeps the 50 ms for
// every later send.
func TestScheduleCatchUp(t *testing.T) {
	const n, rate, stall, stalled, onTime = 10, 50, 70 * time.Millisecond, 3, 25 * time.Millisecond
	st := newStub(t, nil, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	start := time.Now().Add(onTime)
	err := s.schedule(context.Background(), tg, "run1", start, rate, n, func(i int) (Request, error) {
		if i == stalled {
			time.Sleep(stall)
		}
		return hashRequest(newPassword()), nil
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	recs := readFrom(t, tg.Ring, 0)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	checkSchedule(t, s, recs, tg, "run1", start, time.Second/rate, 0)
	for _, rec := range recs {
		checkSuccess(t, rec)
		lag := rec.ActualMono - rec.IntendedMono
		switch {
		case rec.Index == stalled && lag < stall-time.Second/rate:
			t.Errorf("Index %d: lag %v, want at least %v after the stall", rec.Index, lag, stall-time.Second/rate)
		case rec.Index >= stalled+3 && (lag < 0 || lag >= onTime):
			t.Errorf("Index %d: lag %v, want 0 to %v once caught up", rec.Index, lag, onTime)
		}
	}
}

// TestRunBurst checks a burst of 40 at 400 per second: every send
// recorded once at start + i/400, all successes, in BurstCounts(40)'s
// kinds.
func TestRunBurst(t *testing.T) {
	const n, rate, step = 40, 400, 2500 * time.Microsecond
	hashes, verifies, negatives, err := BurstCounts(n)
	if err != nil {
		t.Fatal(err)
	}
	pairs := testPairs(verifies)
	st := newStub(t, pairs, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	start := time.Now()
	if err := s.RunBurst(context.Background(), tg, "burst1", start, rate, n, pairs); err != nil {
		t.Fatalf("RunBurst: %v", err)
	}
	recs := readFrom(t, tg.Ring, 0)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	checkSchedule(t, s, recs, tg, "burst1", start, step, 0)
	counts, classes, wantClasses := map[Kind]int{}, map[NegativeClass]int{}, map[NegativeClass]int{}
	for _, rec := range recs {
		checkSuccess(t, rec)
		counts[rec.Kind]++
		if rec.Kind == KindNegative || rec.Class != "" {
			classes[rec.Class]++
		}
	}
	for i := range negatives {
		wantClasses[negativeClasses[i%len(negativeClasses)]]++
	}
	if !maps.Equal(classes, wantClasses) {
		t.Errorf("negative classes %v, want %v", classes, wantClasses)
	}
	if counts[KindHash] != hashes || counts[KindVerify] != verifies || counts[KindNegative] != negatives || len(counts) != 3 {
		t.Errorf("kind counts %v, want hash %d, verify %d, negative %d", counts, hashes, verifies, negatives)
	}
}

// TestRunErrors checks that RunWindow's and RunBurst's argument errors
// come before anything is sent or recorded.
func TestRunErrors(t *testing.T) {
	st := newStub(t, nil, nil)
	s := NewSender()
	tg := newTarget(t, "v1", st.url, 100)
	ctx, start, pairs := context.Background(), time.Now(), testPairs(100)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"window rate 0", func() error { return s.RunWindow(ctx, tg, "r", start, 0, testWindow, pairs) }},
		{"window NaN", func() error { return s.RunWindow(ctx, tg, "r", start, math.NaN(), testWindow, pairs) }},
		{"window too short", func() error { return s.RunWindow(ctx, tg, "r", start, testRate, time.Millisecond, pairs) }},
		{"burst rate 0", func() error { return s.RunBurst(ctx, tg, "r", start, 0, 40, pairs) }},
		{"burst rate -1", func() error { return s.RunBurst(ctx, tg, "r", start, -1, 40, pairs) }},
		{"burst NaN", func() error { return s.RunBurst(ctx, tg, "r", start, math.NaN(), 40, pairs) }},
		{"burst +Inf", func() error { return s.RunBurst(ctx, tg, "r", start, math.Inf(1), 40, pairs) }},
		{"burst -Inf", func() error { return s.RunBurst(ctx, tg, "r", start, math.Inf(-1), 40, pairs) }},
		{"burst n too small", func() error { return s.RunBurst(ctx, tg, "r", start, 400, 4, pairs) }},
		{"burst out of pairs", func() error { return s.RunBurst(ctx, tg, "r", start, 400, 40, pairs[:1]) }},
	} {
		if err := tc.run(); err == nil {
			t.Errorf("%s: nil error", tc.name)
		}
	}
	if recs, _ := tg.Ring.Read(0); len(recs) != 0 || st.seen.Load() != 0 {
		t.Errorf("%d records written and %d requests sent, want none", len(recs), st.seen.Load())
	}
}

// TestRunWindowCancel cancels a window a quarter of the way in, with
// /hash answers taking 100 ms so sends are in flight then: RunWindow
// returns ctx.Err() only once every launched send is recorded, so the
// records are exactly the sends the stub saw, a run of indices from 0,
// fewer than the window's.
func TestRunWindowCancel(t *testing.T) {
	const w, n = time.Second, 200
	pairs := testPairs(n)
	st := newStub(t, pairs, func(st *stub, w http.ResponseWriter, password string) {
		time.Sleep(100 * time.Millisecond)
		st.okHash(w, password)
	})
	s := NewSender()
	tg := newTarget(t, "v1", st.url, n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(w/4, cancel)
	start := time.Now()
	if err := s.RunWindow(ctx, tg, "run1", start, testRate, w, pairs); err != context.Canceled {
		t.Fatalf("RunWindow = %v, want %v", err, context.Canceled)
	}
	recs := readFrom(t, tg.Ring, 0)
	if seen := st.seen.Load(); int64(len(recs)) != seen || len(recs) == 0 || len(recs) >= n {
		t.Fatalf("%d records, stub saw %d; want equal, between 1 and %d", len(recs), seen, n-1)
	}
	checkSchedule(t, s, recs, tg, "run1", start, testStep, 0)
	for _, rec := range recs {
		checkSuccess(t, rec)
	}
}

// TestRunWindowConcurrent runs windows to two Targets at once on one
// Sender. Run it under -race.
func TestRunWindowConcurrent(t *testing.T) {
	s := NewSender()
	tgs := make([]Target, 2)
	pairs := make([][]Pair, len(tgs))
	for i := range tgs {
		pairs[i] = testPairs(testSends)
		tgs[i] = newTarget(t, fmt.Sprintf("v%d", i), newStub(t, pairs[i], nil).url, testSends)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for i, tg := range tgs {
		wg.Go(func() {
			if err := s.RunWindow(context.Background(), tg, "run1", start, testRate, testWindow, pairs[i]); err != nil {
				t.Errorf("%s: RunWindow: %v", tg.ID, err)
			}
		})
	}
	wg.Wait()
	for _, tg := range tgs {
		recs := readFrom(t, tg.Ring, 0)
		if len(recs) != testSends {
			t.Fatalf("%s: %d records, want %d", tg.ID, len(recs), testSends)
		}
		checkSchedule(t, s, recs, tg, "run1", start, testStep, 0)
		for _, rec := range recs {
			checkSuccess(t, rec)
		}
	}
}
