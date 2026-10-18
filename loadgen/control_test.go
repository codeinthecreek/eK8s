package loadgen

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

func newTestControl(t *testing.T, cfg Config) *Control {
	t.Helper()
	c, err := NewControl(cfg, NewSender())
	if err != nil {
		t.Fatalf("NewControl: %v", err)
	}
	return c
}

// call sends one request to h and returns the response.
func call(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// mustCall is call, failing the test unless the status is want.
func mustCall(t *testing.T, h http.Handler, method, target string, want int) *httptest.ResponseRecorder {
	t.Helper()
	rec := call(h, method, target)
	if rec.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", method, target, rec.Code, rec.Body, want)
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body, err)
	}
	return v
}

// errorBody checks that rec is an error response with status want, and
// returns its error.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder, want int) string {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, want)
	}
	e := decode[wireError](t, rec)
	if e.Error == "" {
		t.Errorf("error body %s has no error", rec.Body)
	}
	return e.Error
}

// target returns c's target id, read under c.mu.
func (c *Control) target(id string) *target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targets[id]
}

func (c *Control) setBusy(id string, busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets[id].busy = busy
}

func TestNewControlRefusesConfig(t *testing.T) {
	cfg := validConfig()
	cfg.BurstRate = 0
	if _, err := NewControl(cfg, NewSender()); err == nil {
		t.Error("NewControl with burst rate 0: nil error")
	}
}

func TestConfigEndpoint(t *testing.T) {
	c := newTestControl(t, validConfig())
	got := decode[wireConfig](t, mustCall(t, c.Handler(), "GET", "/config", http.StatusOK))
	want := wireConfig{
		BootID: c.bootID, Rate: 2, WindowS: 300, BurstRate: 8, BurstSends: 240,
		WindowSends: 600, WindowPairs: windowPairs(600), BurstPairs: 112, RingCapacity: 840,
	}
	if got != want {
		t.Errorf("GET /config = %+v, want %+v", got, want)
	}
}

func TestPutTarget(t *testing.T) {
	c := newTestControl(t, validConfig())
	h := c.Handler()
	got := decode[wirePutTarget](t, mustCall(t, h, "PUT", "/targets/7", http.StatusCreated))
	if want := (wirePutTarget{c.bootID, "7", 0}); got != want {
		t.Errorf("first PUT = %+v, want %+v", got, want)
	}
	tg := c.target("7")
	if tg == nil || tg.ID != "7" || tg.URL != "http://hasher.ek8s-v7.svc.cluster.local:8080" || tg.Checker == nil || tg.Ring == nil {
		t.Fatalf("target 7 = %+v", tg)
	}
	ring, checker := tg.Ring, tg.Checker
	ring.Add(Record{})
	ring.Add(Record{})
	got = decode[wirePutTarget](t, mustCall(t, h, "PUT", "/targets/7", http.StatusOK))
	if want := (wirePutTarget{c.bootID, "7", 2}); got != want {
		t.Errorf("second PUT = %+v, want %+v", got, want)
	}
	if tg := c.target("7"); tg.Ring != ring || tg.Checker != checker {
		t.Error("second PUT replaced the target's Ring or Checker")
	}

	for _, id := range []string{"07", "-1", "1e3", "a", "v1", "1234567890123456789", "1%2F2", "1%3F", "%20"} {
		errorBody(t, call(h, "PUT", "/targets/"+id), http.StatusBadRequest)
	}
	if n := len(c.targetList()); n != 1 {
		t.Errorf("%d targets after the bad PUTs, want 1", n)
	}
}

func TestDeleteTarget(t *testing.T) {
	c := newTestControl(t, validConfig())
	h := c.Handler()
	errorBody(t, call(h, "DELETE", "/targets/3?through=0"), http.StatusNotFound)
	mustCall(t, h, "PUT", "/targets/3", http.StatusCreated)

	for _, q := range []string{"", "?through=", "?through=x", "?through=-1", "?through=+1", "?through=1.0", "?through=18446744073709551616", "?thru=1"} {
		errorBody(t, call(h, "DELETE", "/targets/3"+q), http.StatusBadRequest)
	}

	ring := c.target("3").Ring
	ring.Add(Record{}) // Seq 0
	ring.Add(Record{}) // Seq 1
	if e := errorBody(t, call(h, "DELETE", "/targets/3?through=0"), http.StatusConflict); !strings.Contains(e, "unread") {
		t.Errorf("DELETE with Seq 1 unread: error %q, want it to say unread", e)
	}

	c.setBusy("3", true)
	if e := errorBody(t, call(h, "DELETE", "/targets/3?through=1"), http.StatusConflict); !strings.Contains(e, "in a run") {
		t.Errorf("DELETE of a busy target: error %q, want it to say in a run", e)
	}
	c.setBusy("3", false)

	got := decode[wireBootID](t, mustCall(t, h, "DELETE", "/targets/3?through=1", http.StatusOK))
	if got.BootID != c.bootID {
		t.Errorf("DELETE body boot_id %q, want %q", got.BootID, c.bootID)
	}
	errorBody(t, call(h, "GET", "/targets/3/records?since=0"), http.StatusNotFound)
	errorBody(t, call(h, "DELETE", "/targets/3?through=1"), http.StatusNotFound)
	if n := len(c.targetList()); n != 0 {
		t.Errorf("%d targets after DELETE, want 0", n)
	}
	got2 := decode[wirePutTarget](t, mustCall(t, h, "PUT", "/targets/3", http.StatusCreated))
	if got2.Next != 0 || c.target("3").Ring == ring {
		t.Errorf("PUT after DELETE: next %d, same Ring %v; want a new, empty Ring", got2.Next, c.target("3").Ring == ring)
	}

	// An empty Ring has nothing unread at any through, the largest included.
	mustCall(t, h, "DELETE", "/targets/3?through=18446744073709551615", http.StatusOK)
	mustCall(t, h, "PUT", "/targets/3", http.StatusCreated)
	mustCall(t, h, "DELETE", "/targets/3?through=0", http.StatusOK)
}

// readPage GETs one records page and checks its boot_id, gap, next and
// Seqs (from first, consecutive, n of them).
func readPage(t *testing.T, c *Control, query string, first uint64, n int, gap bool, next uint64) {
	t.Helper()
	page := decode[wireRecords](t, mustCall(t, c.Handler(), "GET", "/targets/1/records?"+query, http.StatusOK))
	if page.BootID != c.bootID || page.Gap != gap || page.Next != next || len(page.Records) != n {
		t.Fatalf("%s: boot_id %q, gap %v, next %d, %d records; want %q, %v, %d, %d",
			query, page.BootID, page.Gap, page.Next, len(page.Records), c.bootID, gap, next, n)
	}
	for i, r := range page.Records {
		if r.Seq != first+uint64(i) || r.Index != int(first)+i {
			t.Fatalf("%s: record %d has Seq %d, Index %d; want %d", query, i, r.Seq, r.Index, first+uint64(i))
		}
	}
}

func TestRecordsPaging(t *testing.T) {
	cfg := validConfig()
	cfg.Rate = 10 // ring of 3,000 + 240
	c := newTestControl(t, cfg)
	h := c.Handler()
	errorBody(t, call(h, "GET", "/targets/1/records?since=0"), http.StatusNotFound)
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)

	readPage(t, c, "since=0", 0, 0, false, 0) // empty
	ring := c.target("1").Ring
	for i := range 1500 {
		ring.Add(Record{Index: i})
	}
	readPage(t, c, "since=0", 0, 1000, false, 1000) // default limit
	readPage(t, c, "since=0&limit=1000", 0, 1000, false, 1000)
	readPage(t, c, "since=1000", 1000, 500, false, 1500)
	readPage(t, c, "since=1497&limit=2", 1497, 2, false, 1499)
	readPage(t, c, "since=1499&limit=1", 1499, 1, false, 1500)
	readPage(t, c, "since=1500", 0, 0, false, 1500)
	readPage(t, c, "since=9999", 0, 0, false, 1500) // past the newest: next is the Ring's

	for _, q := range []string{"", "since=", "since=x", "since=-1", "since=18446744073709551616",
		"since=0&limit=1001", "since=0&limit=0", "since=0&limit=-1", "since=0&limit=", "since=0&limit=x", "since=0&limit=1e3"} {
		errorBody(t, call(h, "GET", "/targets/1/records?"+q), http.StatusBadRequest)
	}
}

func TestRecordsGap(t *testing.T) {
	cfg := validConfig()
	cfg.Rate, cfg.Window = 1, time.Second // ring of 1 + 240
	c := newTestControl(t, cfg)
	mustCall(t, c.Handler(), "PUT", "/targets/1", http.StatusCreated)
	ring := c.target("1").Ring
	for i := range 300 {
		ring.Add(Record{Index: i})
	}
	readPage(t, c, "since=0", 59, 241, true, 300)
	readPage(t, c, "since=58&limit=10", 59, 10, true, 69)
	readPage(t, c, "since=59&limit=10", 59, 10, false, 69)
	readPage(t, c, "since=300", 0, 0, false, 300)
}

func TestTargetsOrder(t *testing.T) {
	c := newTestControl(t, validConfig())
	h := c.Handler()
	if got := decode[wireTargets](t, mustCall(t, h, "GET", "/targets", http.StatusOK)); got.Targets == nil || len(got.Targets) != 0 {
		t.Errorf("GET /targets with none = %+v, want an empty list", got)
	}
	for _, id := range []string{"10", "9", "100", "0", "2", "19", "1"} {
		mustCall(t, h, "PUT", "/targets/"+id, http.StatusCreated)
	}
	c.target("9").Ring.Add(Record{})
	got := decode[wireTargets](t, mustCall(t, h, "GET", "/targets", http.StatusOK))
	want := []wireTarget{{"0", 0}, {"1", 0}, {"2", 0}, {"9", 1}, {"10", 0}, {"19", 0}, {"100", 0}}
	if got.BootID != c.bootID || fmt.Sprint(got.Targets) != fmt.Sprint(want) {
		t.Errorf("GET /targets = %+v, want boot_id %q and %v", got, c.bootID, want)
	}
}

// TestRecordGolden pins the records wire format: a /hash record with a
// pair and evidence, a negative, a skipped record and a fault record.
func TestRecordGolden(t *testing.T) {
	c := newTestControl(t, validConfig())
	c.bootID = "00112233445566778899aabbccddeeff"
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/4", http.StatusCreated)
	ring := c.target("4").Ring
	wall := time.Date(2026, 10, 18, 3, 4, 5, 123456789, time.FixedZone("x", 3600))
	ring.Add(Record{
		Target: "4", RunID: "w1", Index: 7, Kind: KindHash,
		IntendedWall: wall, IntendedMono: 1500 * time.Millisecond, ActualMono: 1501 * time.Millisecond, Latency: 250 * time.Millisecond,
		Verdict:  Verdict{classifier.Failed, classifier.ReasonMalformedBody},
		Pair:     &HashPair{Password: "PW", Hash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$a2V5"},
		Evidence: &Evidence{RequestBody: []byte(`{"password":"PW"}`), Status: 200, Body: []byte("x\x00")},
	})
	ring.Add(Record{
		Target: "4", RunID: "w1", Index: 8, Kind: KindNegative, Class: InvalidJSON,
		IntendedWall: wall.Add(time.Second), IntendedMono: 2500 * time.Millisecond, ActualMono: 2500 * time.Millisecond, Latency: time.Millisecond,
		Verdict: Verdict{Bucket: classifier.Success},
	})
	ring.Add(Record{
		Target: "4", RunID: "w1", Index: 9, Kind: KindVerify, Skipped: true,
		IntendedWall: wall.Add(2 * time.Second), IntendedMono: 3500 * time.Millisecond,
	})
	ring.Add(Record{ // a Verdict set by mistake must still not reach the wire
		Target: "4", RunID: "w1", Index: 10, Kind: KindHash, Fault: "loadgen: building the request: x",
		IntendedWall: wall.Add(3 * time.Second), IntendedMono: 4500 * time.Millisecond,
		Verdict: Verdict{classifier.Failed, classifier.ReasonTransportError},
	})
	rec := mustCall(t, h, "GET", "/targets/4/records?since=0", http.StatusOK)
	const want = `{"boot_id":"00112233445566778899aabbccddeeff","records":[` +
		`{"seq":0,"target":"4","run_id":"w1","index":7,"kind":"hash","intended_wall":"2026-10-18T02:04:05.123456789Z",` +
		`"intended_mono_ns":1500000000,"actual_mono_ns":1501000000,"latency_ns":250000000,"bucket":"failed","reason":"malformed_body",` +
		`"skipped":false,"fault":"","pair":{"password":"PW","hash":"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$a2V5"},` +
		`"evidence":{"request_body":"eyJwYXNzd29yZCI6IlBXIn0=","status":200,"body":"eAA="}},` +
		`{"seq":1,"target":"4","run_id":"w1","index":8,"kind":"negative","class":"invalid_json","intended_wall":"2026-10-18T02:04:06.123456789Z",` +
		`"intended_mono_ns":2500000000,"actual_mono_ns":2500000000,"latency_ns":1000000,"bucket":"success",` +
		`"skipped":false,"fault":""},` +
		`{"seq":2,"target":"4","run_id":"w1","index":9,"kind":"verify","intended_wall":"2026-10-18T02:04:07.123456789Z",` +
		`"intended_mono_ns":3500000000,"actual_mono_ns":0,"latency_ns":0,"skipped":true,"fault":""},` +
		`{"seq":3,"target":"4","run_id":"w1","index":10,"kind":"hash","intended_wall":"2026-10-18T02:04:08.123456789Z",` +
		`"intended_mono_ns":4500000000,"actual_mono_ns":0,"latency_ns":0,"skipped":false,"fault":"loadgen: building the request: x"}` +
		`],"gap":false,"next":4}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("records body:\n got %s\nwant %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
}

func TestBootID(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := validConfig()
	cfg.CanaryAddr = l.Addr().String()
	l.Close() // the canary answers refused at once
	c := newTestControl(t, cfg)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(c.bootID) {
		t.Errorf("boot_id %q, want 32 lowercase hex digits", c.bootID)
	}
	if c2 := newTestControl(t, cfg); c2.bootID == c.bootID {
		t.Errorf("two Controls share boot_id %q", c.bootID)
	}
	h := c.Handler()
	for _, tc := range []struct {
		method, target string
		status         int
		json           bool
	}{
		{"GET", "/config", http.StatusOK, true},
		{"GET", "/targets", http.StatusOK, true},
		{"PUT", "/targets/5", http.StatusCreated, true},
		{"PUT", "/targets/5", http.StatusOK, true},
		{"PUT", "/targets/05", http.StatusBadRequest, true},
		{"GET", "/targets/5/records?since=0", http.StatusOK, true},
		{"GET", "/targets/5/records", http.StatusBadRequest, true},
		{"GET", "/targets/6/records?since=0", http.StatusNotFound, true},
		{"POST", "/canary", http.StatusOK, true},
		{"DELETE", "/targets/5", http.StatusBadRequest, true},
		{"DELETE", "/targets/5?through=0", http.StatusOK, true},
		{"DELETE", "/targets/5?through=0", http.StatusNotFound, true},
		{"GET", "/nope", http.StatusNotFound, false},            // the mux's 404
		{"POST", "/config", http.StatusMethodNotAllowed, false}, // the mux's 405
		{"GET", "/targets/5", http.StatusMethodNotAllowed, false},
	} {
		rec := call(h, tc.method, tc.target)
		name := tc.method + " " + tc.target
		if rec.Code != tc.status {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, tc.status)
		}
		if got := rec.Header().Get(bootIDHeader); got != c.bootID {
			t.Errorf("%s: %s header %q, want %q", name, bootIDHeader, got, c.bootID)
		}
		if tc.json {
			if got := decode[wireBootID](t, rec).BootID; got != c.bootID {
				t.Errorf("%s: body boot_id %q, want %q", name, got, c.bootID)
			}
		}
	}
	// The table's DELETE removed the one target its PUT made.
	c.mu.Lock()
	n := len(c.targets)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("%d targets left, want 0", n)
	}
}

// TestDeleteRacesAdd runs DELETE and records reads against a busy
// target's Ring while a run's stand-in adds to it; run it with -race.
func TestDeleteRacesAdd(t *testing.T) {
	c := newTestControl(t, validConfig())
	h := c.Handler()
	mustCall(t, h, "PUT", "/targets/1", http.StatusCreated)
	c.setBusy("1", true)
	ring := c.target("1").Ring
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				ring.Add(Record{Target: "1"})
			}
		}
	})
	for ring.Next() < 2 { // so that next-2 below is a Seq
		time.Sleep(time.Millisecond)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 50 {
				if rec := call(h, "GET", "/targets/1/records?since=0&limit=10"); rec.Code != http.StatusOK {
					t.Errorf("GET records = %d %s", rec.Code, rec.Body)
				}
				// through is the largest Seq, so only busy can refuse it.
				rec := call(h, "DELETE", fmt.Sprintf("/targets/1?through=%d", uint64(math.MaxUint64)))
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in a run") {
					t.Errorf("DELETE of a busy target = %d %s, want 409 in a run", rec.Code, rec.Body)
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	wg.Wait()
	c.setBusy("1", false)
	next := ring.Next()
	errorBody(t, call(h, "DELETE", fmt.Sprintf("/targets/1?through=%d", next-2)), http.StatusConflict)
	mustCall(t, h, "DELETE", fmt.Sprintf("/targets/1?through=%d", next-1), http.StatusOK)
	if c.target("1") != nil {
		t.Error("target 1 still present after DELETE")
	}
}
