package gate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// fakeLoadgen is a loadgen control API with only what a Stage 5 burst
// touches, and knobs to break each part of it. Its zero values are a
// loadgen that serves a burst cleanly.
type fakeLoadgen struct {
	mu sync.Mutex

	boot    string
	cfg     wire.Config
	targets map[string]bool

	putStatus    int      // PUT /targets status, default 201
	canaries     []string // results in order; the last repeats; default refused
	canaryCalls  int
	canaryStatus int // an error status for POST /canary when not 0

	startStatus int // an error status for the start when not 0
	startKind   string
	startTarget []string

	runningPolls int    // GET /runs reads running this many times first
	end          string // the state after that, default done
	forever      bool   // running never ends, until cancelled
	stuck        bool   // running even after a cancel
	polls        int
	cancels      int
	cancelled    bool

	rebootAtPoll int // from this poll on (1-based), answer with another boot_id

	records func(target, run string, n int) []wire.Record // default: n clean records
	gap     bool
	pairs   wire.Upload
	runID   string

	runError     string // the target's error text in a run
	startGarbage bool   // the start is applied, but its response is unreadable
	pageMax      int    // records per page, when not 0
	stuckNext    bool   // a records page does not advance next
}

func newFake() *fakeLoadgen {
	return &fakeLoadgen{
		boot:    "boot-1",
		targets: map[string]bool{},
		cfg: wire.Config{
			Rate: 2, WindowS: 300, BurstRate: 30, BurstSends: 30, BurstPairs: 14,
			WindowSends: 600, WindowPairs: 300, RingCapacity: 700,
		},
	}
}

func (f *fakeLoadgen) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set(wire.BootIDHeader, f.bootNow())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeLoadgen) bootNow() string {
	if f.rebootAtPoll > 0 && f.polls >= f.rebootAtPoll {
		return "boot-2"
	}
	return f.boot
}

func (f *fakeLoadgen) fail(w http.ResponseWriter, status int) {
	f.reply(w, status, wire.Error{BootID: f.bootNow(), Error: "fake"})
}

func (f *fakeLoadgen) handler() http.Handler {
	mux := http.NewServeMux()
	lock := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /config", lock(func(w http.ResponseWriter, r *http.Request) {
		c := f.cfg
		c.BootID = f.bootNow()
		f.reply(w, 200, c)
	}))
	mux.HandleFunc("GET /targets", lock(func(w http.ResponseWriter, r *http.Request) {
		ts := wire.Targets{BootID: f.bootNow(), Targets: []wire.Target{}}
		for id := range f.targets {
			ts.Targets = append(ts.Targets, wire.Target{ID: id})
		}
		f.reply(w, 200, ts)
	}))
	mux.HandleFunc("PUT /targets/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		status := cmpOr(f.putStatus, 201)
		f.targets[r.PathValue("id")] = true
		f.reply(w, status, wire.PutTarget{BootID: f.bootNow(), ID: r.PathValue("id"), Next: 0})
	}))
	mux.HandleFunc("PUT /targets/{id}/pairs/{run}", lock(func(w http.ResponseWriter, r *http.Request) {
		f.pairs = wire.Upload{}
		if err := json.NewDecoder(r.Body).Decode(&f.pairs); err != nil {
			f.fail(w, 400)
			return
		}
		f.reply(w, 201, wire.Uploaded{BootID: f.bootNow(), Count: len(f.pairs.Pairs)})
	}))
	mux.HandleFunc("POST /canary", lock(func(w http.ResponseWriter, r *http.Request) {
		if f.canaryStatus != 0 {
			f.fail(w, f.canaryStatus)
			return
		}
		res := wire.CanaryRefused
		if len(f.canaries) > 0 {
			res = f.canaries[min(f.canaryCalls, len(f.canaries)-1)]
		}
		f.canaryCalls++
		f.reply(w, 200, wire.Canary{BootID: f.bootNow(), Result: res})
	}))
	mux.HandleFunc("POST /runs/{run}/start", lock(func(w http.ResponseWriter, r *http.Request) {
		if f.startStatus != 0 {
			f.fail(w, f.startStatus)
			return
		}
		var s wire.Start
		json.NewDecoder(r.Body).Decode(&s)
		f.startKind, f.startTarget, f.runID = s.Kind, s.Targets, r.PathValue("run")
		if f.startGarbage {
			w.Header().Set(wire.BootIDHeader, f.bootNow())
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{"))
			return
		}
		f.reply(w, 200, wire.Started{BootID: f.bootNow(), RunID: f.runID, StartWall: time.Now().UTC()})
	}))
	runBody := func(state string) wire.Run {
		run := wire.Run{BootID: f.bootNow(), RunID: f.runID, Kind: wire.KindBurst, Targets: map[string]wire.RunTarget{}}
		for _, id := range f.startTarget {
			run.Targets[id] = wire.RunTarget{State: state, Error: f.runError}
		}
		return run
	}
	state := func() string {
		switch {
		case f.cancelled && !f.stuck:
			return wire.StateCancelled
		case f.stuck || f.forever || f.polls <= f.runningPolls:
			return wire.StateRunning
		}
		return cmpOr(f.end, wire.StateDone)
	}
	mux.HandleFunc("GET /runs/{run}", lock(func(w http.ResponseWriter, r *http.Request) {
		f.polls++
		f.reply(w, 200, runBody(state()))
	}))
	mux.HandleFunc("POST /runs/{run}/cancel", lock(func(w http.ResponseWriter, r *http.Request) {
		f.cancels++
		f.cancelled = true
		f.reply(w, 200, runBody(state()))
	}))
	mux.HandleFunc("GET /targets/{id}/records", lock(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		mk := f.records
		if mk == nil {
			mk = cleanRecords
		}
		all := mk(id, f.runID, f.cfg.BurstSends)
		for i := range all {
			all[i].Seq = uint64(i)
		}
		if f.pageMax > 0 {
			limit = min(limit, f.pageMax)
		}
		end := min(len(all), since+limit)
		page := []wire.Record{}
		if since < end {
			page = all[since:end]
		}
		next := since + len(page)
		if f.stuckNext {
			next = since
		}
		f.reply(w, 200, wire.Records{BootID: f.bootNow(), Records: page, Gap: f.gap, Next: uint64(next)})
	}))
	return mux
}

func cmpOr[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// cleanRecords is n success records for target and run, with the
// indices in a scrambled order, as a ring read would not guarantee them.
func cleanRecords(target, run string, n int) []wire.Record {
	recs := make([]wire.Record, n)
	for i := range recs {
		recs[i] = wire.Record{Target: target, RunID: run, Index: (i * 7) % n, Kind: "verify", Bucket: "success"}
	}
	return recs
}

// fastParams are burstParams that wait milliseconds, for a variant "7"
// in run "b1", over a fake whose config matches.
func fastParams(t *testing.T) burstParams {
	t.Helper()
	return burstParams{
		Target: "7", RunID: "b1",
		Want: loadgenShape{Rate: 2, WindowS: 300, BurstRate: 30},
		Core: hashcore.New(),

		canaryEvery: time.Millisecond, canaryBound: 5 * time.Second,
		pollEvery: time.Millisecond, clientTimeout: 10 * time.Millisecond,
		runSlack: 50 * time.Millisecond, drainBound: 5 * time.Second,
	}
}

// run runs a burst against f and returns its result and error.
func (f *fakeLoadgen) run(t *testing.T, ctx context.Context, p burstParams) (burstResult, error) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return runBurst(ctx, client.New(srv.URL, srv.Client()), p)
}

func TestRunBurst(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.runningPolls = 2
	res, err := f.run(t, t.Context(), fastParams(t))
	if err != nil {
		t.Fatalf("runBurst: %v", err)
	}
	if len(res.Records) != 30 || res.Through != 29 {
		t.Fatalf("%d records, through %d; want 30, 29", len(res.Records), res.Through)
	}
	for i, r := range res.Records {
		if r.Index != i {
			t.Fatalf("record %d has index %d: not in index order", i, r.Index)
		}
	}
	if f.startKind != wire.KindBurst || len(f.startTarget) != 1 || f.startTarget[0] != "7" || f.cancels != 0 {
		t.Errorf("started %q %v, %d cancels; want a burst of [7], none", f.startKind, f.startTarget, f.cancels)
	}
	matches := 0
	for _, p := range f.pairs.Pairs {
		if *p.Match {
			matches++
		}
	}
	if len(f.pairs.Pairs) != 14 || matches != 7 {
		t.Errorf("uploaded %d pairs, %d matches; want 14, 7", len(f.pairs.Pairs), matches)
	}
}

// TestRunBurstOddPairs: an odd pair count gives the extra pair to the
// matches.
func TestRunBurstOddPairs(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.cfg.BurstPairs = 15
	if _, err := f.run(t, t.Context(), fastParams(t)); err != nil {
		t.Fatalf("runBurst: %v", err)
	}
	matches := 0
	for _, p := range f.pairs.Pairs {
		if *p.Match {
			matches++
		}
	}
	if len(f.pairs.Pairs) != 15 || matches != 8 {
		t.Errorf("uploaded %d pairs, %d matches; want 15, 8", len(f.pairs.Pairs), matches)
	}
}

// TestRunBurstStartLost: a start whose response is unreadable may have
// started the run, so it is cancelled; a status answer started nothing.
func TestRunBurstStartLost(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.startGarbage = true
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || !strings.Contains(err.Error(), "stage 5 burst") {
		t.Fatalf("runBurst error = %v; want a burst fault", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancels != 1 {
		t.Errorf("%d cancels after a lost start, want 1", f.cancels)
	}
}

// TestRunBurstRecordsStall: records pages that do not advance, or that
// run past the burst's last page, end the read.
func TestRunBurstRecordsStall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(f *fakeLoadgen)
		want string
	}{
		{"stuck_next", func(f *fakeLoadgen) { f.stuckNext = true }, "has next"},
		{"too_many_pages", func(f *fakeLoadgen) { f.pageMax = 1 }, "past the burst's last page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			tc.set(f)
			_, err := f.run(t, t.Context(), fastParams(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runBurst error = %v; want a fault containing %q", err, tc.want)
			}
		})
	}
}

func TestRunBurstPages(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.cfg.BurstRate, f.cfg.BurstSends, f.cfg.BurstPairs = 100, 2500, 0
	p := fastParams(t)
	p.Want.BurstRate = 100
	res, err := f.run(t, t.Context(), p)
	if err != nil || len(res.Records) != 2500 || res.Through != 2499 {
		t.Fatalf("runBurst = %d records, through %d, %v; want 2500, 2499, nil", len(res.Records), res.Through, err)
	}
}

func TestRunBurstPreRunFaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(f *fakeLoadgen, p *burstParams)
		want string
	}{
		{"rate", func(f *fakeLoadgen, p *burstParams) { f.cfg.Rate = 2.5 }, "loadgen config"},
		{"window", func(f *fakeLoadgen, p *burstParams) { f.cfg.WindowS = 60 }, "loadgen config"},
		{"burst_rate", func(f *fakeLoadgen, p *burstParams) { p.Want.BurstRate = 31 }, "loadgen config"},
		{"no_sends", func(f *fakeLoadgen, p *burstParams) { f.cfg.BurstSends = 0 }, "loadgen burst"},
		{"pairs_over_sends", func(f *fakeLoadgen, p *burstParams) { f.cfg.BurstPairs = 31 }, "loadgen burst"},
		{"target_exists", func(f *fakeLoadgen, p *burstParams) { f.targets["7"] = true }, "already in loadgen"},
		{"put_not_created", func(f *fakeLoadgen, p *burstParams) { f.putStatus = 200 }, "existed when put"},
		{"canary_connected", func(f *fakeLoadgen, p *burstParams) { f.canaries = []string{wire.CanaryConnected} }, `canary still reads "connected"`},
		{"canary_error_result", func(f *fakeLoadgen, p *burstParams) { f.canaries = []string{wire.CanaryError} }, `canary still reads "error"`},
		{"canary_http_error", func(f *fakeLoadgen, p *burstParams) { f.canaryStatus = 500 }, "status 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, p := newFake(), fastParams(t)
			p.canaryBound = 20 * time.Millisecond
			tc.set(f, &p)
			_, err := f.run(t, t.Context(), p)
			if err == nil || !strings.Contains(err.Error(), "stage 5 pre-run") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runBurst error = %v; want a pre-run fault containing %q", err, tc.want)
			}
			if f.startKind != "" {
				t.Error("the burst was started")
			}
		})
	}
}

func TestRunBurstCanaryPoll(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.canaries = []string{wire.CanaryConnected, wire.CanaryError, wire.CanaryRefused}
	if _, err := f.run(t, t.Context(), fastParams(t)); err != nil {
		t.Fatalf("runBurst: %v", err)
	}
	if f.canaryCalls != 3 {
		t.Errorf("%d canary calls, want 3", f.canaryCalls)
	}
}

func TestRunBurstStartRefused(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.startStatus = 409
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || !strings.Contains(err.Error(), "stage 5 burst") || !strings.Contains(err.Error(), "status 409") {
		t.Fatalf("runBurst error = %v; want a burst fault with status 409", err)
	}
}

func TestRunBurstRunEnds(t *testing.T) {
	t.Parallel()
	for _, end := range []string{wire.StateCancelled, wire.StateError} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			f.end = end
			_, err := f.run(t, t.Context(), fastParams(t))
			if err == nil || !strings.Contains(err.Error(), "run ended "+strconv.Quote(end)) {
				t.Fatalf("runBurst error = %v; want a fault for a run that ended %s", err, end)
			}
			if f.cancels != 0 {
				t.Error("a run that had ended was cancelled")
			}
		})
	}
}

// TestRunBurstOverBound: a run that never ends is cancelled and drained,
// and the fault says so.
func TestRunBurstOverBound(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.forever = true
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || !strings.Contains(err.Error(), "still running after") {
		t.Fatalf("runBurst error = %v; want a fault for a run past its bound", err)
	}
	if strings.Contains(err.Error(), "draining") {
		t.Errorf("error %v: the cancelled run should have drained", err)
	}
	if f.cancels != 1 {
		t.Errorf("%d cancels, want 1", f.cancels)
	}
}

func TestRunBurstDrainFails(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.forever, f.stuck = true, true
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || !strings.Contains(err.Error(), "still running after") || !strings.Contains(err.Error(), "draining the cancelled run") {
		t.Fatalf("runBurst error = %v; want the bound fault joined with a drain fault", err)
	}
}

// TestRunBurstCtxCancelled: a gate whose context ends mid-run still
// cancels the run, on a detached context.
func TestRunBurstCtxCancelled(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.forever = true
	ctx, cancel := context.WithCancel(t.Context())
	p := fastParams(t)
	p.pollEvery = 5 * time.Millisecond
	p.runSlack = time.Minute
	go func() {
		for { // cancel once the run is being polled
			f.mu.Lock()
			polled := f.polls >= 2
			f.mu.Unlock()
			if polled {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, err := f.run(t, ctx, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runBurst error = %v; want context.Canceled", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancels != 1 {
		t.Errorf("%d cancels, want 1", f.cancels)
	}
}

// TestRunBurstReboot: loadgen restarting mid-run is ErrBootChanged, and
// the gate does not try to cancel a run that no longer exists.
func TestRunBurstReboot(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.forever, f.rebootAtPoll = true, 2
	_, err := f.run(t, t.Context(), fastParams(t))
	if !errors.Is(err, client.ErrBootChanged) {
		t.Fatalf("runBurst error = %v; want ErrBootChanged", err)
	}
	if f.cancels != 0 {
		t.Errorf("%d cancels after a restart, want 0", f.cancels)
	}
}

func TestRunBurstRecordFaults(t *testing.T) {
	t.Parallel()
	mutate := func(f func(recs []wire.Record) []wire.Record) func(target, run string, n int) []wire.Record {
		return func(target, run string, n int) []wire.Record { return f(cleanRecords(target, run, n)) }
	}
	for _, tc := range []struct {
		name string
		set  func(f *fakeLoadgen)
		want string
	}{
		{"gap", func(f *fakeLoadgen) { f.gap = true }, "gap"},
		{"short", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { return r[:29] })
		}, "29 records, want 30"},
		{"long", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { return append(r, r[0]) })
		}, "more than the 30 records"},
		{"duplicate_index", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { r[3].Index = r[4].Index; return r })
		}, "recorded twice"},
		{"index_out_of_range", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { r[3].Index = 30; return r })
		}, "index 30, of 30"},
		{"negative_index", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { r[3].Index = -1; return r })
		}, "index -1"},
		{"other_target", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { r[3].Target = "8"; return r })
		}, "of target"},
		{"other_run", func(f *fakeLoadgen) {
			f.records = mutate(func(r []wire.Record) []wire.Record { r[3].RunID = "b0"; return r })
		}, "run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			tc.set(f)
			_, err := f.run(t, t.Context(), fastParams(t))
			if err == nil || !strings.Contains(err.Error(), "stage 5 burst") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runBurst error = %v; want a burst fault containing %q", err, tc.want)
			}
		})
	}
}

// TestRunBurstFaultTextBounded: text from loadgen's records or a run's
// error reaches an error cut and quoted, never as given.
func TestRunBurstFaultTextBounded(t *testing.T) {
	t.Parallel()
	f := newFake()
	long := strings.Repeat("x", 500) + "\nsecret"
	f.records = func(target, run string, n int) []wire.Record {
		r := cleanRecords(target, run, n)
		r[0].Target = long
		return r
	}
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || len(err.Error()) > 400 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q: want one line with the target cut", err)
	}
}

// TestRunBurstRunErrorBounded: a run's error text is cut and quoted too.
func TestRunBurstRunErrorBounded(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.end = wire.StateError
	f.runError = strings.Repeat("y", 500) + "\nsecret"
	_, err := f.run(t, t.Context(), fastParams(t))
	if err == nil || len(err.Error()) > 400 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q: want one line with the run's error cut", err)
	}
}
