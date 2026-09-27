package main

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/wrapper"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr string // substring; "" means no error
	}{
		{"all absent", map[string]string{}, config{variant: "local"}, ""},
		{"valid", map[string]string{"EK8S_VARIANT": "v7", "EK8S_CPU_LIMIT_MILLIS": "500", "EK8S_MEM_LIMIT_BYTES": "268435456"},
			config{variant: "v7", env: wrapper.Env{CPULimitMillis: 500, MemoryLimitBytes: 268435456}}, ""},
		{"empty variant", map[string]string{"EK8S_VARIANT": ""}, config{variant: "local"}, ""},
		{"max int64", map[string]string{"EK8S_MEM_LIMIT_BYTES": strconv.FormatInt(math.MaxInt64, 10)},
			config{variant: "local", env: wrapper.Env{MemoryLimitBytes: math.MaxInt64}}, ""},
		{"cpu zero", map[string]string{"EK8S_CPU_LIMIT_MILLIS": "0"}, config{}, "EK8S_CPU_LIMIT_MILLIS"},
		{"mem zero", map[string]string{"EK8S_MEM_LIMIT_BYTES": "0"}, config{}, "EK8S_MEM_LIMIT_BYTES"},
		{"negative", map[string]string{"EK8S_MEM_LIMIT_BYTES": "-1"}, config{}, "EK8S_MEM_LIMIT_BYTES"},
		{"non-numeric", map[string]string{"EK8S_MEM_LIMIT_BYTES": "abc"}, config{}, "EK8S_MEM_LIMIT_BYTES"},
		{"unit suffix", map[string]string{"EK8S_MEM_LIMIT_BYTES": "256Mi"}, config{}, "EK8S_MEM_LIMIT_BYTES"},
		{"present but empty", map[string]string{"EK8S_CPU_LIMIT_MILLIS": ""}, config{}, "EK8S_CPU_LIMIT_MILLIS"},
		{"huge overflows int64", map[string]string{"EK8S_MEM_LIMIT_BYTES": "99999999999999999999"}, config{}, "EK8S_MEM_LIMIT_BYTES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConfig(lookupFrom(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseConfig = (%+v, %v), want (%+v, nil)", got, err, tc.want)
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

// newTestServer serves the real route table with the real hashcore.
func newTestServer(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var draining atomic.Bool
	ts := httptest.NewServer(newHandler(wrapper.New(hashcore.New(), wrapper.Env{}), "v-test", &draining))
	t.Cleanup(ts.Close)
	return ts, &draining
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get(variantHeader); got != "v-test" {
		t.Errorf("%s %s: %s = %q, want v-test", method, url, variantHeader, got)
	}
	return resp, string(b)
}

func TestEndToEndHashThenVerify(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := do(t, "POST", ts.URL+"/hash", `{"password":"correct horse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/hash: %d %s", resp.StatusCode, body)
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(body), `{"hash":"`), `"}`)
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("/hash body %q lacks the fixed-params prefix", body)
	}
	for pw, want := range map[string]string{"correct horse": `{"match":true}`, "wrong": `{"match":false}`} {
		resp, body := do(t, "POST", ts.URL+"/verify", `{"password":"`+pw+`","hash":"`+hash+`"}`)
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != want {
			t.Fatalf("/verify %q: %d %s, want 200 %s", pw, resp.StatusCode, body, want)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	ts, _ := newTestServer(t)
	big := `{"password":"pw","pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	for _, path := range []string{"/hash", "/verify"} {
		if resp, body := do(t, "POST", ts.URL+path, big); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s with >4 KiB body: %d %s, want 400", path, resp.StatusCode, body)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)
	if resp, _ := do(t, "GET", ts.URL+"/hash", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /hash: %d, want 405", resp.StatusCode)
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	ts, draining := newTestServer(t)
	if resp, body := do(t, "GET", ts.URL+"/healthz", ""); resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("/healthz: %d %q", resp.StatusCode, body)
	}
	if resp, _ := do(t, "GET", ts.URL+"/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz before shutdown: %d, want 200", resp.StatusCode)
	}
	draining.Store(true)
	if resp, _ := do(t, "GET", ts.URL+"/readyz", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after shutdown began: %d, want 503", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz while draining: %d, want 200", resp.StatusCode)
	}
}

func TestVariantHeaderCannotBeOverridden(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"set then WriteHeader": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(variantHeader, "spoofed")
			w.WriteHeader(http.StatusCreated)
		},
		"delete then implicit 200 via Write": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Del(variantHeader)
			io.WriteString(w, "body")
		},
		"set then implicit 200 via Write": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(variantHeader, "spoofed")
			io.WriteString(w, "body")
		},
		"writes nothing": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(variantHeader, "spoofed")
		},
		"not found": http.NotFound,
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(withVariant("v-test", h))
			defer ts.Close()
			resp, err := http.Get(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := resp.Header.Values(variantHeader); len(got) != 1 || got[0] != "v-test" {
				t.Fatalf("%s = %q, want exactly [v-test]", variantHeader, got)
			}
		})
	}
}

func TestUnwrapReachesUnderlyingWriter(t *testing.T) {
	ts := httptest.NewServer(withVariant("v-test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through variantWriter: %v", err)
		}
	})))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
