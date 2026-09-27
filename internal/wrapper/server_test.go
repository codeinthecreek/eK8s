package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHasher counts calls and, when gate is set, blocks each call until
// the test sends on gate. entered receives once per call as it starts.
type fakeHasher struct {
	hashCalls, verifyCalls atomic.Int32
	entered                chan struct{}
	gate                   chan struct{}
	hashErr, verifyErr     error
	match                  bool
}

func (f *fakeHasher) wait() {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
}

func (f *fakeHasher) Hash(string) (string, error) {
	f.hashCalls.Add(1)
	f.wait()
	if f.hashErr != nil {
		return "", f.hashErr
	}
	return "fake-hash", nil
}

func (f *fakeHasher) Verify(string, string) (bool, error) {
	f.verifyCalls.Add(1)
	f.wait()
	return f.match, f.verifyErr
}

// oneSlot sizes the semaphore to exactly 1 regardless of GOMAXPROCS.
var oneSlot = Env{MemoryLimitBytes: 2 * perHashMemory}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

// decodeBody asserts a JSON response and returns its fields.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", rec.Body.String(), err)
	}
	return m
}

func TestCapacity(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name       string
		gomaxprocs int
		memLimit   int64
		want       int
	}{
		{"memory unknown uses GOMAXPROCS", 8, 0, 8},
		{"memory unknown, one CPU", 1, 0, 1},
		{"large memory, CPU-bound", 8, 4 << 30, 8},
		{"memory-bound", 8, 100 * mib, 2}, // 50 MiB budget / 20 MiB
		{"budget fits exactly one", 8, 40 * mib, 1},
		{"budget below one call floors at 1", 8, 10 * mib, 1},
		{"CPU-bound under memory", 2, 1 << 30, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capacity(tc.gomaxprocs, tc.memLimit); got != tc.want {
				t.Fatalf("capacity(%d, %d) = %d, want %d", tc.gomaxprocs, tc.memLimit, got, tc.want)
			}
		})
	}
}

func TestHashOK(t *testing.T) {
	for name, body := range map[string]string{
		"plain":          `{"password":"pw"}`,
		"unknown field":  `{"password":"pw","extra":1}`,
		"max length":     `{"password":"` + strings.Repeat("a", maxPasswordBytes) + `"}`,
		"trailing space": "{\"password\":\"pw\"}\n ",
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, New(&fakeHasher{}, Env{}).HashHandler(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			m := decodeBody(t, rec)
			if len(m) != 1 || m["hash"] != "fake-hash" {
				t.Fatalf("body = %v, want exactly {\"hash\": \"fake-hash\"}", m)
			}
		})
	}
}

func TestVerifyMatchAndMismatch(t *testing.T) {
	for _, match := range []bool{true, false} {
		rec := post(t, New(&fakeHasher{match: match}, Env{}).VerifyHandler(), `{"password":"pw","hash":"h"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("match=%v: status = %d, want 200", match, rec.Code)
		}
		m := decodeBody(t, rec)
		if len(m) != 1 || m["match"] != match {
			t.Fatalf("body = %v, want exactly {\"match\": %v}", m, match)
		}
	}
}

func TestVerifyErrorIs400(t *testing.T) {
	f := &fakeHasher{match: true, verifyErr: errors.New("unsupported params")}
	rec := post(t, New(f, Env{}).VerifyHandler(), `{"password":"pw","hash":"h"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if _, ok := decodeBody(t, rec)["match"]; ok {
		t.Fatal("a Verify error must never produce a match field")
	}
}

func TestHashErrorIs500WithoutRetryAfter(t *testing.T) {
	rec := post(t, New(&fakeHasher{hashErr: errors.New("no randomness")}, Env{}).HashHandler(), `{"password":"pw"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After = %q on a 500, want none", ra)
	}
	decodeBody(t, rec)
}

func TestMalformedInputIs400AndNeverCallsHasher(t *testing.T) {
	long := strings.Repeat("a", maxPasswordBytes+1)
	cases := []struct {
		name, endpoint, body string
	}{
		{"invalid JSON", "hash", `{"password":`},
		{"empty body", "hash", ``},
		{"array", "hash", `[]`},
		{"null", "hash", `null`},
		{"password wrong type", "hash", `{"password":1}`},
		{"trailing object", "hash", `{"password":"pw"}{}`},
		{"trailing garbage", "hash", `{"password":"pw"} x`},
		{"missing password", "hash", `{}`},
		{"empty password", "hash", `{"password":""}`},
		{"password too long", "hash", `{"password":"` + long + `"}`},
		{"verify invalid JSON", "verify", `{`},
		{"verify trailing data", "verify", `{"password":"pw","hash":"h"}]`},
		{"verify missing password", "verify", `{"hash":"h"}`},
		{"verify empty password", "verify", `{"password":"","hash":"h"}`},
		{"verify password too long", "verify", `{"password":"` + long + `","hash":"h"}`},
		{"verify missing hash", "verify", `{"password":"pw"}`},
		{"verify empty hash", "verify", `{"password":"pw","hash":""}`},
		{"verify hash wrong type", "verify", `{"password":"pw","hash":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHasher{match: true}
			s := New(f, Env{})
			h := s.HashHandler()
			if tc.endpoint == "verify" {
				h = s.VerifyHandler()
			}
			rec := post(t, h, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if _, ok := decodeBody(t, rec)["error"]; !ok {
				t.Fatal("400 body lacks an error field")
			}
			if n := f.hashCalls.Load() + f.verifyCalls.Load(); n != 0 {
				t.Fatalf("Hasher called %d times, want 0", n)
			}
		})
	}
}

func TestOversizedBodyIs400(t *testing.T) {
	f := &fakeHasher{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"`+strings.Repeat("a", 100)+`"}`))
	req.Body = http.MaxBytesReader(rec, req.Body, 16) // as cmd/service would, smaller
	New(f, Env{}).HashHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if f.hashCalls.Load() != 0 {
		t.Fatal("Hasher called for an oversized body")
	}
}

// saturate starts one request that holds the only slot, blocked inside
// the fake, and returns a func that releases it and checks it succeeded.
func saturate(t *testing.T, s *Server, f *fakeHasher) (finish func()) {
	t.Helper()
	f.entered = make(chan struct{}, 1)
	f.gate = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		s.HashHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"first"}`)))
		done <- rec
	}()
	<-f.entered // the first request now holds the slot
	return func() {
		f.gate <- struct{}{}
		if rec := <-done; rec.Code != http.StatusOK {
			t.Errorf("first request status = %d, want 200", rec.Code)
		}
	}
}

func TestSaturationSheds503WithRetryAfter(t *testing.T) {
	f := &fakeHasher{}
	s := New(f, oneSlot)
	finish := saturate(t, s, f)
	defer finish()

	start := time.Now()
	rec := post(t, s.HashHandler(), `{"password":"second"}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want \"1\"", ra)
	}
	decodeBody(t, rec)
	if elapsed < acquireWait {
		t.Fatalf("shed after %v, want at least the wait budget %v", elapsed, acquireWait)
	}
	if n := f.hashCalls.Load(); n != 1 {
		t.Fatalf("Hasher called %d times, want 1 (the blocked request only)", n)
	}
}

// silentWriter records whether a handler touched the response at all.
type silentWriter struct {
	header  http.Header
	written bool
}

func (w *silentWriter) Header() http.Header         { return w.header }
func (w *silentWriter) Write(b []byte) (int, error) { w.written = true; return len(b), nil }
func (w *silentWriter) WriteHeader(int)             { w.written = true }

func TestCancelledWhileWaitingWritesNothing(t *testing.T) {
	f := &fakeHasher{}
	s := New(f, oneSlot)
	finish := saturate(t, s, f)
	defer finish()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"second"}`)).WithContext(ctx)
	w := &silentWriter{header: http.Header{}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	s.HashHandler().ServeHTTP(w, req)

	if elapsed := time.Since(start); elapsed >= acquireWait {
		t.Fatalf("returned after %v, want before the wait budget %v", elapsed, acquireWait)
	}
	if w.written || len(w.header) != 0 {
		t.Fatalf("handler wrote a response (written=%v, header=%v), want nothing", w.written, w.header)
	}
	if n := f.hashCalls.Load(); n != 1 {
		t.Fatalf("Hasher called %d times, want 1 (the blocked request only)", n)
	}
}

func TestReadyHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	New(&fakeHasher{}, Env{}).ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q, want 200 \"ok\"", rec.Code, rec.Body.String())
	}
}
