package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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

// startRun runs run in its own goroutine and returns its stderr, a
// cancel for its ctx, and a channel that gets its error.
func startRun(t *testing.T, args []string) (*lockedBuffer, context.CancelFunc, <-chan error) {
	t.Helper()
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
	stderr, cancel, done := startRun(t, validArgs("http://127.0.0.1:1"))

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
// returns nil, after its in-flight sends are recorded, long before the
// 60 s window would end.
func TestRunCancelsRuns(t *testing.T) {
	var seen atomic.Int64
	variant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer variant.Close()
	url := useListener(t)
	_, cancel, done := startRun(t, validArgs(variant.URL))

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

	t0 := time.Now()
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("run returned %v after ctx ended, want well under the window's 60 s", d)
	}
	n := seen.Load()
	time.Sleep(1500 * time.Millisecond) // past the next send's time, at 1/s
	if got := seen.Load(); got != n {
		t.Errorf("the variant got %d sends after run returned, want none", got-n)
	}
}
