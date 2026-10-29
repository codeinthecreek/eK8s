package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/wrapper"
	"github.com/codeinthecreek/eK8s/loadgen"
	"github.com/codeinthecreek/eK8s/loadgen/client"
)

// These tests run windows through a real loadgen Control, in process,
// against fake variants: the gen-0 wrapper around the fixed core, or a
// cheat Hasher behind noShed.

// testShape is a window of 20 sends at 10/s; loadgen's burst is unused.
var testShape = Shape{Rate: 10, WindowS: 2, BurstRate: 4}

// reuseHasher answers every /hash with its first hash: the second is
// salt_reused in loadgen, confirmed on the host against the first's
// recorded pair.
type reuseHasher struct {
	*hashcore.Core
	once  sync.Once
	first string
}

func (h *reuseHasher) Hash(pw string) (string, error) {
	var err error
	h.once.Do(func() { h.first, err = h.Core.Hash(pw) })
	return h.first, err
}

// replayHasher answers every /hash with a fixed hash, one the host has
// already seen (a burst hash).
type replayHasher struct {
	*hashcore.Core
	hash string
}

func (h replayHasher) Hash(string) (string, error) { return h.hash, nil }

// fakeHasher answers /hash with a canonical PHC string that is no hash
// of anything: loadgen's checks pass, full verification fails.
type fakeHasher struct{ *hashcore.Core }

func (fakeHasher) Hash(string) (string, error) {
	b64 := base64.RawStdEncoding.EncodeToString
	return "$argon2id$v=19$m=19456,t=2,p=1$" + b64(randBytes(16)) + "$" + b64(randBytes(32)), nil
}

// bothHasher fakes every /hash and answers every /verify wrongly.
type bothHasher struct{ fakeHasher }

func (h bothHasher) Verify(pw, enc string) (bool, error) { return invertHasher{h.Core}.Verify(pw, enc) }

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// invertHasher answers every /verify wrongly.
type invertHasher struct{ *hashcore.Core }

func (h invertHasher) Verify(pw, enc string) (bool, error) {
	ok, err := h.Core.Verify(pw, enc)
	return !ok, err
}

// gen0 is the gen-0 wrapper's handlers around h, as cmd/service mounts
// them.
func gen0(h wrapper.Hasher) http.Handler {
	srv := wrapper.New(h, wrapper.Env{})
	mux := http.NewServeMux()
	mux.Handle("POST /hash", srv.HashHandler())
	mux.Handle("POST /verify", srv.VerifyHandler())
	return mux
}

// noShed serves the HTTP contract around h with no concurrency limit,
// so a cheat answers every request even when the race detector slows
// Argon2 (the gen-0 wrapper would shed most of them). It decodes
// strictly, so a contract negative still gets a 400.
func noShed(h wrapper.Hasher) http.Handler {
	decode := func(r *http.Request, v any) bool {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		return dec.Decode(v) == nil && dec.Decode(&struct{}{}) == io.EOF
	}
	valid := func(pw *string) bool { return pw != nil && *pw != "" && len(*pw) <= maxPasswordBytes }
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hash", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password *string `json:"password"`
		}
		if !decode(r, &req) || !valid(req.Password) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hash, _ := h.Hash(*req.Password)
		reply(w, map[string]string{"hash": hash})
	})
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password *string `json:"password"`
			Hash     *string `json:"hash"`
		}
		if !decode(r, &req) || !valid(req.Password) || req.Hash == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ok, err := h.Verify(*req.Password, *req.Hash)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reply(w, map[string]bool{"match": ok})
	})
	return mux
}

// serveVariants serves a fake variant per id at url/v{id}.
func serveVariants(t *testing.T, variants map[string]http.Handler) string {
	t.Helper()
	mux := http.NewServeMux()
	for id, h := range variants {
		mux.Handle("/v"+id+"/", http.StripPrefix("/v"+id, h))
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// fakeCluster is a Cluster with no cluster: each PodCPU call reads one
// CPU-second more per pod than the last, and gone drops pods. ops logs
// the stress, kindnet and CPU calls in order, each stress call with the
// count reads gives at the time (records reads, in the fixture).
type fakeCluster struct {
	mu        sync.Mutex
	pods      []string // variant ids with one pod each
	notReady  bool
	notReadyN int // with notReady unset: the n-th KindnetReady call (from 1) reads not Ready
	kindnetN  int
	cpuCalls  int
	gone      map[string]bool // ids whose pod is gone from the second sample on
	stressOff int
	stressOn  int
	onStress  func(on bool) // called on each StressOn and StressOff, if set
	reads     func() int
	ops       []string
	deleted   []string
}

func (c *fakeCluster) stress(on bool) {
	c.mu.Lock()
	name := "stress off"
	if on {
		c.stressOn++
		name = "stress on"
	} else {
		c.stressOff++
	}
	if c.reads != nil {
		name += fmt.Sprintf("@%d", c.reads())
	}
	c.ops = append(c.ops, name)
	hook := c.onStress
	c.mu.Unlock()
	if hook != nil {
		hook(on)
	}
}

func (c *fakeCluster) StressOff(context.Context) error { c.stress(false); return nil }
func (c *fakeCluster) StressOn(context.Context) error  { c.stress(true); return nil }
func (c *fakeCluster) KindnetReady(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kindnetN++
	c.ops = append(c.ops, "kindnet")
	return !c.notReady && c.kindnetN != c.notReadyN, nil
}
func (c *fakeCluster) PodCPU(context.Context) (fitness.Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cpuCalls++
	c.ops = append(c.ops, "cpu")
	s := make(fitness.Sample)
	for _, id := range c.pods {
		if c.cpuCalls > 1 && c.gone[id] {
			continue
		}
		s[fitness.Pod{Namespace: "ek8s-v" + id, Name: "hasher-" + id}] = float64(c.cpuCalls)
	}
	return s, nil
}
func (c *fakeCluster) DeleteNamespace(_ context.Context, ns string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, ns)
	return nil
}

// swapHandler serves whichever loadgen Control it holds, so a test can
// restart loadgen behind the same URL.
type swapHandler struct{ h atomic.Pointer[http.Handler] }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// fixture is a loadgen in process, fake variants and a Session over
// them, with the given variants live and adopted.
type fixture struct {
	t       *testing.T
	cfg     loadgen.Config
	lg      *swapHandler
	cluster *fakeCluster
	s       *Session
	cl      *client.Client
	live    []string
	srvURL  string
	rec     *recorder
}

// recorder is an http.RoundTripper that records each request's method,
// path and query, and calls hook, if set, after each response.
type recorder struct {
	rt   http.RoundTripper
	mu   sync.Mutex
	reqs []string
	hook func(req *http.Request)
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.rt.RoundTrip(req)
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.RequestURI())
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	return resp, err
}

// sent returns the recorded requests that start with prefix.
func (r *recorder) sent(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.reqs {
		if strings.HasPrefix(q, prefix) {
			out = append(out, q)
		}
	}
	return out
}

func (r *recorder) setHook(f func(req *http.Request)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = f
}

func newFixture(t *testing.T, variants map[string]http.Handler, live ...string) *fixture {
	t.Helper()
	url := serveVariants(t, variants)
	fx := &fixture{t: t, lg: &swapHandler{}, cluster: &fakeCluster{pods: live}, live: live}
	fx.cfg = loadgen.Config{
		Rate: testShape.Rate, Window: time.Duration(testShape.WindowS * float64(time.Second)), BurstRate: testShape.BurstRate,
		TargetURL: url + "/v{id}", CanaryAddr: refusingAddr(t),
	}
	fx.restartLoadgen()
	srv := httptest.NewServer(fx.lg)
	t.Cleanup(srv.Close)
	fx.srvURL, fx.rec = srv.URL, &recorder{rt: srv.Client().Transport}
	fx.cl = client.New(srv.URL, &http.Client{Transport: fx.rec})
	fx.cluster.reads = func() int { return len(fx.rec.sent("GET /targets/")) }
	for _, id := range live {
		if _, _, err := fx.cl.PutTarget(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewSession(t.Context(), SessionConfig{
		Client: fx.cl, Cluster: fx.cluster, Core: hashcore.New(), Want: testShape, Params: fitness.DefaultParams(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// The race detector and Argon2 in the same process slow loadgen's
	// scheduler past DESIGN.md's lag thresholds; they are tested apart.
	s.t.lagP99, s.t.lagMax = time.Hour, time.Hour
	s.t.pollEvery, s.t.canaryEvery = 20*time.Millisecond, 10*time.Millisecond
	for _, id := range live {
		if err := s.Adopt(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	fx.s = s
	return fx
}

// restartLoadgen puts a new loadgen Control, with a new boot_id and no
// targets, behind the fixture's URL.
func (fx *fixture) restartLoadgen() {
	fx.t.Helper()
	c, err := loadgen.NewControl(fx.t.Context(), fx.cfg, loadgen.NewSender())
	if err != nil {
		fx.t.Fatalf("NewControl: %v", err)
	}
	h := c.Handler()
	fx.lg.h.Store(&h)
}

// refusingAddr is a loopback address nothing listens on, so the canary
// reads refused.
func refusingAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func (fx *fixture) window() WindowResult {
	fx.t.Helper()
	res, err := fx.s.RunWindow(fx.t.Context(), fx.live, fx.batches())
	if err != nil {
		fx.t.Fatalf("RunWindow: %v", err)
	}
	return res
}

// baseline runs RunBaseline, the baseline half alone: half the hashes
// of a window, for tests of what both halves share.
func (fx *fixture) baseline() WindowResult {
	fx.t.Helper()
	bs, err := GenerateBatches(fx.t.Context(), hashcore.New(), len(fx.live), fx.s.Config().WindowPairs, 0)
	if err != nil {
		fx.t.Fatal(err)
	}
	res, err := fx.s.RunBaseline(fx.t.Context(), fx.live, bs)
	if err != nil {
		fx.t.Fatalf("RunBaseline: %v", err)
	}
	return res
}

// batches makes one batch per half per live variant.
func (fx *fixture) batches() []Batch {
	fx.t.Helper()
	bs, err := GenerateBatches(fx.t.Context(), hashcore.New(), HalvesPerWindow*len(fx.live), fx.s.Config().WindowPairs, 0)
	if err != nil {
		fx.t.Fatal(err)
	}
	return bs
}

func TestWindowScored(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": gen0(core)}, "1", "2")
	res := fx.window()
	if !res.Scored() || len(res.Variants) != 2 {
		t.Fatalf("window excluded %v, %d variants; want scored, 2", res.Excluded, len(res.Variants))
	}
	if res.StressRunID == "" || res.StressRunID == res.RunID {
		t.Errorf("run ids %q, %q; want a stress run of its own", res.RunID, res.StressRunID)
	}
	for id, v := range res.Variants {
		if v.Records != 40 || v.Disqualified != "" || v.Baseline == nil || v.Stress == nil ||
			v.Baseline.CPUUnmeasured != "" || v.Stress.CPUUnmeasured != "" {
			t.Errorf("variant %s: %+v; want 40 records, both halves scored, CPU measured", id, v)
			continue
		}
		for _, h := range []*HalfScore{v.Baseline, v.Stress} {
			if h.Requests+h.Negatives != 20 {
				t.Errorf("variant %s: a half of %d records, want 20", id, h.Requests+h.Negatives)
			}
			// The fake cluster gives each pod one CPU-second per half.
			if got, want := h.CPUPerSuccess, 1/float64(h.Successes); h.Successes > 0 && got != want {
				t.Errorf("variant %s: cpu per success %v, want %v", id, got, want)
			}
		}
		if f, ok := v.Fitness(); !ok || f != min(v.Baseline.Score, v.Stress.Score) {
			t.Errorf("variant %s: fitness %v, %v; want the lower half score", id, f, ok)
		}
	}
	// Baseline with stress off, then stress on for the stress half's run
	// only: off again after its last CPU sample, before any of its
	// records is read.
	want := []string{"stress off@0", "kindnet", "cpu", "cpu", "stress on@4", "kindnet", "cpu", "cpu", "stress off@4"}
	if got := fx.cluster.ops; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cluster calls %q, want %q", got, want)
	}
	// A second window reads only its own records.
	if res2 := fx.window(); !res2.Scored() || res2.Variants["1"].Records != 40 {
		t.Errorf("second window: %+v", res2)
	}
	// The DELETE handshake: every record was read, so loadgen lets go,
	// and Drop names the last of them, not some larger through.
	ts, err := fx.cl.Targets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var last uint64
	for _, tg := range ts.Targets {
		if tg.ID == "2" {
			last = tg.Next - 1
		}
	}
	if err := fx.s.Drop(t.Context(), "2"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if got := fx.rec.sent("DELETE "); len(got) != 1 || got[0] != fmt.Sprintf("DELETE /targets/2?through=%d", last) {
		t.Errorf("DELETE requests %q, want one through=%d", got, last)
	}
	if strings.Join(fx.cluster.deleted, ",") != "ek8s-v2" {
		t.Errorf("deleted namespaces %v, want ek8s-v2", fx.cluster.deleted)
	}
	ts, err = fx.cl.Targets(t.Context())
	if err != nil || len(ts.Targets) != 1 || ts.Targets[0].ID != "1" {
		t.Errorf("targets after Drop: %+v, %v", ts, err)
	}
}

// TestWindowIntegrity checks each cheat is disqualified on the host,
// with the honest variant scored beside it, in a baseline half (the
// stress half is judged the same way: TestWindowStressIntegrity).
func TestWindowIntegrity(t *testing.T) {
	core := hashcore.New()
	burst := mustHash(t, core, "a burst password")
	for _, tc := range []struct {
		name  string
		cheat wrapper.Hasher
		seed  []string
		want  classifier.Reason
	}{
		{"reused salt", &reuseHasher{Core: core}, nil, classifier.ReasonSaltReused},
		{"burst salt replayed", replayHasher{core, burst}, []string{burst}, classifier.ReasonSaltReused},
		{"fake hashes", fakeHasher{core}, nil, classifier.ReasonVerifyMismatch},
		{"inverted verify", invertHasher{core}, nil, classifier.ReasonWrongMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "9": noShed(tc.cheat)}, "1")
			fx.live = append(fx.live, "9")
			fx.cluster.pods = fx.live
			if _, _, err := fx.cl.PutTarget(t.Context(), "9"); err != nil {
				t.Fatal(err)
			}
			if err := fx.s.Adopt("9", tc.seed); err != nil {
				t.Fatal(err)
			}
			res := fx.baseline()
			if !res.Scored() {
				t.Fatalf("window excluded: %v", res.Excluded)
			}
			if v := res.Variants["9"]; v.Disqualified != tc.want || v.DisqualifiedHalf != HalfBaseline || v.Baseline != nil {
				t.Errorf("cheat: disqualified %q in %q at %d, scores %v %v; want %q in the baseline half, no score",
					v.Disqualified, v.DisqualifiedHalf, v.DisqualifiedIndex, v.Baseline, v.Stress, tc.want)
			}
			if v := res.Variants["1"]; v.Disqualified != "" || v.Baseline == nil {
				t.Errorf("honest variant: %+v", v)
			}
		})
	}
}

// stressCheat fakes its /hash answers once on is set (by the stress
// half's StressOn): honest in the baseline half, a cheat under stress.
type stressCheat struct {
	*hashcore.Core
	on *atomic.Bool
}

func (h stressCheat) Hash(pw string) (string, error) {
	if h.on.Load() {
		return fakeHasher{h.Core}.Hash(pw)
	}
	return h.Core.Hash(pw)
}

// TestWindowStressIntegrity checks a cheat that fakes results only under
// stress is caught in the stress half, and that a stress salt repeating
// a baseline one is caught across the halves.
func TestWindowStressIntegrity(t *testing.T) {
	core := hashcore.New()
	var on atomic.Bool
	reuse := &reuseHasher{Core: core}
	cross := stressReuse{reuse, &on}
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "8": noShed(stressCheat{core, &on}), "9": noShed(cross)}, "1", "8", "9")
	fx.cluster.onStress = func(v bool) { on.Store(v) }
	res := fx.window()
	if !res.Scored() {
		t.Fatalf("window excluded: %v", res.Excluded)
	}
	for id, want := range map[string]classifier.Reason{"8": classifier.ReasonVerifyMismatch, "9": classifier.ReasonSaltReused} {
		if v := res.Variants[id]; v.Disqualified != want || v.DisqualifiedHalf != HalfStress || v.Baseline != nil || v.Stress != nil {
			t.Errorf("cheat %s: disqualified %q in %q; want %q in the stress half, no score", id, v.Disqualified, v.DisqualifiedHalf, want)
		}
	}
	if v := res.Variants["1"]; v.Disqualified != "" || v.Baseline == nil || v.Stress == nil {
		t.Errorf("honest variant: %+v", v)
	}
}

// stressReuse hashes honestly in the baseline half and, under stress,
// answers every /hash with the first hash it gave in the baseline half:
// salt_reused only across the halves, which the host's history spans.
type stressReuse struct {
	r  *reuseHasher
	on *atomic.Bool
}

func (h stressReuse) Hash(pw string) (string, error) {
	if h.on.Load() {
		return h.r.first, nil
	}
	hash, err := h.r.Core.Hash(pw)
	h.r.once.Do(func() { h.r.first = hash })
	return hash, err
}

func (h stressReuse) Verify(pw, enc string) (bool, error) { return h.r.Core.Verify(pw, enc) }

// TestWindowExcludedStillJudged checks that an excluded window still
// disqualifies a cheat: exclusion is from scoring only.
func TestWindowExcludedStillJudged(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": noShed(fakeHasher{core})}, "1", "2")
	fx.s.t.lagP99 = time.Nanosecond
	res := fx.window()
	if res.Scored() || !strings.Contains(strings.Join(res.Excluded, ","), ExcludedLagP99) {
		t.Fatalf("excluded %v, want lag-p99", res.Excluded)
	}
	if v := res.Variants["2"]; v.Disqualified != classifier.ReasonVerifyMismatch {
		t.Errorf("cheat in an excluded window: %+v, want verify_mismatch", v)
	}
	if v := res.Variants["1"]; v.Baseline != nil || v.Stress != nil || v.Records != 20 {
		t.Errorf("honest variant in an excluded window: %+v; want 20 records, no score", v)
	}
	// An excluded baseline half: no stress half is run.
	if res.StressRunID != "" || fx.cluster.stressOn != 0 || len(res.StressExcluded) != 0 {
		t.Errorf("stress half after an excluded baseline: run %q, stress on %d times, excluded %v", res.StressRunID, fx.cluster.stressOn, res.StressExcluded)
	}
}

// TestWindowStressExcluded checks a stress half excluded before it
// starts (its kindnet first look): the window is excluded, the baseline
// half's records are still judged, and stress-ng is off again.
func TestWindowStressExcluded(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": noShed(fakeHasher{core})}, "1", "2")
	fx.cluster.notReadyN = 2
	res := fx.window()
	if res.Scored() || strings.Join(res.Excluded, ",") != ExcludedKindnet || strings.Join(res.StressExcluded, ",") != ExcludedKindnet || len(res.BaselineExcluded) != 0 {
		t.Fatalf("excluded %v (baseline %v, stress %v), want kindnet-not-ready in the stress half", res.Excluded, res.BaselineExcluded, res.StressExcluded)
	}
	if v := res.Variants["2"]; v.Disqualified != classifier.ReasonVerifyMismatch || v.DisqualifiedHalf != HalfBaseline {
		t.Errorf("cheat: %+v, want verify_mismatch in the baseline half", v)
	}
	if v := res.Variants["1"]; v.Baseline != nil || v.Records != 20 {
		t.Errorf("honest variant: %+v; want 20 records, no score", v)
	}
	if got := fx.cluster.ops[len(fx.cluster.ops)-1]; fx.cluster.stressOn != 1 || !strings.HasPrefix(got, "stress off") {
		t.Errorf("stress on %d times, last cluster call %q; want once, then off", fx.cluster.stressOn, got)
	}
}

// TestRunBaseline checks C0's window: the baseline half alone, stress-ng
// never on, one batch per variant.
func TestRunBaseline(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
	bs, err := GenerateBatches(t.Context(), core, 1, fx.s.Config().WindowPairs, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := fx.s.RunBaseline(t.Context(), fx.live, bs)
	if err != nil || !res.Scored() {
		t.Fatalf("RunBaseline = %+v, %v", res, err)
	}
	v := res.Variants["1"]
	if f, ok := v.Fitness(); v.Records != 20 || v.Baseline == nil || v.Stress != nil || !ok || f != v.Baseline.Score {
		t.Errorf("variant: %+v; want 20 records, the baseline half alone as its fitness", v)
	}
	if fx.cluster.stressOn != 0 || res.StressRunID != "" || fx.cluster.cpuCalls != 2 {
		t.Errorf("stress on %d times, stress run %q, %d CPU samples; want 0, none, 2", fx.cluster.stressOn, res.StressRunID, fx.cluster.cpuCalls)
	}
}

func TestWindowCPUUnmeasured(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": gen0(core)}, "1", "2")
	fx.cluster.gone = map[string]bool{"2": true}
	res := fx.window()
	if !res.Scored() {
		t.Fatalf("a variant's own pod going excluded the window: %v", res.Excluded)
	}
	v := res.Variants["2"]
	for _, h := range []*HalfScore{v.Baseline, v.Stress} {
		if h == nil || h.CPUUnmeasured == "" || h.CPUScore != 0 {
			t.Errorf("variant 2: %+v; want CPU unmeasured in each half, scored with no CPU credit", h)
			continue
		}
		if h.Successes > 0 && h.CPUPerSuccess != fitness.DefaultParams().CPUCeiling {
			t.Errorf("variant 2: cpu per success %v, want the ceiling", h.CPUPerSuccess)
		}
	}
	if v := res.Variants["1"]; v.Baseline == nil || v.Stress == nil || v.Baseline.CPUUnmeasured != "" || v.Stress.CPUUnmeasured != "" {
		t.Errorf("variant 1: %+v", v)
	}
}

// TestWindowNotStarted checks the first look and the canary exclude a
// window before it starts, and that no records are read for it.
func TestWindowNotStarted(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
	fx.cluster.notReady = true
	if res := fx.window(); strings.Join(res.Excluded, ",") != ExcludedKindnet || len(res.Variants) != 0 {
		t.Errorf("kindnet not Ready: %+v", res)
	}
	fx.cluster.notReady = false

	// A canary that connects: loadgen dials a listener.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fx.cfg.CanaryAddr = l.Addr().String()
	fx.restartLoadgen()
	if res := fx.window(); strings.Join(res.Excluded, ",") != ExcludedRestart {
		t.Fatalf("after a restart: %+v", res)
	}
	if err := fx.s.Recover(t.Context(), fx.live); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// After a new boot_id the canary is polled, at most canaryBound.
	fx.s.t.canaryBound = 100 * time.Millisecond
	if res := fx.window(); strings.Join(res.Excluded, ",") != ExcludedCanary {
		t.Errorf("canary connected: %+v", res)
	}
	if fx.cluster.cpuCalls != 0 {
		t.Errorf("%d CPU samples for windows that never started", fx.cluster.cpuCalls)
	}
}

// TestWindowRestart checks a loadgen restart: the window is excluded,
// the next refused until Recover re-puts the live targets, then scored.
func TestWindowRestart(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
	fx.restartLoadgen()
	if res := fx.window(); strings.Join(res.Excluded, ",") != ExcludedRestart || !fx.s.Restarted() {
		t.Fatalf("after a restart: %+v, restarted %v", res, fx.s.Restarted())
	}
	if _, err := fx.s.RunWindow(t.Context(), fx.live, nil); err == nil {
		t.Error("RunWindow before Recover: want an error")
	}
	if err := fx.s.Recover(t.Context(), fx.live); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res := fx.window(); !res.Scored() || res.Variants["1"].Stress == nil {
		t.Errorf("window after Recover: %+v", res)
	}
}

func TestNewSessionShape(t *testing.T) {
	fx := newFixture(t, map[string]http.Handler{"1": gen0(hashcore.New())}, "1")
	// The shape is compared exactly: the smallest differences count.
	for _, want := range []Shape{{11, 2, 4}, {10, 300, 4}, {10, 2, 40}, {10.000000001, 2, 4}, {10, 1.999999999, 4}, {10, 2, 4.000000001}} {
		_, err := NewSession(t.Context(), SessionConfig{
			Client: fx.cl, Cluster: fx.cluster, Core: hashcore.New(), Want: want, Params: fitness.DefaultParams(),
		})
		if err == nil || !strings.Contains(err.Error(), "want") {
			t.Errorf("NewSession(%+v) = %v, want a shape refusal", want, err)
		}
	}
}

func TestStreak(t *testing.T) {
	var s Streak
	ex, ok := WindowResult{Excluded: []string{ExcludedGap}}, WindowResult{}
	for i, tc := range []struct {
		w    WindowResult
		halt bool
	}{{ex, false}, {ex, false}, {ok, false}, {ex, false}, {ex, false}, {ex, true}} {
		if got := s.Add(tc.w); got != tc.halt {
			t.Errorf("window %d: halt %v, want %v", i, got, tc.halt)
		}
	}
}

// TestWindowUnconfirmed checks that integrity claims the host cannot
// confirm are a fault, never a score: here the host has lost the ground
// truth of the pairs the cheat answers wrongly.
func TestWindowUnconfirmed(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"9": noShed(invertHasher{core})}, "9")
	bs := fx.batches()
	bs[0].truth = map[pairKey]bool{}
	_, err := fx.s.RunWindow(t.Context(), fx.live, bs)
	if !errors.Is(err, errUnconfirmed) {
		t.Errorf("RunWindow = %v, want errUnconfirmed", err)
	}
}

// TestWindowCanaryPoll checks that after a new boot_id the canary is
// polled until it reads refused: it connects at first, then the
// listener closes.
func TestWindowCanaryPoll(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fx.cfg.CanaryAddr = l.Addr().String()
	fx.restartLoadgen()
	if res := fx.window(); strings.Join(res.Excluded, ",") != ExcludedRestart {
		t.Fatalf("after a restart: %+v", res)
	}
	if err := fx.s.Recover(t.Context(), fx.live); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	fx.s.t.canaryBound = 20 * time.Second
	fx.rec.setHook(func(req *http.Request) {
		if req.URL.Path == "/canary" && len(fx.rec.sent("POST /canary")) == 3 {
			l.Close()
		}
	})
	res := fx.baseline()
	if !res.Scored() {
		t.Errorf("window after the canary turned refused: excluded %v", res.Excluded)
	}
	if n := len(fx.rec.sent("POST /canary")); n < 4 {
		t.Errorf("%d canary calls, want at least 4 (3 connected, then refused)", n)
	}
}

// TestWindowAbandon checks the run's bound and a cancelled context: both
// cancel the run in loadgen and drain it, and are faults.
func TestWindowAbandon(t *testing.T) {
	core := hashcore.New()
	t.Run("bound", func(t *testing.T) {
		fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
		fx.s.cfg.WindowS = 0
		fx.s.t.clientTimeout, fx.s.t.runSlack = time.Nanosecond, time.Nanosecond
		if _, err := fx.s.RunWindow(t.Context(), fx.live, fx.batches()); err == nil || !strings.Contains(err.Error(), "past its bound") {
			t.Errorf("RunWindow = %v, want the bound's fault", err)
		}
		checkCancelled(t, fx)
	})
	t.Run("ctx", func(t *testing.T) {
		fx := newFixture(t, map[string]http.Handler{"1": gen0(core)}, "1")
		ctx, cancel := context.WithCancel(t.Context())
		fx.rec.setHook(func(req *http.Request) {
			if strings.HasSuffix(req.URL.Path, "/start") {
				cancel()
			}
		})
		if _, err := fx.s.RunWindow(ctx, fx.live, fx.batches()); !errors.Is(err, context.Canceled) {
			t.Errorf("RunWindow = %v, want context.Canceled", err)
		}
		checkCancelled(t, fx)
	})
}

// checkCancelled checks the window run was cancelled and has drained:
// no target is running.
func checkCancelled(t *testing.T, fx *fixture) {
	t.Helper()
	cancels := fx.rec.sent("POST /runs/")
	var runID string
	for _, c := range cancels {
		if strings.HasSuffix(c, "/cancel") {
			runID = strings.TrimSuffix(strings.TrimPrefix(c, "POST /runs/"), "/cancel")
		}
	}
	if runID == "" {
		t.Fatalf("no cancel sent: %q", cancels)
	}
	run, err := fx.cl.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if st := run.Targets["1"].State; st != "cancelled" && st != "done" {
		t.Errorf("run %s target 1 is %q after the drain", runID, st)
	}
}

// TestWindowRunNotDone checks a run cancelled from outside is excluded,
// and its records are still judged.
func TestWindowRunNotDone(t *testing.T) {
	core := hashcore.New()
	// The cheat fails every /hash and every /verify, so the sends made
	// before the cancel hold one whichever kinds they are.
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": noShed(bothHasher{fakeHasher{core}})}, "1", "2")
	other := client.New(fx.srvURL, http.DefaultClient)
	fx.rec.setHook(func(req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/start") {
			runID := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/runs/"), "/start")
			go func() {
				time.Sleep(1200 * time.Millisecond)
				other.Cancel(context.Background(), runID)
			}()
		}
	})
	res := fx.window()
	if strings.Join(res.Excluded, ",") != ExcludedRunEnded {
		t.Fatalf("excluded %v, want run-not-done", res.Excluded)
	}
	if v := res.Variants["2"]; v.Disqualified != classifier.ReasonVerifyMismatch && v.Disqualified != classifier.ReasonWrongMatch {
		t.Errorf("cheat in a cancelled run: %+v, want verify_mismatch or wrong_match", v)
	}
	if v := res.Variants["1"]; v.Records == 0 || v.Records >= 20 || v.Baseline != nil {
		t.Errorf("honest variant: %+v; want some records, not all, no score", v)
	}
}

// TestWindowGuards checks a batch is never used twice and that every
// variant's CPU unmeasured at once is a fault.
func TestWindowGuards(t *testing.T) {
	core := hashcore.New()
	fx := newFixture(t, map[string]http.Handler{"1": gen0(core), "2": gen0(core)}, "1", "2")
	bs := fx.batches()
	if _, err := fx.s.RunWindow(t.Context(), fx.live, bs[:3]); err == nil || !strings.Contains(err.Error(), "3 batches for 2 live variants and 2 halves") {
		t.Errorf("a batch short: %v", err)
	}
	if _, err := fx.s.RunWindow(t.Context(), fx.live, []Batch{bs[0], bs[1], bs[2], bs[0]}); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Errorf("one batch for two halves: %v", err)
	}
	if _, err := fx.s.RunWindow(t.Context(), fx.live, bs); err != nil {
		t.Fatalf("RunWindow: %v", err)
	}
	if _, err := fx.s.RunWindow(t.Context(), fx.live, bs); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Errorf("batches used again: %v", err)
	}
	fx.cluster.gone = map[string]bool{"1": true, "2": true}
	if _, err := fx.s.RunWindow(t.Context(), fx.live, fx.batches()); err == nil || !strings.Contains(err.Error(), "CPU counters") {
		t.Errorf("every variant's CPU unmeasured: %v", err)
	}
}
