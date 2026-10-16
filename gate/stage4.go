package gate

import (
	"bytes"
	"context"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
)

// Stage 4 (DESIGN.md sec 7): the candidate's static race build, booted
// like Stage 1's bare boot, under concurrent load.
const (
	// minConcurrent and maxConcurrent clamp N, the number of concurrent
	// requests (concurrentRequests).
	minConcurrent = 8
	maxConcurrent = 64

	// maxAttempts bounds the sends per request: a graceful shed or a
	// transport error is retried, up to this many sends in all.
	maxAttempts = 15

	// retryDelay plus up to retryJitter is the wait before a retry. A
	// shed's Retry-After is validated, not honoured.
	retryDelay  = time.Second
	retryJitter = 250 * time.Millisecond

	// stageDeadline bounds the whole load phase.
	stageDeadline = 120 * time.Second

	// raceClientTimeout bounds one request. HangTimeout is not applied
	// under -race; cmd/service's WriteTimeout (6s) still is.
	raceClientTimeout = 30 * time.Second

	// raceGrace is how long the gate waits after the load phase (or the
	// candidate's early exit) for a race on a path that runs after the
	// response (e.g. a deferred write).
	raceGrace = time.Second

	// goRace makes the race runtime exit with raceExitCode at the first
	// report, without its default 1s sleep at exit; log_path is not set,
	// since the sandbox fs is read-only, so reports go to stderr.
	goRace       = "halt_on_error=1 exitcode=66 atexit_sleep_ms=0"
	raceExitCode = 66

	// minRaceProcs is the floor on the race boot's GOMAXPROCS
	// (raceProcs).
	minRaceProcs = 4

	// raceMarker starts every race report; raceTailBytes of output are
	// kept for the excerpt; the race-report Detail is at most
	// raceDetailBytes.
	raceMarker      = "WARNING: DATA RACE"
	raceTailBytes   = 8 << 10
	raceDetailBytes = 4 << 10

	// maxListedProblems caps the failing requests listed in Detail.
	maxListedProblems = 10
)

// concurrentRequests is N for a gate host with numCPU CPUs: twice the
// CPU count, so gen 0 (one slot per GOMAXPROCS, raceProcs) sheds on
// hosts up to 32 CPUs while the per-CPU load, and so the race build's
// latency, stays the same, clamped to [minConcurrent, maxConcurrent].
func concurrentRequests(numCPU int) int {
	return min(max(2*numCPU, minConcurrent), maxConcurrent)
}

// raceProcs is the race boot's GOMAXPROCS for a gate host with numCPU
// CPUs: at least minRaceProcs, so a wrapper that sizes its concurrency
// from GOMAXPROCS (gen 0's slots) has several holders in flight at once
// even on a 1- or 2-CPU host. The race detector only sees a race between
// two holders of a chan struct{} semaphore when both run at once: it
// orders every send and receive on that channel through one sync object,
// so one holder's release happens-before the next holder's acquire.
func raceProcs(numCPU int) int { return max(numCPU, minRaceProcs) }

// raceEnv is the race boot's extra environment: GORACE and GOMAXPROCS.
func raceEnv() []string {
	return []string{"GORACE", goRace, "GOMAXPROCS", strconv.Itoa(raceProcs(runtime.NumCPU()))}
}

// raceRequest is one of Stage 4's concurrent requests.
type raceRequest struct {
	kind      classifier.Kind
	path      string
	password  string
	hash      string // /verify only
	wantMatch bool   // /verify only
}

// planRequests returns n requests, alternating /hash and /verify. /hash
// gets fresh passwords (never pool ones, as in Stage 2); /verify gets
// pool pairs with distinct passwords, alternately Match true and false.
func planRequests(n int) []raceRequest {
	pairs := testvectors.Pairs()
	mrand.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	used := make(map[string]bool)
	next := func(match bool) testvectors.Pair {
		for _, p := range pairs {
			if p.Match == match && !used[p.Password] {
				used[p.Password] = true
				return p
			}
		}
		panic("gate: test-vector pool too small") // TestPlanRequests guards this
	}
	reqs := make([]raceRequest, n)
	for i := range reqs {
		if i%2 == 0 {
			reqs[i] = raceRequest{kind: classifier.Hash, path: "/hash", password: freshPassword()}
			continue
		}
		p := next(i%4 == 1)
		reqs[i] = raceRequest{kind: classifier.Verify, path: "/verify", password: p.Password, hash: p.Hash, wantMatch: p.Match}
	}
	return reqs
}

func (q raceRequest) body() []byte {
	if q.kind == classifier.Hash {
		return jsonBody(map[string]string{"password": q.password})
	}
	return jsonBody(map[string]string{"password": q.password, "hash": q.hash})
}

// outcome is one request's last response after retries. problem is set
// when it never got a final answer: a bad X-EK8s-Variant, retries
// exhausted, the candidate exited, or the stage deadline.
type outcome struct {
	o        classifier.Observation
	attempts int
	problem  string
}

// sendWithRetry sends until the answer is neither a graceful shed (a
// 503 that classifies as graceful_shed, latency aside) nor a transport
// error, at most maxAttempts times, waiting wait() between sends. A
// transport error is one with no response status; a response whose body
// failed to read (cut short) is final. A transport error is not retried
// once exited is closed. send returns the observation (Kind set) and any
// X-EK8s-Variant problem.
func sendWithRetry(ctx context.Context, send func(context.Context) (classifier.Observation, string), exited <-chan struct{}, wait func() time.Duration) outcome {
	var out outcome
	for out.attempts < maxAttempts {
		out.attempts++
		o, problem := send(ctx)
		out.o = o
		if problem != "" {
			out.problem = problem
			return out
		}
		if ctx.Err() != nil {
			out.problem = fmt.Sprintf("stage deadline (%v) reached", stageDeadline)
			return out
		}
		if o.Err != nil && o.Status != 0 {
			return out // a response arrived; its body is the problem
		}
		if o.Err != nil {
			select {
			case <-exited:
				out.problem = fmt.Sprintf("candidate exited (%v)", o.Err)
				return out
			default:
			}
		} else if !gracefulShed(o) {
			return out
		}
		if out.attempts == maxAttempts {
			break
		}
		select {
		case <-time.After(wait()):
		case <-exited:
			out.problem = "candidate exited during a retry wait"
			return out
		case <-ctx.Done():
			out.problem = fmt.Sprintf("stage deadline (%v) reached", stageDeadline)
			return out
		}
	}
	last := "503 shed"
	if out.o.Err != nil {
		last = "transport error: " + out.o.Err.Error()
	}
	out.problem = fmt.Sprintf("no answer but a retryable one in %d attempts (last: %s)", maxAttempts, last)
	return out
}

// gracefulShed reports whether o is a 503 the classifier calls
// graceful_shed, ignoring latency (HangTimeout is not applied under
// -race). A 503 classifies before any body or salt check, so a
// throwaway Run is enough.
func gracefulShed(o classifier.Observation) bool {
	if o.Status != http.StatusServiceUnavailable {
		return false
	}
	b, _ := classifier.NewRun(nil).Classify(classifier.Observation{Kind: o.Kind, Status: o.Status, Header: o.Header})
	return b == classifier.GracefulShed
}

func retryWait() time.Duration { return retryDelay + mrand.N(retryJitter+1) }

// runStage4 is Stage 4. It builds and boots the race probe (any failure
// a gate fault), then the candidate's race build, sends N concurrent
// requests and checks for a race report, then that the candidate is
// still running and every response. Results:
// race-build, race-boot, race-report, concurrency. run is Stage 2's
// classifier Run, so salts are checked across both stages.
func runStage4(ctx context.Context, r *Report, tc toolchain, tree, repo, work, relay string, run *classifier.Run) (ok bool, err error) {
	dir := filepath.Join(work, "race") // its own relay.sock
	if err := os.Mkdir(dir, 0o700); err != nil {
		return false, err
	}
	env := raceEnv()
	probe := filepath.Join(dir, "probe")
	if ok, out, err := goBuild(ctx, tc, repo, probePkg, probe, true); err != nil {
		return false, fmt.Errorf("race build %s: %w", probePkg, err)
	} else if !ok {
		return false, fmt.Errorf("race build %s (needs gcc and a static libc): %s", probePkg, out)
	}
	if err := preflight(ctx, probe, relay, dir, env...); err != nil {
		return false, fmt.Errorf("race %w", err)
	}

	service := filepath.Join(dir, "service")
	built, out, err := goBuild(ctx, tc, tree, "cmd/service", service, true)
	if err != nil {
		return false, err
	}
	if !r.add(4, "race-build", buildDetail(built, out)) {
		return false, nil
	}
	label := newLabel()
	sb, problem, err := bootBare(ctx, "candidate", service, relay, dir, label, env...)
	if err != nil {
		return false, err
	}
	if !r.add(4, "race-boot", problem) {
		return false, nil
	}
	defer sb.stop()

	c := *sb.client
	c.Timeout = raceClientTimeout
	s := &interop{client: &c, label: label, run: run}
	reqs := planRequests(concurrentRequests(runtime.NumCPU()))
	outs := load(ctx, s, reqs, sb.exited)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	select {
	case <-time.After(raceGrace):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if !r.add(4, "race-report", sb.raceReport()) {
		return false, nil
	}
	problem = checkOutcomes(s.run, reqs, outs)
	select {
	case <-sb.exited:
		// Exit 66 already failed race-report; any other exit fails here.
		exit := fmt.Sprintf("candidate exited (status %d) by the end of the load", sb.bwrap.ProcessState.ExitCode())
		problem = strings.Join(slices.DeleteFunc([]string{exit, problem}, func(p string) bool { return p == "" }), "; ")
	default:
	}
	if problem != "" {
		problem = sb.withOutput(problem)
	}
	return r.add(4, "concurrency", problem), nil
}

// load sends every request at once, released together by a closed
// channel, each with sendWithRetry, under stageDeadline.
func load(ctx context.Context, s *interop, reqs []raceRequest, exited <-chan struct{}) []outcome {
	ctx, cancel := context.WithTimeout(ctx, stageDeadline)
	defer cancel()
	outs := make([]outcome, len(reqs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, q := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			send := func(ctx context.Context) (classifier.Observation, string) {
				o, problem := s.post(ctx, q.path, q.body())
				o.Kind = q.kind
				return o, problem
			}
			outs[i] = sendWithRetry(ctx, send, exited, retryWait)
		}()
	}
	close(start)
	wg.Wait()
	return outs
}

// checkOutcomes classifies every final response, after the load phase so
// the gate's own Argon2 (FullVerify) does not compete with the candidate:
// each must be exactly 200 and classify as success, latency aside, hash
// responses fully verified against their own password. It lists failing
// requests by index, endpoint and attempts, never passwords or hashes.
func checkOutcomes(run *classifier.Run, reqs []raceRequest, outs []outcome) string {
	var problems []string
	for i, out := range outs {
		q := reqs[i]
		problem := out.problem
		if problem == "" {
			o := out.o
			o.Password, o.WantMatch, o.FullVerify, o.Latency = q.password, q.wantMatch, q.kind == classifier.Hash, 0
			b, reason := run.Classify(o)
			switch {
			// A 503 here is not a graceful shed (sendWithRetry retries
			// those), so the classifier names it below.
			case o.Status != http.StatusOK && o.Status != http.StatusServiceUnavailable:
				problem = fmt.Sprintf("status %d, want 200", o.Status)
			case b != classifier.Success:
				problem = fmt.Sprintf("%s %v (%s, status %d)", q.kind, b, reason, o.Status)
			}
		}
		if problem != "" {
			problems = append(problems, fmt.Sprintf("request %d (%s, attempt %d): %s", i+1, q.path, out.attempts, problem))
		}
	}
	if len(problems) > maxListedProblems {
		problems = append(problems[:maxListedProblems], fmt.Sprintf("and %d more", len(problems)-maxListedProblems))
	}
	return strings.Join(problems, "; ")
}

// raceReport returns why the race check fails, or "": the candidate
// exited with raceExitCode, or its output carried raceMarker. Call it
// only once the load phase and raceGrace are over.
func (sb *sandbox) raceReport() string {
	code := false
	select {
	case <-sb.exited:
		code = sb.bwrap.ProcessState.ExitCode() == raceExitCode
	default:
	}
	found, excerpt := sb.race.result()
	if !code && !found {
		return ""
	}
	why := "race report in the output"
	if code {
		why = fmt.Sprintf("exited with the race detector's status %d", raceExitCode)
	}
	head := why + ":\n"
	if !strings.HasPrefix(excerpt, raceMarker) {
		// No report to start from: keep the end of the output, where an
		// exit is explained.
		excerpt = strings.ToValidUTF8(excerpt[max(0, len(excerpt)-(raceDetailBytes-len(head))):], "")
	}
	return capDetail(head+excerpt, raceDetailBytes)
}

// capDetail cuts s to at most n bytes, marking the cut within them; a
// rune split by the cut is dropped.
func capDetail(s string, n int) string {
	const cut = "\n[truncated]"
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n-len(cut)], "") + cut
}

// raceWatch scans all of a sandbox's output for raceMarker, including a
// marker split across writes, however much output came first (a
// candidate can log to stderr to push a report past the 64 KiB
// cappedBuffer), and keeps the last raceTailBytes for the excerpt. With
// halt_on_error the report is the last output. It is safe for
// concurrent use.
type raceWatch struct {
	mu    sync.Mutex
	carry []byte // the last len(raceMarker)-1 bytes so far
	found bool
	tail  []byte
}

func (w *raceWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.found {
		edge := append(slices.Clip(w.carry), p[:min(len(p), len(raceMarker)-1)]...)
		w.found = bytes.Contains(edge, []byte(raceMarker)) || bytes.Contains(p, []byte(raceMarker))
		w.carry = lastBytes(append(w.carry, lastBytes(p, len(raceMarker)-1)...), len(raceMarker)-1)
	}
	w.tail = lastBytes(append(w.tail, lastBytes(p, raceTailBytes)...), raceTailBytes)
	return len(p), nil
}

// lastBytes returns a copy of b's last n bytes (all of b if shorter).
func lastBytes(b []byte, n int) []byte {
	return append([]byte(nil), b[max(0, len(b)-n):]...)
}

// result reports whether the marker was seen, and an excerpt: the tail
// from the first marker in it, or the whole tail if it has none.
func (w *raceWatch) result() (found bool, excerpt string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	tail := w.tail
	if i := bytes.Index(tail, []byte(raceMarker)); i >= 0 {
		tail = tail[i:]
	} else if w.found {
		return true, fmt.Sprintf("(report not in the last %d bytes of output)", raceTailBytes)
	}
	return w.found, string(tail)
}
