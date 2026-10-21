package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shedder is a fake variant: it answers every request 503 with
// Retry-After: 1, once hold is closed.
type shedder struct {
	url  string
	seen atomic.Int64 // requests received
	hold chan struct{}
	once sync.Once
}

// newShedder starts a shedder, holding every request until release if
// held is set.
func newShedder(t *testing.T, held bool) *shedder {
	t.Helper()
	sh := &shedder{hold: make(chan struct{})}
	if !held {
		sh.release()
	}
	sh.url = serve(t, func(w http.ResponseWriter, r *http.Request) {
		sh.seen.Add(1)
		io.Copy(io.Discard, r.Body)
		<-sh.hold
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}).URL
	t.Cleanup(sh.release) // runs before serve's Close, which waits for held requests
	return sh
}

func (sh *shedder) release() { sh.once.Do(func() { close(sh.hold) }) }

// runConfig returns a Config whose targets are url/v{id}: a window of
// testSends (50) sends at testRate (200/s), over maxRate, and a burst of
// 20 at 200/s.
func runConfig(url string) Config {
	c := validConfig()
	c.Rate, c.Window, c.maxRate = testRate, testWindow, testRate
	c.BurstRate, c.burstDuration = 200, 100*time.Millisecond
	c.TargetURL = url + "/v{id}"
	return c
}

// longConfig is runConfig with a window of 200 sends at 20/s, 10 s, long
// enough to cancel a run in the middle.
func longConfig(url string) Config {
	c := runConfig(url)
	c.Rate, c.Window = 20, 10*time.Second
	return c
}

// callBody sends one request with body to h and returns the response.
func callBody(h http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, bytes.NewReader(body)))
	return rec
}

// mustCallBody is callBody, failing the test unless the status is want.
func mustCallBody(t *testing.T, h http.Handler, method, target string, body []byte, want int) *httptest.ResponseRecorder {
	t.Helper()
	rec := callBody(h, method, target, body)
	if rec.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", method, target, rec.Code, rec.Body, want)
	}
	return rec
}

func pairsJSON(pairs []Pair) []byte {
	var u wireUpload
	for _, p := range pairs {
		u.Pairs = append(u.Pairs, wireUploadPair{&p.Password, &p.Hash, &p.Match})
	}
	return mustJSON(u)
}

// upload uploads n pairs to target id for run runID.
func upload(t *testing.T, h http.Handler, id, runID string, n int) {
	t.Helper()
	mustCallBody(t, h, "PUT", "/targets/"+id+"/pairs/"+runID, pairsJSON(testPairs(n)), http.StatusCreated)
}

func startJSON(kind string, ids ...string) []byte {
	return mustJSON(wireStart{kind, ids})
}

func start(t *testing.T, h http.Handler, runID, kind string, ids ...string) wireStarted {
	t.Helper()
	return decode[wireStarted](t, mustCallBody(t, h, "POST", "/runs/"+runID+"/start", startJSON(kind, ids...), http.StatusOK))
}

// waitFor fails the test unless cond holds within 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// waitRun waits until no target of run runID is running, and returns it.
func waitRun(t *testing.T, h http.Handler, runID string) wireRun {
	t.Helper()
	var v wireRun
	waitFor(t, "run "+runID+" to end", func() bool {
		v = decode[wireRun](t, mustCall(t, h, "GET", "/runs/"+runID, http.StatusOK))
		for _, s := range v.Targets {
			if s.State == stateRunning {
				return false
			}
		}
		return true
	})
	return v
}

func wantStates(t *testing.T, v wireRun, want map[string]wireRunTarget) {
	t.Helper()
	if len(v.Targets) != len(want) {
		t.Errorf("run %s targets = %+v, want %+v", v.RunID, v.Targets, want)
	}
	for id, w := range want {
		if got := v.Targets[id]; got != w {
			t.Errorf("run %s target %s = %+v, want %+v", v.RunID, id, got, w)
		}
	}
}

// allRecords returns target id's records, read through the API.
func allRecords(t *testing.T, h http.Handler, id string) []wireRecord {
	t.Helper()
	page := decode[wireRecords](t, mustCall(t, h, "GET", "/targets/"+id+"/records?since=0", http.StatusOK))
	if page.Gap {
		t.Fatalf("target %s's records have a gap", id)
	}
	return page.Records
}

// pendingBatch returns target id's pending batch's run_id and size, read
// under c.mu: "", 0 for none.
func (c *Control) pendingBatch(id string) (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.targets[id].pending; b != nil {
		return b.runID, len(b.pairs)
	}
	return "", 0
}

func (c *Control) wantPending(t *testing.T, id, runID string, n int) {
	t.Helper()
	if gotID, gotN := c.pendingBatch(id); gotID != runID || gotN != n {
		t.Errorf("target %s pending = %q, %d pairs; want %q, %d", id, gotID, gotN, runID, n)
	}
}

// idle checks that no run is active and no target is busy.
func (c *Control) wantIdle(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		t.Errorf("run %s still active", c.active.id)
	}
	for id, tg := range c.targets {
		if tg.busy {
			t.Errorf("target %s still busy", id)
		}
	}
}

func (c *Control) runCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.runs)
}

func TestUploadPairs(t *testing.T) {
	c := newTestControl(t, runConfig("http://127.0.0.1:1"))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)

	got := decode[wireUploaded](t, mustCallBody(t, h, "PUT", "/targets/1/pairs/w1", pairsJSON(testPairs(3)), http.StatusCreated))
	if want := (wireUploaded{c.bootID, 3}); got != want {
		t.Errorf("upload = %+v, want %+v", got, want)
	}
	c.wantPending(t, "1", "w1", 3)
	pairs := testPairs(3)
	mustCallBody(t, h, "PUT", "/targets/1/pairs/w1b", pairsJSON(pairs), http.StatusCreated)
	c.mu.Lock()
	gotPairs := c.targets["1"].pending.pairs
	c.mu.Unlock()
	if !slices.Equal(gotPairs, pairs) {
		t.Errorf("pending pairs = %+v, want %+v", gotPairs, pairs)
	}

	errorBody(t, callBody(h, "PUT", "/targets/1/pairs/w1b", pairsJSON(testPairs(5))), http.StatusConflict)
	c.wantPending(t, "1", "w1b", 3)
	upload(t, h, "1", "w2", 5) // another run_id replaces the batch
	c.wantPending(t, "1", "w2", 5)
	errorBody(t, callBody(h, "PUT", "/targets/9/pairs/w3", pairsJSON(testPairs(5))), http.StatusNotFound)
	c.setBusy("1", true)
	errorBody(t, callBody(h, "PUT", "/targets/1/pairs/w3", pairsJSON(testPairs(5))), http.StatusConflict)
	c.setBusy("1", false)
	c.wantPending(t, "1", "w2", 5)

	valid := string(pairsJSON(testPairs(1)))
	for name, body := range map[string]string{
		"empty":            "",
		"not JSON":         "x",
		"array":            "[]",
		"unterminated":     valid[:len(valid)-1],
		"unknown field":    `{"pairs":[{"password":"p","hash":"h","match":true}],"extra":1}`,
		"unknown in pair":  `{"pairs":[{"password":"p","hash":"h","match":true,"salt":"s"}]}`,
		"wrong type":       `{"pairs":[{"password":1,"hash":"h","match":true}]}`,
		"match a string":   `{"pairs":[{"password":"p","hash":"h","match":"true"}]}`,
		"trailing value":   valid + `{}`,
		"trailing garbage": valid + "x",
		"trailing brace":   valid + "}",
		"no pairs":         `{"pairs":[]}`,
		"null pairs":       `{"pairs":null}`,
		"no pairs field":   `{}`,
	} {
		if rec := callBody(h, "PUT", "/targets/1/pairs/w4", []byte(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: upload = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	c.wantPending(t, "1", "w2", 5)

	// Every pair must carry all three fields; the batch is refused whole.
	good := `{"password":"p","hash":"h","match":false}`
	for _, tc := range []struct{ pair, index, field string }{
		{`{"hash":"h","match":true}`, "0", "password"},
		{`{"password":"p","match":true}`, "0", "hash"},
		{`{"password":"p","hash":"h"}`, "0", "match"},
		{`{"password":null,"hash":"h","match":true}`, "0", "password"},
		{`{"password":"p","hash":null,"match":true}`, "0", "hash"},
		{`{"password":"p","hash":"h","match":null}`, "0", "match"},
		{good + `,{"password":"p","hash":"h"}`, "1", "match"},
	} {
		msg := errorBody(t, callBody(h, "PUT", "/targets/1/pairs/w4", []byte(`{"pairs":[`+tc.pair+`]}`)), http.StatusBadRequest)
		if want := "pair " + tc.index + " has no " + tc.field; !strings.Contains(msg, want) {
			t.Errorf("pairs %s: error %q, want %q in it", tc.pair, msg, want)
		}
	}
	c.wantPending(t, "1", "w2", 5)
	mustCallBody(t, h, "PUT", "/targets/1/pairs/w5", []byte(`{"pairs":[`+good+`]}`), http.StatusCreated)
	mustCallBody(t, h, "PUT", "/targets/1/pairs/w6", []byte(valid+" \n\t"), http.StatusCreated)

	// The run_id is checked before the body is read: a body over the cap
	// still gets 400.
	over := bytes.Repeat([]byte(" "), int(c.pairsCap)+1)
	for _, id := range []string{"a%20b", "a%2Fb", "a+b", "a:b", "%C3%A9", strings.Repeat("a", 65)} {
		errorBody(t, callBody(h, "PUT", "/targets/1/pairs/"+id, over), http.StatusBadRequest)
	}
	for _, id := range []string{"a", "A.b_c-9", strings.Repeat("a", 64)} {
		upload(t, h, "1", id, 1)
	}
}

// TestValidRunID checks the run_id format directly: the ServeMux cleans
// dot segments before a handler sees them.
func TestValidRunID(t *testing.T) {
	for _, id := range []string{"x.", "a.b", "a", "-", "_x", "A.b_c-9", strings.Repeat("a", 64)} {
		if !validRunID(id) {
			t.Errorf("validRunID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", ".", "..", ".x", "a/b", "a b", "a+b", "\u00e9", strings.Repeat("a", 65)} {
		if validRunID(id) {
			t.Errorf("validRunID(%q) = true, want false", id)
		}
	}
}

// TestUploadCap checks the upload body cap: max(window_pairs,
// burst_pairs) pairs of the largest size fit, and a body over the cap is
// 413.
func TestUploadCap(t *testing.T) {
	c := newTestControl(t, runConfig("http://127.0.0.1:1"))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	if want := int64(max(c.windowPairs, c.burstPairs))*1161 + 4096; c.pairsCap != want {
		t.Fatalf("pairsCap = %d, want %d", c.pairsCap, want)
	}

	password, hash, match := strings.Repeat("A", maxPasswordLen), randomExactPHC(), false
	big := wireUploadPair{&password, &hash, &match}
	if n := len(mustJSON(big)) + len(","); n != maxPairJSON {
		t.Errorf("the largest pair takes %d bytes, want maxPairJSON = %d", n, maxPairJSON)
	}
	worst := wireUpload{}
	for range max(c.windowPairs, c.burstPairs) {
		worst.Pairs = append(worst.Pairs, big)
	}
	mustCallBody(t, h, "PUT", "/targets/1/pairs/worst", mustJSON(worst), http.StatusCreated)

	padded := func(n int64) []byte {
		b := pairsJSON(testPairs(1))
		return append(b, bytes.Repeat([]byte(" "), int(n)-len(b))...)
	}
	mustCallBody(t, h, "PUT", "/targets/1/pairs/under", padded(c.pairsCap-1), http.StatusCreated)
	mustCallBody(t, h, "PUT", "/targets/1/pairs/at", padded(c.pairsCap), http.StatusCreated)
	errorBody(t, callBody(h, "PUT", "/targets/1/pairs/over", padded(c.pairsCap+1)), http.StatusRequestEntityTooLarge)
	c.wantPending(t, "1", "at", 1)
}

func TestStartRefusals(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, runConfig(sh.url))
	h := c.Handler()
	for _, id := range []string{"1", "2", "3", "4"} {
		mustCall(t, h, "PUT", "/targets/"+id, http.StatusCreated)
	}
	upload(t, h, "1", "w1", c.windowPairs)
	unchanged := func(name string) {
		t.Helper()
		c.wantPending(t, "1", "w1", c.windowPairs)
		if n := c.runCount(); n != 0 {
			t.Errorf("%s: %d runs, want 0", name, n)
		}
		c.wantIdle(t)
	}

	for name, body := range map[string]string{
		"empty":            "",
		"not JSON":         "x",
		"unknown field":    `{"kind":"window","targets":["1"],"x":1}`,
		"trailing":         `{"kind":"window","targets":["1"]}{}`,
		"no kind":          `{"targets":["1"]}`,
		"kind case":        `{"kind":"Window","targets":["1"]}`,
		"kind unknown":     `{"kind":"stream","targets":["1"]}`,
		"no targets":       `{"kind":"window"}`,
		"empty targets":    `{"kind":"window","targets":[]}`,
		"duplicate":        `{"kind":"window","targets":["1","2","1"]}`,
		"burst of two":     `{"kind":"burst","targets":["1","2"]}`,
		"burst of none":    `{"kind":"burst","targets":[]}`,
		"number targets":   `{"kind":"window","targets":[1]}`,
		"targets a string": `{"kind":"window","targets":"1"}`,
	} {
		if rec := callBody(h, "POST", "/runs/w1/start", []byte(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: start = %d %s, want 400", name, rec.Code, rec.Body)
		}
		unchanged(name)
	}
	for _, id := range []string{"a%20b", "a%2Fb", strings.Repeat("a", 65)} {
		errorBody(t, callBody(h, "POST", "/runs/"+id+"/start", startJSON("window", "1")), http.StatusBadRequest)
	}
	unchanged("bad run_id")

	upload(t, h, "2", "w1", c.windowPairs-1)
	upload(t, h, "3", "w0", c.windowPairs)
	upload(t, h, "4", "w1", c.burstPairs-1)
	for _, tc := range []struct {
		name string
		body []byte
		msg  string
	}{
		{"missing target", startJSON("window", "1", "9"), `no target "9"`},
		{"short window batch", startJSON("window", "1", "2"), "target 2's batch holds"},
		{"batch for another run", startJSON("window", "3", "1"), "target 3 has no batch for run_id w1"},
		{"short burst batch", startJSON("burst", "4"), "target 4's batch holds"},
	} {
		if msg := errorBody(t, callBody(h, "POST", "/runs/w1/start", tc.body), http.StatusConflict); !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: error %q, want %q in it", tc.name, msg, tc.msg)
		}
		unchanged(tc.name)
	}
	c.setBusy("2", true)
	upload(t, h, "3", "w1", c.windowPairs)
	if msg := errorBody(t, callBody(h, "POST", "/runs/w1/start", startJSON("window", "1", "3", "2")), http.StatusConflict); !strings.Contains(msg, "target 2 is in a run") {
		t.Errorf("busy: error %q", msg)
	}
	c.setBusy("2", false)
	c.wantPending(t, "3", "w1", c.windowPairs)
	unchanged("busy")
	errorBody(t, call(h, "GET", "/runs/w1"), http.StatusNotFound)

	// A burst takes exactly one target with burst_pairs.
	upload(t, h, "4", "b1", c.burstPairs)
	start(t, h, "b1", "burst", "4")
	wantStates(t, waitRun(t, h, "b1"), map[string]wireRunTarget{"4": {State: stateDone}})
}

// TestStartCap checks the start body cap on both sides: a body of
// exactly 64 KiB starts the run, and one byte more is 413 and takes no
// batch.
func TestStartCap(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, runConfig(sh.url))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)
	padded := func(n int) []byte {
		b := startJSON("window", "1")
		return append(b, bytes.Repeat([]byte(" "), n-len(b))...)
	}

	errorBody(t, callBody(h, "POST", "/runs/w1/start", padded(64<<10+1)), http.StatusRequestEntityTooLarge)
	c.wantPending(t, "1", "w1", c.windowPairs)
	mustCallBody(t, h, "POST", "/runs/w1/start", padded(64<<10), http.StatusOK)
	wantStates(t, waitRun(t, h, "w1"), map[string]wireRunTarget{"1": {State: stateDone}})
}

// TestStartAllOrNothing checks that a start refused on one target takes
// no other target's batch, and that it starts once fixed.
func TestStartAllOrNothing(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, runConfig(sh.url))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	mustCall(t, h, "PUT", "/targets/2", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)

	errorBody(t, callBody(h, "POST", "/runs/w1/start", startJSON("window", "1", "2")), http.StatusConflict)
	c.wantPending(t, "1", "w1", c.windowPairs)
	c.wantIdle(t)
	if n := c.runCount(); n != 0 {
		t.Fatalf("%d runs after a refused start, want 0", n)
	}
	errorBody(t, call(h, "GET", "/runs/w1"), http.StatusNotFound)

	upload(t, h, "2", "w1", c.windowPairs)
	start(t, h, "w1", "window", "1", "2")
	c.wantPending(t, "1", "", 0)
	c.wantPending(t, "2", "", 0)
	wantStates(t, waitRun(t, h, "w1"), map[string]wireRunTarget{"1": {State: stateDone}, "2": {State: stateDone}})
}

// TestStartWhileActive checks that a run is refused while another is
// active, that start sets busy, and that a run_id is never reused.
func TestStartWhileActive(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, longConfig(sh.url))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	mustCall(t, h, "PUT", "/targets/2", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)
	upload(t, h, "2", "w2", c.windowPairs)
	start(t, h, "w1", "window", "1")

	if msg := errorBody(t, callBody(h, "POST", "/runs/w2/start", startJSON("window", "2")), http.StatusConflict); !strings.Contains(msg, "run w1 is active") {
		t.Errorf("second start: error %q, want run w1 is active", msg)
	}
	c.wantPending(t, "2", "w2", c.windowPairs)
	if msg := errorBody(t, callBody(h, "PUT", "/targets/1/pairs/w3", pairsJSON(testPairs(1))), http.StatusConflict); !strings.Contains(msg, "in a run") {
		t.Errorf("upload to a running target: error %q, want in a run", msg)
	}
	mustCall(t, h, "POST", "/runs/w1/cancel", http.StatusOK)
	wantStates(t, waitRun(t, h, "w1"), map[string]wireRunTarget{"1": {State: stateCancelled}})
	c.wantIdle(t)

	// w1 is used: neither an upload nor a start may name it again.
	if msg := errorBody(t, callBody(h, "PUT", "/targets/2/pairs/w1", pairsJSON(testPairs(1))), http.StatusConflict); !strings.Contains(msg, "already used") {
		t.Errorf("upload under a used run_id: error %q, want already used", msg)
	}
	if msg := errorBody(t, callBody(h, "POST", "/runs/w1/start", startJSON("window", "2")), http.StatusConflict); !strings.Contains(msg, "already used") {
		t.Errorf("start under a used run_id: error %q, want already used", msg)
	}
	c.wantPending(t, "2", "w2", c.windowPairs)
}

// TestWindowRun runs one window on two targets to the end.
func TestWindowRun(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, runConfig(sh.url))
	h := c.Handler()
	ids := []string{"1", "2"}
	for _, id := range ids {
		mustCall(t, h, "PUT", "/targets/"+id, http.StatusCreated)
		upload(t, h, id, "w1", c.windowPairs)
	}
	before := time.Now()
	st := start(t, h, "w1", "window", ids...)
	if st.BootID != c.bootID || st.RunID != "w1" || st.StartWall.Location() != time.UTC {
		t.Errorf("start = %+v", st)
	}
	if lead := st.StartWall.Sub(before.Round(0)); lead < startLead || lead > startLead+time.Second {
		t.Errorf("start_wall is %v after the request, want about %v", lead, startLead)
	}
	// The first send is startLead away, so both are still running.
	wantStates(t, decode[wireRun](t, mustCall(t, h, "GET", "/runs/w1", http.StatusOK)),
		map[string]wireRunTarget{"1": {State: stateRunning}, "2": {State: stateRunning}})

	v := waitRun(t, h, "w1")
	if v.BootID != c.bootID || v.RunID != "w1" || v.Kind != "window" || !v.StartWall.Equal(st.StartWall) {
		t.Errorf("run = %+v, want w1, window, start_wall %v", v, st.StartWall)
	}
	wantStates(t, v, map[string]wireRunTarget{"1": {State: stateDone}, "2": {State: stateDone}})
	c.wantIdle(t)

	for _, id := range ids {
		recs := allRecords(t, h, id)
		if len(recs) != c.windowSends || c.windowSends != testSends {
			t.Fatalf("target %s: %d records, want %d", id, len(recs), testSends)
		}
		seen := make([]bool, len(recs))
		for _, r := range recs {
			if r.RunID != "w1" || r.Target != id || r.Fault != "" || r.Index < 0 || r.Index >= len(recs) || seen[r.Index] {
				t.Fatalf("target %s: record %+v", id, r)
			}
			seen[r.Index] = true
			if want := st.StartWall.Add(time.Duration(r.Index) * testStep); !r.IntendedWall.Equal(want) {
				t.Errorf("target %s: record %d intended_wall %v, want %v", id, r.Index, r.IntendedWall, want)
			}
		}
		c.wantPending(t, id, "", 0)
	}
	if n := sh.seen.Load(); n != int64(len(ids)*testSends) {
		t.Errorf("the variants got %d requests, want %d", n, len(ids)*testSends)
	}
}

// TestBurstRun runs one burst to the end.
func TestBurstRun(t *testing.T) {
	sh := newShedder(t, false)
	c := newTestControl(t, runConfig(sh.url))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/5", http.StatusCreated)
	upload(t, h, "5", "b1", c.burstPairs)
	start(t, h, "b1", "burst", "5")
	v := waitRun(t, h, "b1")
	if v.Kind != "burst" {
		t.Errorf("kind %q, want burst", v.Kind)
	}
	wantStates(t, v, map[string]wireRunTarget{"5": {State: stateDone}})
	c.wantIdle(t)

	recs := allRecords(t, h, "5")
	if len(recs) != c.burstSends || c.burstSends != 20 {
		t.Fatalf("%d records, burst_sends %d; want 20", len(recs), c.burstSends)
	}
	kinds := map[string]int{}
	for _, r := range recs {
		if r.RunID != "b1" || r.Fault != "" {
			t.Fatalf("record %+v", r)
		}
		kinds[r.Kind]++
	}
	hashes, verifies, negatives, _ := BurstCounts(c.burstSends)
	if want := map[string]int{"hash": hashes, "verify": verifies, "negative": negatives}; fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("kinds %v, want %v", kinds, want)
	}
	if n := sh.seen.Load(); n != int64(c.burstSends) {
		t.Errorf("the variant got %d requests, want %d", n, c.burstSends)
	}
}

// TestCancelRun checks that a cancelled run stays running until its
// sends in flight are recorded, and that its target stays busy until
// then.
func TestCancelRun(t *testing.T) {
	sh := newShedder(t, true)
	c := newTestControl(t, longConfig(sh.url))
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)
	start(t, h, "w1", "window", "1")
	waitFor(t, "a send in flight", func() bool { return sh.seen.Load() > 0 })

	v := decode[wireRun](t, mustCall(t, h, "POST", "/runs/w1/cancel", http.StatusOK))
	wantStates(t, v, map[string]wireRunTarget{"1": {State: stateRunning}})
	time.Sleep(50 * time.Millisecond)
	v = decode[wireRun](t, mustCall(t, h, "GET", "/runs/w1", http.StatusOK))
	wantStates(t, v, map[string]wireRunTarget{"1": {State: stateRunning}})
	rec := call(h, "DELETE", fmt.Sprintf("/targets/1?through=%d", uint64(math.MaxUint64)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in a run") {
		t.Fatalf("DELETE while running = %d %s, want 409 in a run", rec.Code, rec.Body)
	}

	sh.release()
	wantStates(t, waitRun(t, h, "w1"), map[string]wireRunTarget{"1": {State: stateCancelled}})
	c.wantIdle(t)
	held := sh.seen.Load()
	next := c.target("1").Ring.Next()
	if next != uint64(held) {
		t.Errorf("%d records, want one per request sent (%d)", next, held)
	}
	// Cancelling a run that has ended changes nothing.
	v = decode[wireRun](t, mustCall(t, h, "POST", "/runs/w1/cancel", http.StatusOK))
	wantStates(t, v, map[string]wireRunTarget{"1": {State: stateCancelled}})
	errorBody(t, call(h, "POST", "/runs/w9/cancel"), http.StatusNotFound)
	errorBody(t, call(h, "GET", "/runs/w9"), http.StatusNotFound)
	mustCall(t, h, "DELETE", fmt.Sprintf("/targets/1?through=%d", next-1), http.StatusOK)
}

// TestWait checks that Wait returns at once with no run active, waits
// while a cancelled run's send is in flight, and returns once that send
// is recorded.
func TestWait(t *testing.T) {
	sh := newShedder(t, true)
	c := newTestControl(t, longConfig(sh.url))
	if err := c.Wait(t.Context()); err != nil {
		t.Fatalf("Wait with no run = %v, want nil", err)
	}
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)
	start(t, h, "w1", "window", "1")
	waitFor(t, "a send in flight", func() bool { return sh.seen.Load() > 0 })
	mustCall(t, h, "POST", "/runs/w1/cancel", http.StatusOK)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with a send held = %v, want %v", err, context.DeadlineExceeded)
	}
	sh.release()
	if err := c.Wait(t.Context()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	c.wantIdle(t)
	if next, seen := c.target("1").Ring.Next(), sh.seen.Load(); next != uint64(seen) {
		t.Errorf("after Wait, %d records, want one per request sent (%d)", next, seen)
	}
}

// TestRunBaseContext checks that a run's context derives from the
// Control's: cancelling it cancels a run under way, and a run started
// after it is cancelled sends nothing.
func TestRunBaseContext(t *testing.T) {
	sh := newShedder(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := NewControl(ctx, longConfig(sh.url), NewSender())
	if err != nil {
		t.Fatal(err)
	}
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	upload(t, h, "1", "w1", c.windowPairs)
	start(t, h, "w1", "window", "1")
	waitFor(t, "a send", func() bool { return sh.seen.Load() > 0 })
	cancel()
	wantStates(t, waitRun(t, h, "w1"), map[string]wireRunTarget{"1": {State: stateCancelled}})
	c.wantIdle(t)

	next, seen := c.target("1").Ring.Next(), sh.seen.Load()
	upload(t, h, "1", "w2", c.windowPairs)
	start(t, h, "w2", "window", "1")
	wantStates(t, waitRun(t, h, "w2"), map[string]wireRunTarget{"1": {State: stateCancelled}})
	if n, s := c.target("1").Ring.Next(), sh.seen.Load(); n != next || s != seen {
		t.Errorf("a run started under a cancelled context added %d records and sent %d requests", n-next, s-seen)
	}
}

// TestRunError checks that a target whose RunX errors reports error with
// its message.
func TestRunError(t *testing.T) {
	c := newTestControl(t, runConfig("http://127.0.0.1:1"))
	r := &run{id: "w1", kind: windowRun, cancel: func() {}, done: make(chan struct{}), targets: map[string]targetState{"1": {state: stateRunning}}}
	tg := &target{busy: true}
	tg.ID = "1"
	c.runs["w1"], c.active = r, r
	c.finish(r, tg, fmt.Errorf("loadgen: boom"))
	v := decode[wireRun](t, mustCall(t, c.Handler(), "GET", "/runs/w1", http.StatusOK))
	wantStates(t, v, map[string]wireRunTarget{"1": {State: stateError, Error: "loadgen: boom"}})
	if tg.busy || c.active != nil {
		t.Errorf("after the error: busy %v, active %v", tg.busy, c.active)
	}
	select {
	case <-r.done:
	default:
		t.Error("after the error, the run's done is open")
	}
	b, _ := json.Marshal(c.toWireRun(r.view()))
	if !strings.Contains(string(b), `"error":"loadgen: boom"`) {
		t.Errorf("wire run %s has no error", b)
	}
}
