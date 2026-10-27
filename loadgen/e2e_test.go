package loadgen_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/wrapper"
	"github.com/codeinthecreek/eK8s/loadgen"
)

// These tests drive the control API through its Handler, as the
// orchestrator would, against fake variants built from the gen-0
// wrapper's handlers (the ones cmd/service mounts) around a Hasher. They
// decode the wire format with their own types, not loadgen's.

type e2eConfig struct {
	BootID      string `json:"boot_id"`
	WindowSends int    `json:"window_sends"`
	WindowPairs int    `json:"window_pairs"`
	BurstSends  int    `json:"burst_sends"`
	BurstPairs  int    `json:"burst_pairs"`
}

type e2eTargets struct {
	BootID  string `json:"boot_id"`
	Targets []struct {
		ID   string `json:"id"`
		Next uint64 `json:"next"`
	} `json:"targets"`
}

type e2ePair struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
}

type e2eRecord struct {
	Seq      uint64   `json:"seq"`
	Target   string   `json:"target"`
	RunID    string   `json:"run_id"`
	Index    int      `json:"index"`
	Kind     string   `json:"kind"`
	Bucket   string   `json:"bucket"`
	Reason   string   `json:"reason"`
	Skipped  bool     `json:"skipped"`
	Fault    string   `json:"fault"`
	Pair     *e2ePair `json:"pair"`
	Evidence *struct {
		Path        string `json:"path"`
		RequestBody []byte `json:"request_body"`
		Status      int    `json:"status"`
		Body        []byte `json:"body"`
	} `json:"evidence"`
}

type e2ePage struct {
	BootID  string      `json:"boot_id"`
	Records []e2eRecord `json:"records"`
	Gap     bool        `json:"gap"`
	Next    uint64      `json:"next"`
}

type e2eRun struct {
	BootID  string `json:"boot_id"`
	RunID   string `json:"run_id"`
	Kind    string `json:"kind"`
	Targets map[string]struct {
		State string `json:"state"`
		Error string `json:"error"`
	} `json:"targets"`
}

type e2eUploadPair struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
	Match    bool   `json:"match"`
}

// api is one Control's API, for one test.
type api struct {
	t      *testing.T
	h      http.Handler
	bootID string
}

func newAPI(t *testing.T, cfg loadgen.Config) *api {
	t.Helper()
	c, err := loadgen.NewControl(t.Context(), cfg, loadgen.NewSender())
	if err != nil {
		t.Fatalf("NewControl: %v", err)
	}
	return &api{t: t, h: c.Handler(), bootID: c.BootID()}
}

// do sends one request, with body as JSON unless it is nil, and returns
// the response body, failing the test unless the status is want and the
// response carries a's boot_id.
func (a *api) do(method, target string, body any, want int) []byte {
	a.t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			a.t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, httptest.NewRequest(method, target, bytes.NewReader(b)))
	if rec.Code != want {
		a.t.Fatalf("%s %s = %d %s, want %d", method, target, rec.Code, rec.Body, want)
	}
	if got := rec.Header().Get("Ek8s-Boot-Id"); got != a.bootID {
		a.t.Fatalf("%s %s: boot_id %q, want %q", method, target, got, a.bootID)
	}
	return rec.Body.Bytes()
}

func decodeE2E[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return v
}

func (a *api) config() e2eConfig {
	a.t.Helper()
	return decodeE2E[e2eConfig](a.t, a.do("GET", "/config", nil, http.StatusOK))
}

func (a *api) upload(id, runID string, pairs []e2eUploadPair) {
	a.t.Helper()
	a.do("PUT", "/targets/"+id+"/pairs/"+runID, map[string]any{"pairs": pairs}, http.StatusCreated)
}

// run starts run runID of kind on ids and polls it until no target is
// running, then checks that every target is done.
func (a *api) run(runID, kind string, ids ...string) {
	a.t.Helper()
	a.do("POST", "/runs/"+runID+"/start", map[string]any{"kind": kind, "targets": ids}, http.StatusOK)
	deadline := time.Now().Add(30 * time.Second)
	for {
		v := decodeE2E[e2eRun](a.t, a.do("GET", "/runs/"+runID, nil, http.StatusOK))
		running := false
		for _, s := range v.Targets {
			running = running || s.State == "running"
		}
		if !running {
			for _, id := range ids {
				if s := v.Targets[id]; s.State != "done" {
					a.t.Fatalf("run %s target %s = %+v, want done", runID, id, s)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("run %s still running after 30 s", runID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// records reads all of target id's records in pages of at most limit,
// checking that the pages are contiguous and carry no gap.
func (a *api) records(id string, limit int) []e2eRecord {
	a.t.Helper()
	var all []e2eRecord
	for since := uint64(0); ; {
		p := decodeE2E[e2ePage](a.t, a.do("GET", fmt.Sprintf("/targets/%s/records?since=%d&limit=%d", id, since, limit), nil, http.StatusOK))
		if p.Gap || p.BootID != a.bootID || len(p.Records) > limit {
			a.t.Fatalf("target %s page since %d: gap %v, boot_id %q, %d records; want no gap, %q, at most %d",
				id, since, p.Gap, p.BootID, len(p.Records), a.bootID, limit)
		}
		for i, r := range p.Records {
			if r.Seq != since+uint64(i) {
				a.t.Fatalf("target %s page since %d: record %d has seq %d", id, since, i, r.Seq)
			}
		}
		if len(p.Records) == 0 {
			if p.Next != since {
				a.t.Fatalf("target %s: empty page since %d has next %d", id, since, p.Next)
			}
			return all
		}
		all = append(all, p.Records...)
		since = p.Next
	}
}

// next returns target id's next, from GET /targets.
func (a *api) next(id string) uint64 {
	a.t.Helper()
	for _, tg := range decodeE2E[e2eTargets](a.t, a.do("GET", "/targets", nil, http.StatusOK)).Targets {
		if tg.ID == id {
			return tg.Next
		}
	}
	a.t.Fatalf("GET /targets does not list %s", id)
	return 0
}

// parallel runs f(0) to f(n-1) on GOMAXPROCS goroutines: Argon2 is slow
// under the race detector.
func parallel(n int, f func(i int)) {
	work := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for i := range work {
				f(i)
			}
		})
	}
	for i := range n {
		work <- i
	}
	close(work)
	wg.Wait()
}

// hashPairs returns n /verify pairs made with core, as the host makes
// them: the even ones match, and each odd one pairs a password with the
// hash of another.
func hashPairs(t *testing.T, core *hashcore.Core, n int) []e2eUploadPair {
	t.Helper()
	ps := make([]e2eUploadPair, n)
	parallel(n, func(i int) {
		pw, hashed := rand.Text(), rand.Text()
		if i%2 == 0 {
			hashed = pw
		}
		h, err := core.Hash(hashed)
		if err != nil {
			t.Error(err)
		}
		ps[i] = e2eUploadPair{pw, h, i%2 == 0}
	})
	return ps
}

// serveVariants serves a fake variant for each id in hashers at
// url/{id}: a gen-0 wrapper.Server around its Hasher, at /hash and
// /verify. It returns url.
func serveVariants(t *testing.T, hashers map[string]wrapper.Hasher) string {
	t.Helper()
	mux := http.NewServeMux()
	for id, h := range hashers {
		srv := wrapper.New(h, wrapper.Env{})
		vm := http.NewServeMux()
		vm.Handle("POST /hash", srv.HashHandler())
		vm.Handle("POST /verify", srv.VerifyHandler())
		mux.Handle("/"+id+"/", http.StripPrefix("/"+id, vm))
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// e2eConfigFor returns a Config sending to url/{id}: a window of
// rate x window sends, and a burst of 20 at 20/s.
func e2eConfigFor(url string, rate float64, window time.Duration) loadgen.Config {
	cfg := loadgen.Config{Rate: rate, Window: window, BurstRate: 20, TargetURL: url + "/{id}", CanaryAddr: "127.0.0.1:1"}
	return loadgen.WithBurstDuration(cfg, time.Second)
}

// checkRecords checks one run's records for target id: n of them, each
// index once, under runID; no skip, fault, integrity reason or Evidence;
// a pair on every successful /hash and on no other kind, each of which
// core verifies against its password. It returns the records per kind.
func checkRecords(t *testing.T, core *hashcore.Core, recs []e2eRecord, id, runID string, n int) map[string]int {
	t.Helper()
	if len(recs) != n {
		t.Fatalf("target %s: %d records, want %d", id, len(recs), n)
	}
	kinds := make(map[string]int)
	seen := make(map[int]bool)
	var pairs []e2ePair
	for _, r := range recs {
		kinds[r.Kind]++
		switch {
		case r.Target != id || r.RunID != runID:
			t.Errorf("target %s record %d: target %q, run_id %q; want %q, %q", id, r.Seq, r.Target, r.RunID, id, runID)
		case seen[r.Index] || r.Index < 0 || r.Index >= n:
			t.Errorf("target %s record %d: index %d repeated or out of range", id, r.Seq, r.Index)
		case r.Skipped || r.Fault != "":
			t.Errorf("target %s record %d: skipped %v, fault %q", id, r.Seq, r.Skipped, r.Fault)
		case classifier.Integrity(classifier.Reason(r.Reason)) || r.Evidence != nil:
			t.Errorf("target %s record %d: reason %s, evidence %v", id, r.Seq, r.Reason, r.Evidence != nil)
		case r.Kind == "hash" && r.Bucket == "success" && r.Pair == nil:
			t.Errorf("target %s record %d: a successful /hash with no pair", id, r.Seq)
		case r.Kind != "hash" && r.Pair != nil:
			t.Errorf("target %s record %d: a %s with a pair", id, r.Seq, r.Kind)
		}
		seen[r.Index] = true
		if r.Pair != nil {
			pairs = append(pairs, *r.Pair)
		}
	}
	if len(pairs) == 0 {
		t.Fatalf("target %s: no /hash record carries a pair", id)
	}
	parallel(len(pairs), func(i int) {
		if ok, err := core.Verify(pairs[i].Password, pairs[i].Hash); !ok || err != nil {
			t.Errorf("target %s: pair %+v does not verify: %v, %v", id, pairs[i], ok, err)
		}
	})
	t.Logf("target %s run %s: %d records %v, %d pairs verified", id, runID, n, kinds, len(pairs))
	return kinds
}

// TestE2EWindowAndBurst scripts one window to two targets and one burst
// to a third, against gen-0 variants with the real hashcore, and checks
// every record, then the DELETE handshake.
func TestE2EWindowAndBurst(t *testing.T) {
	core := hashcore.New()
	url := serveVariants(t, map[string]wrapper.Hasher{"1": core, "2": core, "3": core})
	a := newAPI(t, e2eConfigFor(url, 5, 4*time.Second))
	cfg := a.config()
	if cfg.BootID != a.bootID || cfg.WindowSends != 20 || cfg.BurstSends != 20 {
		t.Fatalf("GET /config = %+v, want boot_id %s, window_sends 20, burst_sends 20", cfg, a.bootID)
	}

	ids := []string{"1", "2"}
	for _, id := range ids {
		a.do("PUT", "/targets/"+id, nil, http.StatusCreated)
		a.upload(id, "w1", hashPairs(t, core, cfg.WindowPairs))
	}
	a.run("w1", "window", ids...)
	for _, id := range ids {
		checkRecords(t, core, a.records(id, 7), id, "w1", cfg.WindowSends)
		next := a.next(id)
		if next != uint64(cfg.WindowSends) {
			t.Fatalf("target %s next = %d, want %d", id, next, cfg.WindowSends)
		}
		a.do("DELETE", fmt.Sprintf("/targets/%s?through=%d", id, next-2), nil, http.StatusConflict)
		a.do("DELETE", fmt.Sprintf("/targets/%s?through=%d", id, next-1), nil, http.StatusOK)
	}

	a.do("PUT", "/targets/3", nil, http.StatusCreated)
	a.upload("3", "b1", hashPairs(t, core, cfg.BurstPairs))
	a.run("b1", "burst", "3")
	kinds := checkRecords(t, core, a.records("3", 7), "3", "b1", cfg.BurstSends)
	hashes, verifies, negatives, err := loadgen.BurstCounts(cfg.BurstSends)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"hash": hashes, "verify": verifies, "negative": negatives}; fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("burst kinds = %v, want %v", kinds, want)
	}
}

// replayHasher is a variant's Hasher that answers every Hash with one
// valid hash, so every salt after the first is reused. Verify answers
// from pairs, the pairs it will be sent, and otherwise from hashcore,
// which refuses a negative's hash without running Argon2.
type replayHasher struct {
	hash  string
	pairs map[e2ePair]bool
	core  *hashcore.Core
}

func (h replayHasher) Hash(string) (string, error) { return h.hash, nil }

func (h replayHasher) Verify(password, encoded string) (bool, error) {
	if match, ok := h.pairs[e2ePair{password, encoded}]; ok {
		return match, nil
	}
	return h.core.Verify(password, encoded)
}

// TestE2ESaltReused checks the integrity path end to end: against a
// variant that answers every /hash with one hash, every /hash record
// after the first carries salt_reused, and only the first 8 carry
// Evidence.
func TestE2ESaltReused(t *testing.T) {
	core := hashcore.New()
	replayed, err := core.Hash("replayed")
	if err != nil {
		t.Fatal(err)
	}
	// 60 sends, so that at least 10 /hash records exceed the Evidence cap
	// but for a chance under 2e-8.
	cfg := loadgen.WithMaxRate(e2eConfigFor("http://127.0.0.1:1", 30, 2*time.Second), 30)
	n, err := cfg.WindowSends()
	if err != nil {
		t.Fatal(err)
	}
	h := replayHasher{replayed, make(map[e2ePair]bool), core}
	pairs := make([]e2eUploadPair, n) // at least WindowPairs
	for i := range pairs {
		pairs[i] = e2eUploadPair{rand.Text(), loadgen.RandomExactPHC(), i%2 == 0}
		h.pairs[e2ePair{pairs[i].Password, pairs[i].Hash}] = pairs[i].Match
	}
	cfg.TargetURL = serveVariants(t, map[string]wrapper.Hasher{"1": h}) + "/{id}"
	a := newAPI(t, cfg)
	a.do("PUT", "/targets/1", nil, http.StatusCreated)
	a.upload("1", "w1", pairs)
	a.run("w1", "window", "1")

	recs := a.records("1", 1000)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	var hashes, first, reused, integrity, evidence int
	for _, r := range recs {
		switch {
		case r.Kind != "hash":
		case r.Reason == "salt_reused" && r.Pair == nil:
			reused++
		case r.Reason == "" && r.Bucket == "success" && r.Pair != nil && r.Pair.Hash == replayed:
			first++
		default:
			t.Errorf("/hash record %d: bucket %s, reason %q, pair %+v; want salt_reused, or the first's success", r.Seq, r.Bucket, r.Reason, r.Pair)
		}
		if r.Kind == "hash" {
			hashes++
		}
		if classifier.Integrity(classifier.Reason(r.Reason)) {
			integrity++
		}
		if r.Evidence == nil {
			continue
		}
		evidence++
		if r.Reason != "salt_reused" || r.Evidence.Path != "/hash" || r.Evidence.Status != http.StatusOK || !strings.Contains(string(r.Evidence.Body), replayed) {
			t.Errorf("record %d: reason %s, evidence path %q status %d body %q; want salt_reused, /hash, 200 and the replayed hash",
				r.Seq, r.Reason, r.Evidence.Path, r.Evidence.Status, r.Evidence.Body)
		}
	}
	if hashes < 10 {
		t.Fatalf("%d /hash records, want at least 10 to pass the Evidence cap", hashes)
	}
	if first != 1 || reused != hashes-1 || integrity != reused || evidence != 8 {
		t.Errorf("of %d /hash records, %d first and %d salt_reused; %d integrity records, %d with evidence; want 1, %d; %d, 8",
			hashes, first, reused, integrity, evidence, hashes-1, hashes-1)
	}
	t.Logf("%d records: %d /hash, %d salt_reused, %d with evidence", n, hashes, reused, evidence)
}

// TestE2ERestart checks that a new Control, as after a restart, has a
// new boot_id and none of the old one's targets.
func TestE2ERestart(t *testing.T) {
	cfg := e2eConfigFor("http://127.0.0.1:1", 5, 4*time.Second)
	a := newAPI(t, cfg)
	a.do("PUT", "/targets/1", nil, http.StatusCreated)
	b := newAPI(t, cfg)
	if a.bootID == b.bootID {
		t.Fatalf("two Controls share boot_id %s", a.bootID)
	}
	if got := b.config().BootID; got != b.bootID {
		t.Errorf("GET /config boot_id = %s, want %s", got, b.bootID)
	}
	if ts := decodeE2E[e2eTargets](t, b.do("GET", "/targets", nil, http.StatusOK)); len(ts.Targets) != 0 {
		t.Errorf("the new Control's targets = %+v, want none", ts.Targets)
	}
	b.do("GET", "/targets/1/records?since=0", nil, http.StatusNotFound)
	b.do("DELETE", "/targets/1?through=0", nil, http.StatusNotFound)
	a.do("GET", "/targets/1/records?since=0", nil, http.StatusOK)
}
