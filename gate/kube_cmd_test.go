package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeScript is a fake tool run under /bin/sh with a directory D: it
// counts calls in D/n, appends its argv (one arg per line, then "--") to
// D/argv and its stdin to D/stdin, prints D/out.<n> (the last out file
// once n is past the end) and exits with the code in D/exit.<n> if that
// file exists, else 0. If D/sleep.<n> exists it sleeps 30 s instead of
// printing.
const fakeScript = `D=%s
n=0
[ -f "$D/n" ] && read n < "$D/n"
n=$((n+1))
printf '%%s\n' "$n" > "$D/n"
for a in "$@"; do printf '%%s\n' "$a"; done >> "$D/argv"
printf -- '--\n' >> "$D/argv"
cat >> "$D/stdin"
[ -f "$D/sleep.$n" ] && exec sleep 30
i=$n
while [ ! -f "$D/out.$i" ] && [ "$i" -gt 1 ]; do i=$((i-1)); done
[ -f "$D/out.$i" ] && cat "$D/out.$i"
if [ -f "$D/exit.$n" ]; then
	read c < "$D/exit.$n"
	echo boom >&2
	exit "$c"
fi
exit 0
`

// fakeTool installs a fake named name that prints outs in call order and
// returns its directory. exits maps a 1-based call number to its exit
// code.
func fakeTool(t *testing.T, name string, outs []string, exits map[int]int) string {
	t.Helper()
	dir := t.TempDir()
	for i, o := range outs {
		must(t, os.WriteFile(filepath.Join(dir, "out."+strconv.Itoa(i+1)), []byte(o), 0o644))
	}
	for n, c := range exits {
		must(t, os.WriteFile(filepath.Join(dir, "exit."+strconv.Itoa(n)), []byte(strconv.Itoa(c)+"\n"), 0o644))
	}
	fakeToolPath(t, name, fmt.Sprintf(fakeScript, dir))
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func calls(t *testing.T, dir string) int {
	t.Helper()
	s := strings.TrimSpace(readFile(t, filepath.Join(dir, "n")))
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	must(t, err)
	return n
}

// bounded runs f in a goroutine and fails the test if it takes over 10 s.
func bounded(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return within 10 s")
	}
}

func setReady(t *testing.T, poll, timeout time.Duration) {
	t.Helper()
	p, to := readyPoll, readyTimeout
	readyPoll, readyTimeout = poll, timeout
	t.Cleanup(func() { readyPoll, readyTimeout = p, to })
}

func list(items ...string) string { return string(kubeJSON(items...)) }

func TestKubectl(t *testing.T) {
	dir := fakeTool(t, "kubectl", []string{"hello\n"}, nil)
	var out string
	var err error
	bounded(t, func() {
		out, err = kubectl(context.Background(), "kind-x", strings.NewReader("in"), "get", "pods")
	})
	must(t, err)
	if out != "hello" {
		t.Errorf("out = %q, want hello", out)
	}
	if got, want := readFile(t, filepath.Join(dir, "argv")), "get\npods\n--context\nkind-x\n--\n"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "stdin")); got != "in" {
		t.Errorf("stdin = %q, want in", got)
	}
}

func TestKubectlFailure(t *testing.T) {
	fakeTool(t, "kubectl", []string{""}, map[int]int{1: 1})
	var err error
	bounded(t, func() { _, err = kubectl(context.Background(), "kind-x", nil, "get", "pods") })
	if err == nil || !strings.HasPrefix(err.Error(), "kubectl get: exit status 1:") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want kubectl get: exit status 1: ... boom", err)
	}
}

func TestKindLoad(t *testing.T) {
	dir := fakeTool(t, "kind", []string{""}, nil)
	var err error
	bounded(t, func() { err = kindLoad(context.Background(), "c1", "img:1") })
	must(t, err)
	if got, want := readFile(t, filepath.Join(dir, "argv")), "load\ndocker-image\nimg:1\n--name\nc1\n--\n"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestWaitReady(t *testing.T) {
	pending := list(testDeploy, unschedP1)
	tests := []struct {
		name    string
		outs    []string
		exits   map[int]int
		timeout time.Duration
		ok      bool
		problem string
		fault   string
		polls   int
	}{
		{name: "pending, pending, ready", outs: []string{pending, pending, list(testDeploy, readyP1)}, timeout: 5 * time.Second, ok: true, polls: 3},
		{name: "restart on poll 2", outs: []string{pending, list(testDeploy, restartP1)}, timeout: 5 * time.Second, problem: "pod p1 container svc: restarts 1", polls: 2},
		{name: "evicted", outs: []string{list(testDeploy, evictedP1)}, timeout: 5 * time.Second, fault: "Evicted", polls: 1},
		{name: "kubectl exit 1", outs: []string{""}, exits: map[int]int{1: 1}, timeout: 5 * time.Second, fault: "kubectl get: exit status 1", polls: 1},
		{name: "bad JSON", outs: []string{"not json"}, timeout: 5 * time.Second, fault: "kubectl output", polls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeTool(t, "kubectl", tc.outs, tc.exits)
			setReady(t, 10*time.Millisecond, tc.timeout)
			var problem string
			var fault error
			bounded(t, func() { problem, fault = waitReady(context.Background(), "kind-x", "ns", tc.timeout) })
			if tc.ok && (problem != "" || fault != nil) {
				t.Errorf("got (%q, %v), want ready", problem, fault)
			}
			if problem != tc.problem {
				t.Errorf("problem = %q, want %q", problem, tc.problem)
			}
			if tc.fault == "" && fault != nil && !tc.ok || tc.fault != "" && (fault == nil || !strings.Contains(fault.Error(), tc.fault)) {
				t.Errorf("fault = %v, want containing %q", fault, tc.fault)
			}
			if n := calls(t, dir); n != tc.polls {
				t.Errorf("polls = %d, want %d", n, tc.polls)
			}
		})
	}
}

func TestWaitReadyPendingForever(t *testing.T) {
	dir := fakeTool(t, "kubectl", []string{list(testDeploy, unschedP1)}, nil)
	setReady(t, 10*time.Millisecond, 50*time.Millisecond)
	var problem string
	var fault error
	start := time.Now()
	bounded(t, func() { problem, fault = waitReady(context.Background(), "kind-x", "ns", 50*time.Millisecond) })
	elapsed := time.Since(start)
	if problem != "" || fault == nil || !strings.Contains(fault.Error(), "Unschedulable") {
		t.Errorf("got (%q, %v), want a fault naming Unschedulable", problem, fault)
	}
	if elapsed < 50*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want within [50 ms, 5 s]", elapsed)
	}
	if n := calls(t, dir); n < 2 {
		t.Errorf("polls = %d, want at least 2 (one after the deadline)", n)
	}
}

func TestWaitReadyKubectlHangs(t *testing.T) {
	dir := fakeTool(t, "kubectl", []string{list(testDeploy, readyP1)}, nil)
	must(t, os.WriteFile(filepath.Join(dir, "sleep.1"), nil, 0o644))
	setReady(t, 10*time.Millisecond, 50*time.Millisecond)
	g := readyGrace
	readyGrace = 100 * time.Millisecond
	t.Cleanup(func() { readyGrace = g })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var problem string
	var fault error
	start := time.Now()
	bounded(t, func() { problem, fault = waitReady(ctx, "kind-x", "ns", 50*time.Millisecond) })
	elapsed := time.Since(start)
	if problem != "" || !errors.Is(fault, context.DeadlineExceeded) || !strings.Contains(fault.Error(), "did not answer") {
		t.Errorf("got (%q, %v), want a fault wrapping DeadlineExceeded containing \"did not answer\"", problem, fault)
	}
	if elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want within [150 ms, 5 s]", elapsed)
	}
	if ctx.Err() != nil {
		t.Errorf("caller's ctx ended: %v", ctx.Err())
	}
}

func TestWaitReadyContextCancelled(t *testing.T) {
	fakeTool(t, "kubectl", []string{list(testDeploy, unschedP1)}, nil)
	setReady(t, 5*time.Second, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	var problem string
	var fault error
	start := time.Now()
	bounded(t, func() { problem, fault = waitReady(ctx, "kind-x", "ns", time.Minute) })
	if problem != "" || !errors.Is(fault, context.Canceled) {
		t.Errorf("got (%q, %v), want a fault wrapping context.Canceled", problem, fault)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("returned after %v, want prompt", elapsed)
	}
}

func TestPodHealthCheck(t *testing.T) {
	tests := []struct {
		name    string
		outs    []string
		exits   map[int]int
		problem string
		fault   string
	}{
		{name: "ready pods", outs: []string{list(testDeploy, readyP1)}},
		{name: "restart", outs: []string{list(testDeploy, restartP1)}, problem: "pod p1 container svc: restarts 1"},
		{name: "evicted", outs: []string{list(testDeploy, evictedP1)}, fault: "Evicted"},
		{name: "kubectl failure", outs: []string{""}, exits: map[int]int{1: 1}, fault: "kubectl get: exit status 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeTool(t, "kubectl", tc.outs, tc.exits)
			var problem string
			var fault error
			bounded(t, func() { problem, fault = podHealthCheck(context.Background(), "kind-x", "ns") })
			if problem != tc.problem {
				t.Errorf("problem = %q, want %q", problem, tc.problem)
			}
			if tc.fault == "" && fault != nil || tc.fault != "" && (fault == nil || !strings.Contains(fault.Error(), tc.fault)) {
				t.Errorf("fault = %v, want containing %q", fault, tc.fault)
			}
		})
	}
}

func TestPodsRunning(t *testing.T) {
	tests := []struct {
		name  string
		outs  []string
		exits map[int]int
		want  int
		err   string // "" means nil
	}{
		{name: "one Running pod", outs: []string{list(readyP1)}, want: 1},
		{name: "zero pods", outs: []string{list()}, want: 1, err: "0 pods"},
		{name: "two pods", outs: []string{list(readyP1, readyP2)}, want: 1, err: "2 pods"},
		{name: "Pending pod", outs: []string{list(unschedP1)}, want: 1, err: "pod p1: phase Pending"},
		{name: "deleting pod", outs: []string{list(deletingP2)}, want: 1, err: "pod p2: deletionTimestamp"},
		{name: "kubectl failure", outs: []string{""}, exits: map[int]int{1: 1}, want: 1, err: "kubectl get: exit status 1"},
		{name: "non-Pod item", outs: []string{list(testDeploy)}, want: 1, err: "unexpected item kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeTool(t, "kubectl", tc.outs, tc.exits)
			var err error
			bounded(t, func() { err = podsRunning(context.Background(), "kind-x", "ns", "app=svc", tc.want) })
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Errorf("err = %v, want containing %q", err, tc.err)
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, "argv")), "-n\nns\n-l\napp=svc\n") {
				t.Errorf("argv = %q, want -n ns -l app=svc", readFile(t, filepath.Join(dir, "argv")))
			}
		})
	}
}
