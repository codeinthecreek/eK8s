package client_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/wrapper"
	"github.com/codeinthecreek/eK8s/loadgen"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// serveVariant serves a fake variant 1 at url/1: a gen-0 wrapper.Server
// around core, at /hash and /verify, as loadgen's e2e tests do. It
// returns a Config sending to it: a window of 5 sends, and a burst of 30
// at 1/s (the burst's length is loadgen's const D, 30 s).
func serveVariant(t *testing.T, core wrapper.Hasher) loadgen.Config {
	t.Helper()
	srv := wrapper.New(core, wrapper.Env{})
	vm := http.NewServeMux()
	vm.Handle("POST /hash", srv.HashHandler())
	vm.Handle("POST /verify", srv.VerifyHandler())
	mux := http.NewServeMux()
	mux.Handle("/1/", http.StripPrefix("/1", vm))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return loadgen.Config{Rate: 5, Window: time.Second, BurstRate: 1, TargetURL: ts.URL + "/{id}", CanaryAddr: "127.0.0.1:1"}
}

func newControl(t *testing.T, cfg loadgen.Config) *loadgen.Control {
	t.Helper()
	c, err := loadgen.NewControl(t.Context(), cfg, loadgen.NewSender())
	if err != nil {
		t.Fatalf("NewControl: %v", err)
	}
	return c
}

// hashPairs returns n /verify pairs made with core, as the host makes
// them: the even ones match, and each odd one pairs a password with the
// hash of another.
func hashPairs(t *testing.T, core *hashcore.Core, n int) []wire.UploadPair {
	t.Helper()
	ps := make([]wire.UploadPair, n)
	work := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for i := range work {
				pw, hashed, match := rand.Text(), rand.Text(), i%2 == 0
				if match {
					hashed = pw
				}
				h, err := core.Hash(hashed)
				if err != nil {
					t.Error(err)
				}
				ps[i] = wire.UploadPair{Password: &pw, Hash: &h, Match: &match}
			}
		})
	}
	for i := range n {
		work <- i
	}
	close(work)
	wg.Wait()
	return ps
}

// apiError returns err's *client.APIError, failing the test if it has
// none.
func apiError(t *testing.T, err error) *client.APIError {
	t.Helper()
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v, want a *client.APIError", err)
	}
	return ae
}

// TestRealServer drives every method against loadgen's own Handler,
// through a full burst cycle to a gen-0 variant with the real hashcore.
func TestRealServer(t *testing.T) {
	t.Parallel()
	core := hashcore.New()
	ctrl := newControl(t, serveVariant(t, core))
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)
	c := client.New(srv.URL+"/", srv.Client())
	ctx := t.Context()

	cfg, err := c.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.BootID != ctrl.BootID() || cfg.BurstSends != 30 {
		t.Fatalf("Config = %+v, want boot_id %s, burst_sends 30", cfg, ctrl.BootID())
	}
	boot := cfg.BootID
	c.Expect(boot)

	if ts, err := c.Targets(ctx); err != nil || ts.BootID != boot || len(ts.Targets) != 0 {
		t.Fatalf("Targets = %+v, %v; want none", ts, err)
	}
	for _, wantCreated := range []bool{true, false} {
		pt, created, err := c.PutTarget(ctx, "1")
		if err != nil || created != wantCreated || pt != (wire.PutTarget{BootID: boot, ID: "1", Next: 0}) {
			t.Fatalf("PutTarget = %+v, %v, %v; want next 0, created %v", pt, created, err, wantCreated)
		}
	}
	// One escaped segment: loadgen reads the id "1/2", and refuses it as
	// not a variant id, rather than routing it as two segments.
	if _, _, err := c.PutTarget(ctx, "1/2"); apiError(t, err).Status != http.StatusBadRequest {
		t.Errorf("PutTarget(1/2) = %v, want a 400", err)
	}
	if ts, err := c.Targets(ctx); err != nil || len(ts.Targets) != 1 || ts.Targets[0] != (wire.Target{ID: "1", Next: 0}) {
		t.Fatalf("Targets = %+v, %v; want target 1 only", ts, err)
	}

	up, err := c.PutPairs(ctx, "1", "b1", wire.Upload{Pairs: hashPairs(t, core, cfg.BurstPairs)})
	if err != nil || up != (wire.Uploaded{BootID: boot, Count: cfg.BurstPairs}) {
		t.Fatalf("PutPairs = %+v, %v; want count %d", up, err, cfg.BurstPairs)
	}
	st, err := c.Start(ctx, "b1", wire.Start{Kind: wire.KindBurst, Targets: []string{"1"}})
	if err != nil || st.BootID != boot || st.RunID != "b1" || st.StartWall.IsZero() {
		t.Fatalf("Start = %+v, %v", st, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		run, err := c.Run(ctx, "b1")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if s := run.Targets["1"]; len(run.Targets) != 1 || s.State != wire.StateRunning {
			if s.State != wire.StateDone || run.Kind != wire.KindBurst || !run.StartWall.Equal(st.StartWall) {
				t.Fatalf("Run = %+v, want burst b1 done", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run b1 still running after 2 min")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Cancelling a run that has ended changes nothing.
	if run, err := c.Cancel(ctx, "b1"); err != nil || run.Targets["1"].State != wire.StateDone {
		t.Fatalf("Cancel = %+v, %v; want b1 done", run, err)
	}
	_, err = c.Run(ctx, "nope")
	if ae := apiError(t, err); *ae != (client.APIError{Status: http.StatusNotFound, BootID: boot, Message: `loadgen: no run "nope"`}) {
		t.Errorf("Run(nope) = %+v", ae)
	}

	var recs []wire.Record
	since := uint64(0)
	for {
		p, err := c.Records(ctx, "1", since, 7)
		if err != nil {
			t.Fatalf("Records since %d: %v", since, err)
		}
		if p.Gap || len(p.Records) > 7 {
			t.Fatalf("page since %d: gap %v, %d records", since, p.Gap, len(p.Records))
		}
		for i, r := range p.Records {
			if r.Seq != since+uint64(i) || r.Target != "1" || r.RunID != "b1" || r.Skipped || r.Fault != "" || r.Evidence != nil {
				t.Fatalf("page since %d: record %d = %+v", since, i, r)
			}
		}
		if len(p.Records) == 0 {
			if p.Next != since {
				t.Fatalf("empty page since %d has next %d", since, p.Next)
			}
			break
		}
		recs = append(recs, p.Records...)
		since = p.Next
	}
	if len(recs) != cfg.BurstSends {
		t.Fatalf("%d records, want %d", len(recs), cfg.BurstSends)
	}

	last := recs[len(recs)-1].Seq
	_, err = c.DeleteTarget(ctx, "1", last-1)
	if ae := apiError(t, err); ae.Status != http.StatusConflict || ae.BootID != boot || !strings.Contains(ae.Message, "unread records") {
		t.Errorf("DeleteTarget through %d = %+v, want a 409 for unread records", last-1, ae)
	}
	if v, err := c.DeleteTarget(ctx, "1", last); err != nil || v.BootID != boot {
		t.Fatalf("DeleteTarget through %d = %+v, %v", last, v, err)
	}
	if ts, err := c.Targets(ctx); err != nil || len(ts.Targets) != 0 {
		t.Fatalf("Targets after DeleteTarget = %+v, %v; want none", ts, err)
	}

	if v, err := c.Canary(ctx); err != nil || v != (wire.Canary{BootID: boot, Result: wire.CanaryRefused}) {
		t.Errorf("Canary = %+v, %v; want refused", v, err)
	}
}

// TestBootChanged puts a second Control behind the same URL after
// Expect: every response from it, success or error, is ErrBootChanged,
// and the first Control's are accepted again once it is back.
func TestBootChanged(t *testing.T) {
	t.Parallel()
	cfg := serveVariant(t, hashcore.New())
	a, b := newControl(t, cfg), newControl(t, cfg)
	ha, hb := a.Handler(), b.Handler()
	var h atomic.Pointer[http.Handler]
	h.Store(&ha)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*h.Load()).ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, srv.Client())
	ctx := t.Context()

	v, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Expect(v.BootID)
	if _, _, err := c.PutTarget(ctx, "1"); err != nil {
		t.Fatal(err)
	}

	h.Store(&hb)
	if _, err := c.Config(ctx); !errors.Is(err, client.ErrBootChanged) {
		t.Errorf("Config from the second Control: %v, want ErrBootChanged", err)
	}
	_, err = c.Records(ctx, "1", 0, 10)
	if !errors.Is(err, client.ErrBootChanged) {
		t.Errorf("Records from the second Control: %v, want ErrBootChanged", err)
	}
	if ae := apiError(t, err); ae.Status != http.StatusNotFound || ae.BootID != b.BootID() {
		t.Errorf("Records from the second Control: %+v, want its 404", ae)
	}

	h.Store(&ha)
	if _, err := c.Records(ctx, "1", 0, 10); err != nil {
		t.Errorf("Records from the first Control again: %v", err)
	}
}

// fakeResponse is one response a fake server sends.
type fakeResponse struct {
	status int
	header http.Header
	body   string
}

// fake returns a Client for a server that answers every request with r.
func fake(t *testing.T, r fakeResponse) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range r.header {
			w.Header()[k] = vs
		}
		w.WriteHeader(r.status)
		io.WriteString(w, r.body)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, srv.Client())
}

// headers returns a response header with Content-Type ct, unless it is
// "", and boot_id header values boots.
func headers(ct string, boots ...string) http.Header {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	for _, b := range boots {
		h.Add(wire.BootIDHeader, b)
	}
	return h
}

// TestFakeResponses checks each check of a response, with Canary
// against fake servers.
func TestFakeResponses(t *testing.T) {
	const (
		json   = "application/json"
		canary = `{"boot_id":"B","result":"refused"}`
	)
	pad := func(n int) string { return canary + strings.Repeat(" ", n-len(canary)) }
	for _, tc := range []struct {
		name    string
		r       fakeResponse
		wantErr string           // a substring of the error, "" for none
		wantAPI *client.APIError // the error's *client.APIError, nil for none
	}{
		{"ok", fakeResponse{200, headers(json, "B"), canary + "\n"}, "", nil},
		{"ok_at_cap", fakeResponse{200, headers(json, "B"), pad(client.MaxResponseBody)}, "", nil},
		{"over_cap", fakeResponse{200, headers(json, "B"), pad(client.MaxResponseBody + 1)}, "body over", nil},
		{"boot_ids_disagree", fakeResponse{200, headers(json, "A"), canary}, `body boot_id "B", but Ek8s-Boot-Id "A"`, nil},
		{"header_missing", fakeResponse{200, headers(json), canary}, `Ek8s-Boot-Id [], want one boot_id`, nil},
		{"header_empty", fakeResponse{200, headers(json, ""), canary}, `want one boot_id`, nil},
		{"header_twice", fakeResponse{200, headers(json, "B", "B"), canary}, `want one boot_id`, nil},
		{"body_boot_missing", fakeResponse{200, headers(json, "B"), `{"result":"refused"}`}, `body boot_id ""`, nil},
		{"unknown_field", fakeResponse{200, headers(json, "B"), `{"boot_id":"B","result":"refused","extra":1}`}, `unknown field "extra"`, nil},
		{"trailing_value", fakeResponse{200, headers(json, "B"), canary + "{}"}, "data after the JSON value", nil},
		{"trailing_junk", fakeResponse{200, headers(json, "B"), canary + "x"}, "invalid character", nil},
		{"two_values", fakeResponse{200, headers(json, "B"), canary + canary}, "data after the JSON value", nil},
		{"empty_body", fakeResponse{200, headers(json, "B"), ""}, "EOF", nil},
		{"text_plain", fakeResponse{200, headers("text/plain", "B"), canary}, "Content-Type", nil},
		{"json_charset", fakeResponse{200, headers("application/json; charset=utf-8", "B"), canary}, "Content-Type", nil},
		{"no_content_type", fakeResponse{200, headers("", "B"), canary}, "Content-Type", nil},
		{"unexpected_2xx", fakeResponse{201, headers(json, "B"), canary}, "status 201",
			&client.APIError{Status: 201, BootID: "B"}},
		{"error_body", fakeResponse{409, headers(json, "B"), `{"boot_id":"B","error":"loadgen: x"}` + "\n"}, "status 409: loadgen: x",
			&client.APIError{Status: 409, BootID: "B", Message: "loadgen: x"}},
		{"error_body_other_boot", fakeResponse{409, headers(json, "B"), `{"boot_id":"A","error":"loadgen: x"}`}, "status 409",
			&client.APIError{Status: 409, BootID: "B"}},
		{"error_body_extra_field", fakeResponse{409, headers(json, "B"), `{"boot_id":"B","error":"loadgen: x","x":1}`}, "status 409",
			&client.APIError{Status: 409, BootID: "B"}},
		{"error_text", fakeResponse{404, headers("text/plain; charset=utf-8", "B"), "404 page not found\n"}, "status 404",
			&client.APIError{Status: 404, BootID: "B"}},
		{"error_not_loadgen", fakeResponse{503, headers("text/plain"), "no endpoints available\n"}, "status 503",
			&client.APIError{Status: 503}},
		{"error_json_no_header", fakeResponse{500, headers(json), `{"boot_id":"B","error":"loadgen: x"}`}, "status 500",
			&client.APIError{Status: 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := fake(t, tc.r).Canary(t.Context())
			if tc.wantErr == "" {
				if err != nil || v != (wire.Canary{BootID: "B", Result: wire.CanaryRefused}) {
					t.Fatalf("Canary = %+v, %v; want B refused", v, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Canary = %+v, %v; want an error containing %q", v, err, tc.wantErr)
			}
			if v != (wire.Canary{}) {
				t.Errorf("Canary returned %+v with its error, want the zero value", v)
			}
			if errors.Is(err, client.ErrBootChanged) {
				t.Errorf("error %v matches ErrBootChanged without Expect", err)
			}
			var ae *client.APIError
			switch {
			case tc.wantAPI == nil && errors.As(err, &ae):
				t.Errorf("error %v carries %+v, want no APIError", err, ae)
			case tc.wantAPI != nil && *apiError(t, err) != *tc.wantAPI:
				t.Errorf("APIError = %+v, want %+v", *apiError(t, err), *tc.wantAPI)
			}
		})
	}
}

// TestExpect checks Expect against fake servers: a boot_id other than
// the expected one is ErrBootChanged on success and error statuses
// alike, and on an error status it also carries the APIError; after
// ExpectAny any boot_id is accepted again.
func TestExpect(t *testing.T) {
	const json = "application/json"
	ok := fakeResponse{200, headers(json, "B"), `{"boot_id":"B","result":"refused"}`}
	notFound := fakeResponse{404, headers(json, "B"), `{"boot_id":"B","error":"loadgen: no run \"r\""}`}

	c := fake(t, ok)
	c.Expect("B")
	if _, err := c.Canary(t.Context()); err != nil {
		t.Errorf("Canary with the expected boot_id: %v", err)
	}
	c.Expect("A")
	if _, err := c.Canary(t.Context()); !errors.Is(err, client.ErrBootChanged) || !strings.Contains(err.Error(), "got B, want A") {
		t.Errorf("Canary with another boot_id: %v, want ErrBootChanged", err)
	}
	c.Expect("")
	if _, err := c.Canary(t.Context()); !errors.Is(err, client.ErrBootChanged) {
		t.Errorf("Canary after Expect(\"\"): %v, want ErrBootChanged", err)
	}

	c.ExpectAny()
	if _, err := c.Canary(t.Context()); err != nil {
		t.Errorf("Canary after ExpectAny: %v", err)
	}

	c = fake(t, notFound)
	c.Expect("A")
	_, err := c.Run(t.Context(), "r")
	if !errors.Is(err, client.ErrBootChanged) {
		t.Errorf("Run with another boot_id: %v, want ErrBootChanged", err)
	}
	if ae := apiError(t, err); *ae != (client.APIError{Status: 404, BootID: "B", Message: `loadgen: no run "r"`}) {
		t.Errorf("Run with another boot_id: %+v", ae)
	}
}

// TestEchoes checks that a response for another id or run_id than the
// request's is an error.
func TestEchoes(t *testing.T) {
	h := headers("application/json", "B")
	ctx := t.Context()
	if _, _, err := fake(t, fakeResponse{201, h, `{"boot_id":"B","id":"2","next":0}`}).PutTarget(ctx, "1"); err == nil || !strings.Contains(err.Error(), `answered for id "2"`) {
		t.Errorf("PutTarget(1) answered for 2: %v", err)
	}
	if _, err := fake(t, fakeResponse{200, h, `{"boot_id":"B","run_id":"r2","start_wall":"2026-10-18T02:04:05Z"}`}).Start(ctx, "r1", wire.Start{}); err == nil || !strings.Contains(err.Error(), `answered for run_id "r2"`) {
		t.Errorf("Start(r1) answered for r2: %v", err)
	}
	run := fakeResponse{200, h, `{"boot_id":"B","run_id":"r2","kind":"burst","start_wall":"2026-10-18T02:04:05Z","targets":{}}`}
	if _, err := fake(t, run).Run(ctx, "r1"); err == nil || !strings.Contains(err.Error(), `answered for run_id "r2"`) {
		t.Errorf("Run(r1) answered for r2: %v", err)
	}
	if _, err := fake(t, run).Cancel(ctx, "r1"); err == nil || !strings.Contains(err.Error(), `answered for run_id "r2"`) {
		t.Errorf("Cancel(r1) answered for r2: %v", err)
	}
}

// TestRequests checks what each method sends: its method, its escaped
// path and query, and a body of compact JSON.
func TestRequests(t *testing.T) {
	type sent struct{ method, uri, contentType, body string }
	got := make(chan sent, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- sent{r.Method, r.RequestURI, r.Header.Get("Content-Type"), string(b)}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL+"/base/", srv.Client())
	ctx := t.Context()
	pw, hash, match := "p", "h", true
	const ct = "application/json"
	for _, tc := range []struct {
		call func() error
		want sent
	}{
		{func() error { _, err := c.Config(ctx); return err }, sent{"GET", "/base/config", "", ""}},
		{func() error { _, err := c.Targets(ctx); return err }, sent{"GET", "/base/targets", "", ""}},
		{func() error { _, _, err := c.PutTarget(ctx, "1/2"); return err }, sent{"PUT", "/base/targets/1%2F2", "", ""}},
		{func() error { _, err := c.DeleteTarget(ctx, "1", 18446744073709551615); return err },
			sent{"DELETE", "/base/targets/1?through=18446744073709551615", "", ""}},
		{func() error { _, err := c.Records(ctx, "1", 5, 7); return err }, sent{"GET", "/base/targets/1/records?limit=7&since=5", "", ""}},
		{func() error {
			_, err := c.PutPairs(ctx, "1", "r 1", wire.Upload{Pairs: []wire.UploadPair{{Password: &pw, Hash: &hash, Match: &match}}})
			return err
		}, sent{"PUT", "/base/targets/1/pairs/r%201", ct, `{"pairs":[{"password":"p","hash":"h","match":true}]}`}},
		{func() error {
			_, err := c.Start(ctx, "b?1", wire.Start{Kind: wire.KindBurst, Targets: []string{"1"}})
			return err
		}, sent{"POST", "/base/runs/b%3F1/start", ct, `{"kind":"burst","targets":["1"]}`}},
		{func() error { _, err := c.Run(ctx, "b1"); return err }, sent{"GET", "/base/runs/b1", "", ""}},
		{func() error { _, err := c.Cancel(ctx, "b1"); return err }, sent{"POST", "/base/runs/b1/cancel", "", ""}},
		{func() error { _, err := c.Canary(ctx); return err }, sent{"POST", "/base/canary", "", ""}},
	} {
		err := tc.call()
		if ae := apiError(t, err); ae.Status != http.StatusTeapot {
			t.Errorf("%s %s: %v, want a 418 APIError", tc.want.method, tc.want.uri, err)
		}
		if s := <-got; s != tc.want {
			t.Errorf("sent %+v, want %+v", s, tc.want)
		}
	}
}

// TestContextCancelled checks that a request whose context is cancelled
// while loadgen has not answered returns promptly, with the context's
// error wrapped.
func TestContextCancelled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := client.New(srv.URL, srv.Client()).Config(ctx)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Config returned after %v, want promptly after the cancel at 100ms", d)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Config: %v, want context.Canceled wrapped", err)
	}
}
