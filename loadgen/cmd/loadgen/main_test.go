package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// validArgs returns a full set of flags, sending to targetURL/{id}: a
// window of 60 sends at 1/s, and a burst of 30.
func validArgs(targetURL string) []string {
	return []string{
		"-rate", "1", "-window", "60s", "-burst-rate", "1",
		"-target-url", targetURL + "/{id}", "-canary-addr", "127.0.0.1:1",
	}
}

// useListener makes run listen on a new loopback listener, and returns
// its base URL.
func useListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	old := listen
	listen = func() (net.Listener, error) { return ln, nil }
	t.Cleanup(func() { listen = old; ln.Close() })
	return "http://" + ln.Addr().String()
}

// lockedBuffer is a bytes.Buffer safe for concurrent use: run writes its
// log while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// startRun runs run in its own goroutine, with env as its environment
// (useEnv), and returns its stderr, a cancel for its ctx, and a channel
// that gets its error.
func startRun(t *testing.T, env map[string]string, args []string) (*lockedBuffer, context.CancelFunc, <-chan error) {
	t.Helper()
	useEnv(t, env)
	stderr := new(lockedBuffer)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, stderr) }()
	return stderr, cancel, done
}

// waitRun returns run's error, failing the test if run has not returned
// within shutdownTimeout and some slack.
func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(shutdownTimeout + 5*time.Second):
		t.Fatal("run did not return")
		return nil
	}
}

// client has no keep-alives, so that every request dials.
var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}

// do sends one request and returns its body, failing the test unless the
// status is want.
func do(t *testing.T, method, url, body string, want int) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, url, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d %s, want %d", method, url, resp.StatusCode, b, want)
	}
	return resp, b
}

// TestRunFlagErrors checks that each missing flag, and each bad value,
// makes run print the error and return it before it listens.
func TestRunFlagErrors(t *testing.T) {
	old := listen
	listen = func() (net.Listener, error) {
		t.Error("run listened")
		return nil, errors.New("this test must not listen")
	}
	t.Cleanup(func() { listen = old })
	useEnv(t, map[string]string{})

	full := validArgs("http://127.0.0.1:1")
	type flagCase struct {
		name, want string
		args       []string
	}
	var cases []flagCase
	for i := 0; i < len(full); i += 2 {
		name := strings.TrimPrefix(full[i], "-")
		args := slices.Concat(full[:i], full[i+2:])
		cases = append(cases, flagCase{"missing " + name, "flag -" + name + " is required", args})
	}
	// A later value of a flag overrides an earlier one.
	with := func(extra ...string) []string { return slices.Concat(full, extra) }
	cases = append(cases,
		flagCase{"no flags", "flag -rate is required", nil},
		flagCase{"rate not a number", `invalid value "abc" for flag -rate`, with("-rate", "abc")},
		flagCase{"rate zero", "rate 0 and window 1m0s must both be positive", with("-rate", "0")},
		flagCase{"rate over maxRate", "over the 19 the memory sum covers", with("-rate", strconv.FormatFloat(math.Nextafter(19, math.Inf(1)), 'g', -1, 64))},
		flagCase{"window negative", "rate 1 and window -1s must both be positive", with("-window", "-1s")},
		flagCase{"window not a duration", `invalid value "60" for flag -window`, with("-window", "60")},
		flagCase{"burst too large", "over the 3000 the memory sum covers", with("-burst-rate", "1000")},
		flagCase{"burst rate NaN", "want a positive finite rate", with("-burst-rate", "NaN")},
		flagCase{"target URL without id", "holds 0 {id}", with("-target-url", "http://127.0.0.1:1")},
		flagCase{"canary address a name", "want an IP literal host:port", with("-canary-addr", "localhost:1")},
		flagCase{"unknown flag", "flag provided but not defined: -addr", with("-addr", ":1")},
		flagCase{"extra argument", `unexpected argument "x"`, with("x")},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if err := run(t.Context(), tc.args, &stderr); err == nil {
				t.Fatal("run = nil, want an error")
			}
			if out := stderr.String(); !strings.Contains(out, tc.want) || !strings.Contains(out, "Usage of loadgen") {
				t.Errorf("stderr %q, want %q and the usage in it", out, tc.want)
			}
		})
	}
}

// TestRunServes checks that run serves the control API on its listener,
// logs one start-up line and one shutdown line, and returns nil once ctx
// ends and Shutdown has closed the listener.
func TestRunServes(t *testing.T) {
	url := useListener(t)
	stderr, cancel, done := startRun(t, map[string]string{}, validArgs("http://127.0.0.1:1"))

	resp, body := do(t, "GET", url+"/config", "", http.StatusOK)
	var cfg struct {
		BootID      string `json:"boot_id"`
		WindowSends int    `json:"window_sends"`
		BurstSends  int    `json:"burst_sends"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if cfg.WindowSends != 60 || cfg.BurstSends != 30 || cfg.BootID == "" || resp.Header.Get("Ek8s-Boot-Id") != cfg.BootID {
		t.Errorf("GET /config = %s, Ek8s-Boot-Id %q; want window_sends 60, burst_sends 30 and one boot_id", body, resp.Header.Get("Ek8s-Boot-Id"))
	}

	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if resp, err := client.Get(url + "/config"); err == nil {
		resp.Body.Close()
		t.Error("GET /config after run returned: answered, want the listener closed")
	}

	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	for _, l := range lines {
		t.Log(l)
	}
	if len(lines) != 2 ||
		!strings.Contains(lines[0], `msg="loadgen starting"`) || !strings.Contains(lines[0], "boot_id="+cfg.BootID) ||
		!strings.Contains(lines[0], "target_url=http://127.0.0.1:1/{id}") ||
		!strings.Contains(lines[1], `msg="loadgen stopped"`) {
		t.Errorf("log %q, want a start-up line with boot_id=%s and the config, then a shutdown line", lines, cfg.BootID)
	}
}

// TestRunCancelsRuns checks that ctx's end cancels a run in progress: run
// does not return while a send is in flight, returns nil once that send
// is answered and recorded, and does so long before the 60 s window
// would end.
func TestRunCancelsRuns(t *testing.T) {
	var seen atomic.Int64
	hold := make(chan struct{}) // the first send is answered only once closed
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	variant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if seen.Add(1) == 1 {
			<-hold
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer variant.Close()
	defer release() // before Close, which waits for the held handler
	url := useListener(t)
	_, cancel, done := startRun(t, map[string]string{}, validArgs(variant.URL))

	_, body := do(t, "GET", url+"/config", "", http.StatusOK)
	var cfg struct {
		WindowPairs int `json:"window_pairs"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	pairs := make([]string, cfg.WindowPairs)
	for i := range pairs {
		pairs[i] = fmt.Sprintf(`{"password":"p%d","hash":"h","match":%t}`, i, i%2 == 0)
	}
	do(t, "PUT", url+"/targets/1", "", http.StatusCreated)
	do(t, "PUT", url+"/targets/1/pairs/w1", `{"pairs":[`+strings.Join(pairs, ",")+`]}`, http.StatusCreated)
	do(t, "POST", url+"/runs/w1/start", `{"kind":"window","targets":["1"]}`, http.StatusOK)
	for deadline := time.Now().Add(5 * time.Second); seen.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a send")
		}
	}

	// The first send is now held at the variant. A send outlives its
	// run's context, so run must wait for it (well under the sender's
	// 7 s client timeout).
	cancel()
	select {
	case err := <-done:
		t.Fatalf("run returned %v while a send was in flight, want it to wait for the send", err)
	case <-time.After(time.Second):
	}
	t0 := time.Now()
	release()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("run returned %v after the held send was answered, want well under the window's 60 s", d)
	}
	n := seen.Load()
	time.Sleep(1500 * time.Millisecond) // past the next send's time, at 1/s
	if got := seen.Load(); got != n {
		t.Errorf("the variant got %d sends after run returned, want none", got-n)
	}
}

// useEnv makes run read env as its environment.
func useEnv(t *testing.T, env map[string]string) {
	t.Helper()
	old := lookupEnv
	lookupEnv = lookupFrom(env)
	t.Cleanup(func() { lookupEnv = old })
}

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestParseLimit checks EK8S_MEM_LIMIT_BYTES's parsing, with
// cmd/service's cases.
func TestParseLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    int64
		wantErr bool
	}{
		{"absent", map[string]string{}, 0, false},
		{"one", map[string]string{memLimitEnv: "1"}, 1, false},
		{"valid", map[string]string{memLimitEnv: "268435456"}, 268435456, false},
		{"max int64", map[string]string{memLimitEnv: strconv.FormatInt(math.MaxInt64, 10)}, math.MaxInt64, false},
		{"zero", map[string]string{memLimitEnv: "0"}, 0, true},
		{"negative", map[string]string{memLimitEnv: "-1"}, 0, true},
		{"non-numeric", map[string]string{memLimitEnv: "abc"}, 0, true},
		{"unit suffix", map[string]string{memLimitEnv: "256Mi"}, 0, true},
		{"present but empty", map[string]string{memLimitEnv: ""}, 0, true},
		{"huge overflows int64", map[string]string{memLimitEnv: "99999999999999999999"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimit(lookupFrom(tc.env), memLimitEnv)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), memLimitEnv) {
					t.Fatalf("err = %v, want one naming %s", err, memLimitEnv)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseLimit = (%d, %v), want (%d, nil)", got, err, tc.want)
			}
		})
	}
}

func TestGoMemLimit(t *testing.T) {
	if got := goMemLimit(1000); got != 900 {
		t.Fatalf("goMemLimit(1000) = %d, want 900", got)
	}
	if got := goMemLimit(math.MaxInt64); got <= 0 || got >= math.MaxInt64 {
		t.Fatalf("goMemLimit(MaxInt64) = %d, want 90%% without overflow", got)
	}
}

// TestRunMemLimit checks that run sets the Go memory limit to 90% of
// EK8S_MEM_LIMIT_BYTES, leaves it alone when the variable is absent, and
// logs both values on its start-up line.
func TestRunMemLimit(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"absent", map[string]string{}, "mem_limit_bytes=0 go_mem_limit=" + strconv.FormatInt(old, 10)},
		{"256 MiB", map[string]string{memLimitEnv: "268435456"}, "mem_limit_bytes=268435456 go_mem_limit=241591911"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			debug.SetMemoryLimit(old)
			url := useListener(t)
			stderr, cancel, done := startRun(t, tc.env, validArgs("http://127.0.0.1:1"))
			do(t, "GET", url+"/config", "", http.StatusOK)
			cancel()
			if err := waitRun(t, done); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
			first, _, _ := strings.Cut(stderr.String(), "\n")
			if !strings.Contains(first, `msg="loadgen starting"`) || !strings.Contains(first, tc.want) {
				t.Errorf("start-up line %q, want %q in it", first, tc.want)
			}
		})
	}
}

// TestRunEnvError checks that a bad EK8S_MEM_LIMIT_BYTES makes run print
// the error and return it before it listens or changes the limit.
func TestRunEnvError(t *testing.T) {
	old := listen
	listen = func() (net.Listener, error) {
		t.Error("run listened")
		return nil, errors.New("this test must not listen")
	}
	t.Cleanup(func() { listen = old })
	useEnv(t, map[string]string{memLimitEnv: "256Mi"})
	limit := debug.SetMemoryLimit(-1)

	var stderr bytes.Buffer
	if err := run(t.Context(), validArgs("http://127.0.0.1:1"), &stderr); err == nil {
		t.Fatal("run = nil, want an error")
	}
	if out := stderr.String(); !strings.Contains(out, memLimitEnv+`="256Mi": must be a positive integer`) {
		t.Errorf("stderr %q, want the %s error in it", out, memLimitEnv)
	}
	if got := debug.SetMemoryLimit(-1); got != limit {
		t.Errorf("Go memory limit %d after run, want %d unchanged", got, limit)
	}
}
