package gate

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
)

// TestStage4Failures: each mutant passes Stages 1-3 (it misbehaves only
// under concurrency) and must fail exactly at failCheck. Skipped under
// -race: the candidate's race build is a separate process either way,
// and CI's plain go test step runs these.
func TestStage4Failures(t *testing.T) {
	if raceEnabled {
		t.Skip("Stage 4 seeds run in the non-race test run")
	}
	setupHost(t)
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("mutation target not found: " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	const (
		hashCall   = "hash, err := s.h.Hash(req.Password)"
		slotsLine  = "slots chan struct{} // semaphore: one element per in-flight Hasher call"
		verifyCall = "match, err := s.h.Verify(req.Password, req.Hash)"
		syncImport = "\"strconv\"\n\t\"sync\"\n\t\"sync/atomic\""
		syncField  = slotsLine + "\n\tmu sync.Mutex\n\tlast string\n\tinflight atomic.Int64"
		// Counts /hash and /verify requests from handler entry: Stage 2
		// (one at a time) never sees more than one; in Stage 4 only the
		// last request standing does.
		inflight    = "\n\t\ts.inflight.Add(1)\n\t\tdefer s.inflight.Add(-1)"
		hashEntry   = "Password string `json:\"password\"`\n\t\t}"
		verifyEntry = "Hash     string `json:\"hash\"`\n\t\t}"
		concurrent  = "\n\t\tif s.inflight.Add(1) > 1 {\n\t\t\ts.concurrent.Store(true)\n\t\t}\n\t\tdefer s.inflight.Add(-1)"
		lateExit    = `func (s *Server) answered200() {
	if !s.concurrent.Load() {
		return
	}
	if s.answered.Add(1) == int64(min(max(2*runtime.NumCPU(), 8), 64)) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			panic("mutant exits after the load")
		}()
	}
}`
		hashRequired = "writeError(w, http.StatusBadRequest, \"hash is required\")\n\t\t\treturn\n\t\t}"
		// 1ms instead of 500ms, so a request that finds every slot taken
		// is shed for certain; Stage 2's one request at a time never is.
		fastShed = "acquireWait = classifier.HangTimeout / 2000"
	)
	type edit1 struct{ file, old, new string }
	tests := []struct {
		name      string
		edits     []edit1
		failCheck string
		detail    string
	}{
		{"unsynchronized shared response", []edit1{
			{"server.go", hashCall, "var err error\n\t\tlastHash, err = s.h.Hash(req.Password)\n\t\thash := lastHash"},
			{"server.go", "var errTrailingData", "var lastHash string\n\nvar errTrailingData"},
		}, "race-report", "exited with the race detector's status 66:\n" + raceMarker},
		{"data race without cross-talk", []edit1{
			{"server.go", hashCall, hashCall + "\n\t\ts.n++"},
			{"server.go", slotsLine, slotsLine + "\n\tn int"},
		}, "race-report", "exited with the race detector's status 66:\n" + raceMarker},
		// Mutex-guarded and atomic, so no race report: only the responses
		// show it. With another request in flight, /hash answers with the
		// previous request's hash (verify_mismatch or salt_reused) ...
		{"hash cross-talk without a race", []edit1{
			{"server.go", "\"strconv\"", syncImport},
			{"server.go", slotsLine, syncField},
			{"server.go", hashEntry, hashEntry + inflight},
			{"server.go", verifyEntry, verifyEntry + inflight},
			{"server.go", hashCall, hashCall + "\n\t\ts.mu.Lock()\n\t\tother := s.last\n\t\ts.last = hash\n\t\ts.mu.Unlock()\n\t\tif other != \"\" && s.inflight.Load() > 1 {\n\t\t\thash = other\n\t\t}"},
		}, "concurrency", "hash failed ("},
		// ... and /verify, which records every hash before acquire,
		// re-checks the password against another request's hash once it
		// has ever seen two requests in flight (sticky, so a request that
		// runs alone in a retry round still leaks), so a matching pair
		// answers false (wrong_match).
		{"verify cross-talk without a race", []edit1{
			{"server.go", "\"strconv\"", syncImport},
			{"server.go", slotsLine, slotsLine + "\n\tmu sync.Mutex\n\tseen []string\n\tinflight atomic.Int64\n\tconcurrent atomic.Bool"},
			{"server.go", hashEntry, hashEntry + concurrent},
			{"server.go", verifyEntry, verifyEntry + concurrent},
			{"server.go", hashRequired, hashRequired + "\n\t\ts.mu.Lock()\n\t\ts.seen = append(s.seen, req.Hash)\n\t\ts.mu.Unlock()"},
			{"server.go", verifyCall, verifyCall + "\n\t\tif err == nil && s.concurrent.Load() {\n\t\t\tother := \"\"\n\t\t\ts.mu.Lock()\n\t\t\tfor _, h := range s.seen {\n\t\t\t\tif h != req.Hash {\n\t\t\t\t\tother = h\n\t\t\t\t\tbreak\n\t\t\t\t}\n\t\t\t}\n\t\t\ts.mu.Unlock()\n\t\t\tif other != \"\" {\n\t\t\t\tmatch, err = s.h.Verify(req.Password, other)\n\t\t\t}\n\t\t}"},
		}, "concurrency", "wrong_match"},
		// Exits only after answering everything (N, computed as the gate
		// does), so no request sees it: only the post-load exit check can.
		// Only under concurrency, so Stage 2 never triggers it.
		{"exits after the load", []edit1{
			{"server.go", "\"strconv\"", "\"strconv\"\n\t\"sync/atomic\""},
			{"server.go", slotsLine, slotsLine + "\n\tinflight, answered atomic.Int64\n\tconcurrent atomic.Bool"},
			{"server.go", hashEntry, hashEntry + concurrent},
			{"server.go", verifyEntry, verifyEntry + concurrent},
			{"server.go", "}{hash})", "}{hash})\n\t\ts.answered200()"},
			{"server.go", "}{match})", "}{match})\n\t\ts.answered200()"},
			{"server.go", "var errTrailingData", lateExit + "\n\nvar errTrailingData"},
		}, "concurrency", "candidate exited (status 2) by the end of the load"},
		{"400 instead of a shed", []edit1{
			{"tunables.go", "acquireWait = classifier.HangTimeout / 4", fastShed},
			{"server.go", "writeError(w, http.StatusServiceUnavailable, \"overloaded, retry later\")", "writeError(w, http.StatusBadRequest, \"overloaded, retry later\")"},
		}, "concurrency", "status 400, want 200"},
		{"bare 503", []edit1{
			{"tunables.go", "acquireWait = classifier.HangTimeout / 4", fastShed},
			{"server.go", "w.Header().Set(\"Retry-After\", strconv.Itoa(retryAfterSeconds))", "_ = strconv.Itoa(retryAfterSeconds)"},
		}, "concurrency", "bare_503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gen0(t)
			for _, e := range tt.edits {
				edit(t, dir, e.file, replace(e.old, e.new))
			}
			rep := run(t, dir)
			if rep.Passed() || len(rep.Results) == 0 {
				t.Fatalf("passed: %+v", rep.Results)
			}
			last := rep.Results[len(rep.Results)-1]
			if last.Pass || last.Stage != 4 || last.Check != tt.failCheck || !strings.Contains(last.Detail, tt.detail) {
				t.Fatalf("got stage %d %s %q, want %s containing %q", last.Stage, last.Check, last.Detail, tt.failCheck, tt.detail)
			}
		})
	}
}

func TestConcurrentRequests(t *testing.T) {
	for _, tt := range []struct{ cpus, want int }{
		{1, 8}, {3, 8}, {4, 8}, {5, 10}, {20, 40}, {32, 64}, {33, 64}, {128, 64},
	} {
		if got := concurrentRequests(tt.cpus); got != tt.want {
			t.Errorf("concurrentRequests(%d) = %d, want %d", tt.cpus, got, tt.want)
		}
	}
}

// TestPlanRequests: alternating /hash and /verify, verify pairs half
// matching, every password distinct, no pool password sent to /hash, at
// both ends of the clamp (so the pool is large enough for maxConcurrent).
func TestPlanRequests(t *testing.T) {
	pool := make(map[string]bool)
	for _, p := range testvectors.Pairs() {
		pool[p.Password] = true
	}
	for _, n := range []int{minConcurrent, 10, maxConcurrent} {
		reqs := planRequests(n)
		if len(reqs) != n {
			t.Fatalf("n=%d: %d requests", n, len(reqs))
		}
		seen := make(map[string]bool)
		var matches, verifies int
		for i, q := range reqs {
			if seen[q.password] {
				t.Errorf("n=%d: request %d repeats a password", n, i+1)
			}
			seen[q.password] = true
			switch {
			case i%2 == 0 && (q.kind != classifier.Hash || q.path != "/hash" || pool[q.password]):
				t.Errorf("n=%d: request %d: want /hash with a fresh password, got %+v", n, i+1, q.kind)
			case i%2 == 1 && (q.kind != classifier.Verify || q.path != "/verify" || !pool[q.password]):
				t.Errorf("n=%d: request %d: want /verify with a pool pair", n, i+1)
			case i%2 == 1:
				verifies++
				if q.wantMatch {
					matches++
				}
			}
		}
		if matches != (verifies+1)/2 {
			t.Errorf("n=%d: %d of %d verify pairs match", n, matches, verifies)
		}
	}
}

// retryServer answers /hash per key ("?k=<name>") with failures until
// the attempt numbered succeedAt, then 200. fail is "shed", "bare503" or
// "reset" (the connection closes with no response).
func retryServer(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	counts := make(map[string]int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		counts[q.Get("k")]++
		n := counts[q.Get("k")]
		mu.Unlock()
		if succeedAt, _ := strconv.Atoi(q.Get("at")); n >= succeedAt {
			w.Write([]byte(`{"hash": "x"}`))
			return
		}
		switch q.Get("fail") {
		case "shed":
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		case "bare503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "reset":
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSendWithRetry covers maxAttempts on both sides (a request that
// succeeds on the last allowed attempt passes, one needing one more
// fails), for sheds and transport errors, and the cases never retried.
func TestSendWithRetry(t *testing.T) {
	srv := retryServer(t)
	open := make(chan struct{})
	gone := make(chan struct{})
	close(gone)
	for _, tt := range []struct {
		name, fail   string
		succeedAt    int
		exited       chan struct{}
		wantAttempts int
		wantProblem  string
		wantStatus   int
	}{
		{"shed, success on the last attempt", "shed", maxAttempts, open, maxAttempts, "", 200},
		{"shed, success one past the bound", "shed", maxAttempts + 1, open, maxAttempts, "no answer but a retryable one in 15 attempts (last: 503 shed)", 0},
		{"reset, success on the last attempt", "reset", maxAttempts, open, maxAttempts, "", 200},
		{"reset, success one past the bound", "reset", maxAttempts + 1, open, maxAttempts, "(last: transport error", 0},
		{"reset after the candidate exited", "reset", 2, gone, 1, "candidate exited", 0},
		{"bare 503 is final", "bare503", 2, open, 1, "", 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			url := srv.URL + "/hash?fail=" + tt.fail + "&k=" + strings.ReplaceAll(tt.name, " ", "_") + "&at=" + strconv.Itoa(tt.succeedAt)
			send := func(ctx context.Context) (classifier.Observation, string) {
				o := classifier.Observation{Kind: classifier.Hash}
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
				resp, err := srv.Client().Do(req)
				if err != nil {
					o.Err = err
					return o, ""
				}
				resp.Body.Close()
				o.Status, o.Header = resp.StatusCode, resp.Header
				return o, ""
			}
			out := sendWithRetry(context.Background(), send, tt.exited, func() time.Duration { return 0 })
			if out.attempts != tt.wantAttempts {
				t.Errorf("attempts %d, want %d", out.attempts, tt.wantAttempts)
			}
			if (tt.wantProblem == "") != (out.problem == "") || !strings.Contains(out.problem, tt.wantProblem) {
				t.Errorf("problem %q, want %q", out.problem, tt.wantProblem)
			}
			if tt.wantStatus != 0 && out.o.Status != tt.wantStatus {
				t.Errorf("status %d, want %d", out.o.Status, tt.wantStatus)
			}
		})
	}
}

// TestSendWithRetryStops: the stage deadline and the candidate exiting
// during a retry wait end the retries.
func TestSendWithRetryStops(t *testing.T) {
	shed := func(context.Context) (classifier.Observation, string) {
		return classifier.Observation{Kind: classifier.Hash, Status: 503, Header: http.Header{"Retry-After": {"1"}}}, ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := sendWithRetry(ctx, shed, make(chan struct{}), func() time.Duration { return 0 }); out.attempts != 1 || !strings.Contains(out.problem, "stage deadline") {
		t.Errorf("deadline: %+v", out)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	waitCancels := func() time.Duration { cancel(); return time.Hour }
	if out := sendWithRetry(ctx, shed, make(chan struct{}), waitCancels); out.attempts != 1 || !strings.Contains(out.problem, "stage deadline") {
		t.Errorf("deadline during the wait: %+v", out)
	}
	gone := make(chan struct{})
	waitExits := func() time.Duration { close(gone); return time.Hour }
	if out := sendWithRetry(context.Background(), shed, gone, waitExits); out.attempts != 1 || out.problem != "candidate exited during a retry wait" {
		t.Errorf("exit during the wait: %+v", out)
	}
}

// TestSendWithRetryBodyError: a response that arrived but whose body
// failed to read (interop.post sets Err with the status) is final, not a
// retryable transport error.
func TestSendWithRetryBodyError(t *testing.T) {
	calls := 0
	send := func(context.Context) (classifier.Observation, string) {
		calls++
		return classifier.Observation{Kind: classifier.Hash, Status: 200, Err: errors.New("response body over 65536 bytes")}, ""
	}
	out := sendWithRetry(context.Background(), send, make(chan struct{}), func() time.Duration { return 0 })
	if calls != 1 || out.problem != "" || out.o.Status != 200 {
		t.Fatalf("calls %d, outcome %+v", calls, out)
	}
	never := func(string, string) (bool, error) { return false, errors.New("not reached") }
	if got := checkOutcomes(classifier.NewRun(never), []raceRequest{{kind: classifier.Hash, path: "/hash"}}, []outcome{out}); !strings.Contains(got, "transport_error, status 200") {
		t.Fatalf("checkOutcomes: %q", got)
	}
}

// TestSendWithRetryVariantProblem: a missing or wrong X-EK8s-Variant is
// final, never retried.
func TestSendWithRetryVariantProblem(t *testing.T) {
	calls := 0
	send := func(context.Context) (classifier.Observation, string) {
		calls++
		return classifier.Observation{Kind: classifier.Hash, Status: 503, Header: http.Header{"Retry-After": {"1"}}}, "X-EK8s-Variant []"
	}
	out := sendWithRetry(context.Background(), send, make(chan struct{}), func() time.Duration { return 0 })
	if calls != 1 || out.problem != "X-EK8s-Variant []" {
		t.Fatalf("calls %d, outcome %+v", calls, out)
	}
}

// TestOutputWriterFlood: through the writer bootBare gives bwrap, every
// write is accepted in full after cappedBuffer is full, and a child that
// writes far past its 64 KiB before the race marker is drained to the
// end with the marker found. A short write would make os/exec stop
// copying and close the pipe, failing the child (EPIPE) and the scan.
func TestOutputWriterFlood(t *testing.T) {
	sb := &sandbox{output: new(cappedBuffer), race: new(raceWatch)}
	out := sb.outputWriter()
	for range 3 {
		if n, err := out.Write(bytes.Repeat([]byte("x"), maxOutputBytes)); n != maxOutputBytes || err != nil {
			t.Fatalf("write past the cap: n %d, err %v", n, err)
		}
	}
	cmd := exec.Command("sh", "-c", `head -c 1000000 /dev/zero | tr '\0' y; echo; echo "`+raceMarker+`" >&2`)
	cmd.Stdout, cmd.Stderr = out, out
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child: %v", err)
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("child blocked: output stopped being drained")
	}
	if found, excerpt := sb.race.result(); !found || !strings.HasPrefix(excerpt, raceMarker) {
		t.Fatalf("found %v, excerpt %q", found, excerpt)
	}
	if !strings.Contains(sb.output.String(), "more bytes dropped") {
		t.Fatal("cappedBuffer did not cap")
	}
}

// TestRaceWatch: the marker is found when split across writes, after more
// output than cappedBuffer keeps; the excerpt starts at it and is capped.
func TestRaceWatch(t *testing.T) {
	w := new(raceWatch)
	w.Write(bytes.Repeat([]byte("x"), 2*maxOutputBytes))
	report := raceMarker + "\nWrite at 0x00c by goroutine 7:\n" + strings.Repeat("frame\n", 1000)
	for i := range len(report) {
		w.Write([]byte{report[i]})
	}
	found, excerpt := w.result()
	if !found || excerpt != report {
		t.Fatalf("found %v, excerpt %d bytes starting %q", found, len(excerpt), excerpt[:min(40, len(excerpt))])
	}

	w = new(raceWatch)
	w.Write([]byte("WARNING: DATA RAC"))
	w.Write([]byte("e: not the marker"))
	if found, _ := w.result(); found {
		t.Fatal("found a marker that is not there")
	}

	// raceTailBytes: a marker that starts exactly raceTailBytes from the
	// end is still excerpted; one byte more pushes it out of the tail,
	// and it is still reported.
	for _, extra := range []int{0, 1} {
		w = new(raceWatch)
		w.Write([]byte(raceMarker))
		w.Write(bytes.Repeat([]byte("y"), raceTailBytes-len(raceMarker)+extra))
		found, excerpt := w.result()
		inTail := strings.HasPrefix(excerpt, raceMarker)
		if !found || inTail != (extra == 0) || (extra == 1 && !strings.Contains(excerpt, "not in the last")) {
			t.Fatalf("extra %d: found %v, excerpt starts %q", extra, found, excerpt[:min(40, len(excerpt))])
		}
	}
}

// TestRaceReport: exit status 66 alone fails the check (no marker
// needed), any other exit alone does not, and the Detail stays within
// raceDetailBytes.
func TestRaceReport(t *testing.T) {
	exitWith := func(code string, output []byte) *sandbox {
		sb := &sandbox{bwrap: exec.Command("sh", "-c", "exit "+code), race: new(raceWatch), exited: make(chan struct{})}
		sb.bwrap.Run()
		close(sb.exited)
		sb.race.Write(output)
		return sb
	}
	if d := exitWith("66", []byte("no marker here")).raceReport(); !strings.HasPrefix(d, "exited with the race detector's status 66:") {
		t.Errorf("exit 66 without a marker: %q", d)
	}
	if d := exitWith("1", []byte("no marker here")).raceReport(); d != "" {
		t.Errorf("exit 1 without a marker: %q", d)
	}
	if d := exitWith("1", []byte(raceMarker)).raceReport(); !strings.HasPrefix(d, "race report in the output:\n"+raceMarker) {
		t.Errorf("marker without exit 66: %q", d)
	}
	running := &sandbox{race: new(raceWatch), exited: make(chan struct{})}
	if d := running.raceReport(); d != "" {
		t.Errorf("still running, no marker: %q", d)
	}
	// Exit 66 with no marker: the excerpt is the end of the output.
	if d := exitWith("66", append(bytes.Repeat([]byte("a"), 6000), "THE END"...)).raceReport(); len(d) > raceDetailBytes || !strings.Contains(d, "THE END") {
		t.Errorf("exit 66, no marker, long output: %d bytes, end kept %v", len(d), strings.Contains(d, "THE END"))
	}
	long := append([]byte(raceMarker), bytes.Repeat([]byte("z"), 6000)...) // within raceTailBytes
	if d := exitWith("66", long).raceReport(); len(d) != raceDetailBytes || !strings.HasSuffix(d, "[truncated]") {
		t.Errorf("long report: %d bytes", len(d))
	}
}

func TestCapDetail(t *testing.T) {
	for _, n := range []int{raceDetailBytes, raceDetailBytes + 1} {
		got := capDetail(strings.Repeat("a", n), raceDetailBytes)
		if len(got) != raceDetailBytes || strings.HasSuffix(got, "[truncated]") != (n > raceDetailBytes) {
			t.Errorf("len %d in: %d bytes out, %q at the end", n, len(got), got[len(got)-12:])
		}
	}
	// A cut through a 2-byte rune drops the partial rune.
	for _, prefix := range []string{"", "a"} {
		got := capDetail(prefix+strings.Repeat("\u00e9", raceDetailBytes), raceDetailBytes)
		if !utf8.ValidString(got) || len(got) > raceDetailBytes {
			t.Errorf("prefix %q: valid %v, %d bytes", prefix, utf8.ValidString(got), len(got))
		}
	}
}

// TestCheckOutcomesListCap: maxListedProblems failing requests are all
// listed; one more is summarized.
func TestCheckOutcomesListCap(t *testing.T) {
	for _, n := range []int{maxListedProblems, maxListedProblems + 1} {
		reqs := make([]raceRequest, n)
		outs := make([]outcome, n)
		for i := range outs {
			reqs[i] = raceRequest{kind: classifier.Hash, path: "/hash"}
			outs[i] = outcome{attempts: 1, problem: "boom"}
		}
		got := checkOutcomes(classifier.NewRun(nil), reqs, outs)
		if c := strings.Count(got, "boom"); c != maxListedProblems {
			t.Errorf("n=%d: %d listed", n, c)
		}
		if more := strings.HasSuffix(got, "and 1 more"); more != (n > maxListedProblems) {
			t.Errorf("n=%d: %q", n, got)
		}
	}
}

// TestBwrapArgsEnv: extra env pairs become --setenv after EK8S_VARIANT,
// and the binary is still the only thing bound.
func TestBwrapArgsEnv(t *testing.T) {
	args := strings.Join(bwrapArgs("/b", "gate-x", "h", "GORACE", goRace), " ")
	if !strings.Contains(args, "--setenv EK8S_VARIANT gate-x --setenv GORACE "+goRace+" --uid") {
		t.Fatalf("argv %q", args)
	}
	if strings.Count(args, "-bind") != 1 {
		t.Fatalf("argv binds more than the binary: %q", args)
	}
}

// TestStopTwice: a second stop, as Run's deferred one after the explicit
// stop before Stage 4, returns without error or hang, and the sandbox
// stays down.
func TestStopTwice(t *testing.T) {
	setupHost(t)
	tc, err := resolveToolchain(context.Background(), repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	relay, probe := filepath.Join(work, "relay"), filepath.Join(work, "probe")
	for _, b := range []struct{ pkg, out string }{{relayPkg, relay}, {probePkg, probe}} {
		if ok, out, err := goBuild(context.Background(), tc, repoRoot(t), b.pkg, b.out, false); !ok || err != nil {
			t.Fatalf("build %s: %v %s", b.pkg, err, out)
		}
	}
	sb, problem, err := bootBare(context.Background(), "probe", probe, relay, work, newLabel())
	if err != nil || problem != "" {
		t.Fatalf("boot: %v %s", err, problem)
	}
	sb.stop()
	done := make(chan struct{})
	go func() { sb.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second stop did not return")
	}
	if checkHealth(context.Background(), sb.client, "http://candidate/healthz", "") == "" {
		t.Fatal("sandbox still answers after stop")
	}
}
