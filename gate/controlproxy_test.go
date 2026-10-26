package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// The ControlProxy tests' fake kubectl is this test binary, run through
// a symlink named kubectl. fakeKubectlMode selects it and its mode: ok,
// fail-at-start, silent, wrong-line or die-later. It writes its pid to
// fakeKubectlDir/pid and each request's path, one per line, to
// fakeKubectlDir/reqs. The modes noisy and noisy-ok write stdout noise
// before (noisy: without) the start line.
const (
	fakeKubectlMode = "EK8S_FAKE_KUBECTL"
	fakeKubectlDir  = "EK8S_FAKE_KUBECTL_DIR"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeKubectlMode); mode != "" {
		os.Exit(fakeKubectl(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// wantAcceptPaths is the --accept-paths the fake requires.
const wantAcceptPaths = `^/api/v1/namespaces/ek8s-system/services/loadgen:control/proxy(/.*)?$`

const fakeBootID = "boot-1"

var fakeConfig = wire.Config{
	BootID: fakeBootID, Rate: 2, WindowS: 300, BurstRate: 8, BurstSends: 80,
	WindowSends: 600, WindowPairs: 300, BurstPairs: 40, RingCapacity: 4096,
}

// fakeArgvMismatch names how args differ from the argv StartControlProxy
// must give kubectl, "" if they do not. The socket path, arg 4, must be
// absolute and end in /s.
func fakeArgvMismatch(args []string) string {
	want := []string{"--context", "kind-x", "proxy", "--unix-socket", "", "--accept-paths", wantAcceptPaths}
	if len(args) != len(want) {
		return fmt.Sprintf("%d args %q, want %d", len(args), args, len(want))
	}
	for i, a := range args {
		if i == 4 {
			if !filepath.IsAbs(a) || filepath.Base(a) != "s" {
				return fmt.Sprintf("arg 4 %q: want an absolute path ending in /s", a)
			}
			continue
		}
		if a != want[i] {
			return fmt.Sprintf("arg %d %q, want %q", i, a, want[i])
		}
	}
	return ""
}

// fakeStartFailure is what the fail-at-start mode writes to stderr.
var fakeStartFailure = strings.Repeat("x", 8<<10) + "\nerror: fake start failure\n"

// fakeNoise is n bytes of stdout lines.
func fakeNoise(n int) string {
	return strings.Repeat(strings.Repeat("x", 15)+"\n", n/16) + strings.Repeat("\n", n%16)
}

// fakeKubectl is the fake kubectl proxy: it returns its exit code.
func fakeKubectl(mode string, args []string) int {
	dir := os.Getenv(fakeKubectlDir)
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake kubectl:", err)
		return 1
	}
	if msg := fakeArgvMismatch(args); msg != "" {
		fmt.Fprintln(os.Stderr, "fake kubectl: argv:", msg)
		return 2
	}
	sock := args[4]
	if mode == "fail-at-start" {
		fmt.Fprint(os.Stderr, fakeStartFailure)
		return 1
	}
	// As kubectl's ListenUnix: remove a stale socket, listen with umask
	// 0077 so the socket is the user's only.
	if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(sock)
	}
	old := syscall.Umask(0o077)
	l, err := net.Listen("unix", sock)
	syscall.Umask(old)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake kubectl:", err)
		return 1
	}
	switch mode {
	case "silent":
	case "noisy":
		os.Stdout.WriteString(fakeNoise(maxProxyOutput + 1))
	case "noisy-ok":
		os.Stdout.WriteString(fakeNoise(maxProxyOutput))
		os.Stdout.WriteString("Starting to serve on " + l.Addr().String() + "\n")
	case "wrong-line":
		fmt.Println("Starting to serve on " + sock + ".other")
	default:
		fmt.Println("Starting to serve on " + l.Addr().String())
	}
	f := &fakeProxy{reqs: filepath.Join(dir, "reqs"), dieLater: mode == "die-later", died: make(chan struct{})}
	srv := &http.Server{Handler: f}
	if f.dieLater {
		go srv.Serve(l)
		<-f.died
		return 3
	}
	srv.Serve(l)
	return 1
}

// The v1.37.0 kubectl proxy filter's defaults, with --accept-paths as
// fakeArgvMismatch requires it.
var (
	fakeRejectPaths   = []*regexp.Regexp{regexp.MustCompile(`^/api/.*/pods/.*/exec`), regexp.MustCompile(`^/api/.*/pods/.*/attach`)}
	fakeRejectMethods = regexp.MustCompile(`^$`)
	fakeAcceptPaths   = regexp.MustCompile(wantAcceptPaths)
	fakeAcceptHosts   = []*regexp.Regexp{regexp.MustCompile(`^localhost$`), regexp.MustCompile(`^127\.0\.0\.1$`), regexp.MustCompile(`^\[::1\]$`)}
)

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// fakeAccepts is the filter: reject-paths, then reject-methods, then
// accept-paths and accept-hosts, the host with any port stripped.
func fakeAccepts(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if anyMatch(fakeRejectPaths, r.URL.Path) || fakeRejectMethods.MatchString(r.Method) {
		return false
	}
	return fakeAcceptPaths.MatchString(r.URL.Path) && anyMatch(fakeAcceptHosts, host)
}

func unclean(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return true
		}
	}
	return false
}

// fakeProxy is the fake's server: the redirect of unclean paths (Go's
// ServeMux does it before the handler), the filter, then a fake loadgen
// behind the control Service's proxy path, and under it /redirect/<code>,
// which answers <code> with a Location of /config.
type fakeProxy struct {
	reqs     string
	mu       sync.Mutex
	dieLater bool
	once     sync.Once
	died     chan struct{}
}

func (f *fakeProxy) log(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, err := os.OpenFile(f.reqs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fl.Close()
	fmt.Fprintln(fl, p)
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.log(r.URL.Path)
	if unclean(r.URL.Path) {
		w.Header().Set("Location", path.Clean(r.URL.Path))
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if !fakeAccepts(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if s, ok := strings.CutPrefix(r.URL.Path, controlProxyPath+"/redirect/"); ok {
		if code, err := strconv.Atoi(s); err == nil {
			w.Header().Set("Location", controlProxyPath+"/config")
			w.WriteHeader(code)
			return
		}
	}
	h := w.Header()
	h.Set("Audit-Id", "6f1c2a3b-0000-4000-8000-000000000001")
	h.Set("Cache-Control", "no-cache, private")
	h.Set("Content-Type", "application/json")
	h.Set(wire.BootIDHeader, fakeBootID)
	status := http.StatusOK
	var v any = fakeConfig
	if r.Method != http.MethodGet || strings.TrimPrefix(r.URL.Path, controlProxyPath) != "/config" {
		status, v = http.StatusNotFound, wire.Error{BootID: fakeBootID, Error: "not found"}
	}
	b, _ := json.Marshal(v)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
	if f.dieLater {
		w.(http.Flusher).Flush()
		f.once.Do(func() { close(f.died) })
	}
}

// installFakeKubectl puts the fake, in mode, first on PATH and points
// TMPDIR at a fresh empty directory. It returns the fake's directory and
// that TMPDIR.
func installFakeKubectl(t *testing.T, mode string) (fdir, tmp string) {
	t.Helper()
	exe, err := os.Executable()
	must(t, err)
	bin := t.TempDir()
	must(t, os.Symlink(exe, filepath.Join(bin, "kubectl")))
	fdir, tmp = t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeKubectlMode, mode)
	t.Setenv(fakeKubectlDir, fdir)
	t.Setenv("TMPDIR", tmp)
	return fdir, tmp
}

// startProxy starts a ControlProxy for kube context kind-x, closed at
// the test's end.
func startProxy(t *testing.T, ctx context.Context) (*ControlProxy, error) {
	t.Helper()
	var p *ControlProxy
	var err error
	bounded(t, func() { p, err = StartControlProxy(ctx, "kind-x", 5*time.Second) })
	if p != nil {
		t.Cleanup(func() { p.Close() })
	}
	return p, err
}

// fakeRequests counts the fake's requests by path.
func fakeRequests(t *testing.T, fdir string) map[string]int {
	t.Helper()
	n := map[string]int{}
	for _, l := range strings.Split(readFile(t, filepath.Join(fdir, "reqs")), "\n") {
		if l != "" {
			n[l]++
		}
	}
	return n
}

// fakeGone fails the test unless the fake's process is gone.
func fakeGone(t *testing.T, fdir string) {
	t.Helper()
	s := readFile(t, filepath.Join(fdir, "pid"))
	pid, err := strconv.Atoi(s)
	if err != nil {
		t.Errorf("fake kubectl pid %q: %v", s, err)
		return
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("fake kubectl %d: kill 0 = %v, want ESRCH", pid, err)
	}
}

// dirEmpty fails the test unless dir holds nothing.
func dirEmpty(t *testing.T, dir string) {
	t.Helper()
	es, err := os.ReadDir(dir)
	must(t, err)
	if len(es) != 0 {
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q, want nothing", dir, names)
	}
}

func setProxyStartTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := proxyStartTimeout
	proxyStartTimeout = d
	t.Cleanup(func() { proxyStartTimeout = old })
}

func TestControlProxyOK(t *testing.T) {
	fdir, tmp := installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var cfg wire.Config
	bounded(t, func() { cfg, err = p.Client().Config(context.Background()) })
	must(t, err)
	if cfg != fakeConfig {
		t.Errorf("Config = %+v, want %+v", cfg, fakeConfig)
	}
	if got := filepath.Dir(p.dir); got != tmp {
		t.Errorf("dir %s is not in TMPDIR %s", p.dir, tmp)
	}
	fi, err := os.Lstat(p.dir)
	must(t, err)
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want a 0700 directory", fi.Mode())
	}
	fi, err = os.Lstat(filepath.Join(p.dir, "s"))
	must(t, err)
	if got := fi.Mode().String(); got != "Srwx------" {
		t.Errorf("socket mode %s, want Srwx------", got)
	}
	if got, want := fakeRequests(t, fdir), map[string]int{controlProxyPath + "/config": 1}; !maps.Equal(got, want) {
		t.Errorf("requests %v, want %v", got, want)
	}
	bounded(t, func() { err = p.Close() })
	must(t, err)
	if _, err := os.Lstat(p.dir); !os.IsNotExist(err) {
		t.Errorf("dir after Close: %v, want it gone", err)
	}
	fakeGone(t, fdir)
	bounded(t, func() { err = p.Close() })
	if err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// kubectl proxy's server redirects any path with ".." segments to the
// cleaned path, before its filter runs, and the cleaned path can lie
// anywhere; the client must never follow it.
func TestControlProxyRefusesRedirect(t *testing.T) {
	fdir, _ := installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded(t, func() { _, _, err = p.Client().PutTarget(context.Background(), "..") })
	if err == nil || !strings.Contains(err.Error(), "refused a redirect (status 307)") {
		t.Errorf("PutTarget(..) = %v, want a refused redirect", err)
	}
	want := map[string]int{controlProxyPath + "/targets/..": 1}
	for _, raw := range []string{
		controlBaseURL + "/../../../../../namespaces",
		controlBaseURL + "/../../../../pods",
		"http://localhost/api/v1/../namespaces", // off the prefix: still a redirect, not a 403
	} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		must(t, err)
		var resp *http.Response
		bounded(t, func() { resp, err = p.hc.Do(req) })
		if err == nil {
			resp.Body.Close()
			t.Errorf("GET %s: status %d, want an error", raw, resp.StatusCode)
		}
		want[req.URL.Path]++
	}
	if got := fakeRequests(t, fdir); !maps.Equal(got, want) {
		t.Errorf("requests %v, want %v (no request to a cleaned path)", got, want)
	}
}

// Every 3xx is refused, none reaches a caller as an answer, and the
// Location is not followed.
func TestControlProxyRefusesEveryRedirect(t *testing.T) {
	fdir, _ := installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	for _, code := range []int{301, 302, 303, 307, 308} {
		url := controlBaseURL + "/redirect/" + strconv.Itoa(code)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		must(t, err)
		var resp *http.Response
		bounded(t, func() { resp, err = p.hc.Do(req) })
		if err == nil {
			resp.Body.Close()
			t.Errorf("status %d: got a response with status %d, want an error", code, resp.StatusCode)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("refused a redirect (status %d)", code)) {
			t.Errorf("status %d: err = %v, want it to name the status", code, err)
		}
		want[controlProxyPath+"/redirect/"+strconv.Itoa(code)]++
	}
	if got := fakeRequests(t, fdir); !maps.Equal(got, want) {
		t.Errorf("requests %v, want %v (no request to /config)", got, want)
	}
}

func TestControlProxyFilter(t *testing.T) {
	installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"http://localhost/api/v1/namespaces", controlBaseURL + "x"} {
		bounded(t, func() { _, err = client.New(base, p.hc).Config(context.Background()) })
		var ae *client.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
			t.Errorf("Config at %s = %v, want an APIError with status 403", base, err)
		}
	}
	req, err := http.NewRequest(http.MethodGet, controlBaseURL+"/config", nil)
	must(t, err)
	req.Host = "example.com"
	var resp *http.Response
	bounded(t, func() { resp, err = p.hc.Do(req) })
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Host example.com: status %d, want 403", resp.StatusCode)
	}
}

func TestControlProxyStartFailures(t *testing.T) {
	tests := []struct {
		name, mode string
		cancel     bool
		want       string
	}{
		{name: "fail at start", mode: "fail-at-start", want: "kubectl exited before serving: exit status 1: stderr: x"},
		{name: "silent", mode: "silent", want: "no start line within 1s"},
		{name: "wrong line", mode: "wrong-line", want: "start line names another socket"},
		{name: "noisy", mode: "noisy", want: "over 4096 bytes of stdout"},
		{name: "cancelled ctx", mode: "silent", cancel: true, want: "context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fdir, tmp := installFakeKubectl(t, tc.mode)
			if !tc.cancel {
				setProxyStartTimeout(t, time.Second)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				// Cancel only once the fake is running, however slowly a
				// loaded runner starts it.
				// The goroutine ends before the subtest does, so its t.Error
				// cannot come after the subtest has returned.
				stop := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(1)
				t.Cleanup(func() { close(stop); wg.Wait() })
				go func() {
					defer wg.Done()
					for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						select {
						case <-stop:
							return
						default:
						}
						if b, err := os.ReadFile(filepath.Join(fdir, "pid")); err == nil && len(b) > 0 {
							cancel()
							return
						}
					}
					select {
					case <-stop:
					default:
						t.Error("fake kubectl wrote no pid within 5 s")
					}
				}()
			}
			start := time.Now()
			p, err := startProxy(t, ctx)
			elapsed := time.Since(start)
			if p != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got (%v, %v), want an error containing %q", p, err, tc.want)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled", err)
			}
			if tc.mode == "fail-at-start" && err != nil {
				tail := strings.TrimSpace(fakeStartFailure[len(fakeStartFailure)-maxProxyOutput:])
				if !strings.HasSuffix(err.Error(), ": stderr: "+tail) {
					t.Errorf("err (%d bytes) does not end with the last %d bytes of stderr", len(err.Error()), maxProxyOutput)
				}
				if len(err.Error()) > maxProxyOutput+200 {
					t.Errorf("err is %d bytes, want the stderr tail within %d bytes", len(err.Error()), maxProxyOutput)
				}
			}
			if tc.name == "silent" && elapsed < time.Second {
				t.Errorf("failed after %v, before the 1s bound", elapsed)
			}
			if elapsed > 5*time.Second {
				t.Errorf("failed after %v, want promptly", elapsed)
			}
			fakeGone(t, fdir)
			dirEmpty(t, tmp)
		})
	}
}

func TestControlProxyKubectlDies(t *testing.T) {
	fdir, _ := installFakeKubectl(t, "die-later")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	must(t, err)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake kubectl did not exit within 5 s")
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	if err == nil {
		t.Error("Config after kubectl died: want an error")
	}
	bounded(t, func() { err = p.Close() })
	must(t, err)
	bounded(t, func() { err = p.Close() })
	must(t, err)
	if _, err := os.Lstat(p.dir); !os.IsNotExist(err) {
		t.Errorf("dir after Close: %v, want it gone", err)
	}
	fakeGone(t, fdir)
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	n atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// Once Close has removed the directory, another local user can recreate
// the socket path and serve forged answers there; the Client must dial
// nothing. The forger here is this user, which can do no less.
func TestControlProxyNoDialAfterClose(t *testing.T) {
	installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	must(t, err)
	bounded(t, func() { err = p.Close() })
	must(t, err)

	mkPrivate(t, p.dir, 0o700)
	t.Cleanup(func() { os.RemoveAll(p.dir) })
	sock := filepath.Join(p.dir, "s")
	ln, err := net.Listen("unix", sock)
	must(t, err)
	l := &countingListener{Listener: ln}
	srv := &http.Server{Handler: &fakeProxy{reqs: filepath.Join(t.TempDir(), "reqs")}}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(l)
	}()
	t.Cleanup(func() {
		srv.Close()
		<-served
	})

	// The forged socket answers a fresh client as loadgen would.
	ftr, fhc := controlHTTP(&sockDialer{sock: sock}, 5*time.Second)
	var cfg wire.Config
	bounded(t, func() { cfg, err = client.New(controlBaseURL, fhc).Config(context.Background()) })
	ftr.CloseIdleConnections()
	if err != nil || cfg != fakeConfig {
		t.Fatalf("Config at the forged socket = %+v, %v, want %+v", cfg, err, fakeConfig)
	}
	before := l.n.Load()

	bounded(t, func() { cfg, err = p.Client().Config(context.Background()) })
	if !errors.Is(err, errControlProxyClosed) || !strings.Contains(err.Error(), "control proxy: closed") {
		t.Errorf("Config after Close = %+v, %v, want an error naming the closed proxy", cfg, err)
	}
	if n := l.n.Load() - before; n != 0 {
		t.Errorf("the recreated socket accepted %d connections from the closed proxy, want 0", n)
	}
}

// Requests through Client in several goroutines while Close runs each
// succeed or fail, and every goroutine returns.
func TestControlProxyCloseDuringRequests(t *testing.T) {
	installFakeKubectl(t, "ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var started, wg sync.WaitGroup
	started.Add(workers)
	wg.Add(workers)
	// last[i] is worker i's first error; it stops there.
	last := make([]error, workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			started.Done()
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := p.Client().Config(ctx)
				cancel()
				if err != nil {
					last[i] = err
					return
				}
			}
		}()
	}
	started.Wait()
	time.Sleep(20 * time.Millisecond)
	bounded(t, func() { err = p.Close() })
	must(t, err)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("requests did not all return within 10 s of Close")
	}
	for i, err := range last {
		if err == nil {
			t.Errorf("worker %d: no request failed after Close", i)
		}
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	if !errors.Is(err, errControlProxyClosed) {
		t.Errorf("Config after Close = %v, want errControlProxyClosed", err)
	}
}

// A TMPDIR so long that the socket path would be over maxSocketPath puts
// the socket's directory under /tmp.
func TestControlProxyLongTMPDIR(t *testing.T) {
	_, tmp := installFakeKubectl(t, "ok")
	long := filepath.Join(tmp, strings.Repeat("d", 100))
	must(t, os.Mkdir(long, 0o700))
	t.Setenv("TMPDIR", long)
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(p.dir); got != "/tmp" {
		t.Errorf("dir %s, want one in /tmp", p.dir)
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	must(t, err)
	dirEmpty(t, long)
	bounded(t, func() { err = p.Close() })
	must(t, err)
	if _, err := os.Lstat(p.dir); !os.IsNotExist(err) {
		t.Errorf("dir after Close: %v, want it gone", err)
	}
}

func TestControlProxyEmptyContext(t *testing.T) {
	fdir, tmp := installFakeKubectl(t, "ok")
	var p *ControlProxy
	var err error
	bounded(t, func() { p, err = StartControlProxy(context.Background(), "", 0) })
	if p != nil || err == nil || !strings.Contains(err.Error(), "empty kube context") {
		t.Errorf("got (%v, %v), want an error naming the empty kube context", p, err)
	}
	if _, err := os.Lstat(filepath.Join(fdir, "pid")); !os.IsNotExist(err) {
		t.Errorf("fake kubectl ran: %v", err)
	}
	dirEmpty(t, tmp)
}

// A ctx that is done before the call starts nothing: no kubectl, no
// directory. Started and killed at once, the fake may die before it writes
// its pid, so the pid file and TMPDIR alone would not show a start; PATH is
// emptied, so a start would fail in LookPath, not with context.Canceled.
func TestControlProxyCancelledBeforeStart(t *testing.T) {
	fdir, tmp := installFakeKubectl(t, "ok")
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := startProxy(t, ctx)
	if p != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("got (%v, %v), want an error wrapping context.Canceled", p, err)
	}
	if _, err := os.Lstat(filepath.Join(fdir, "pid")); !os.IsNotExist(err) {
		t.Errorf("fake kubectl ran: %v", err)
	}
	dirEmpty(t, tmp)
}

// A TMPDIR that does not exist puts the socket's directory under /tmp.
func TestControlProxyTMPDIRMissing(t *testing.T) {
	_, tmp := installFakeKubectl(t, "ok")
	t.Setenv("TMPDIR", filepath.Join(tmp, "missing"))
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(p.dir); got != "/tmp" {
		t.Errorf("dir %s, want one in /tmp", p.dir)
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	must(t, err)
	bounded(t, func() { err = p.Close() })
	must(t, err)
	if _, err := os.Lstat(p.dir); !os.IsNotExist(err) {
		t.Errorf("dir after Close: %v, want it gone", err)
	}
}

// Exactly maxProxyOutput bytes of stdout before the start line, which comes
// in a write of its own, are not over the bound.
func TestControlProxyNoisyStartAtBound(t *testing.T) {
	installFakeKubectl(t, "noisy-ok")
	p, err := startProxy(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bounded(t, func() { _, err = p.Client().Config(context.Background()) })
	must(t, err)
}

func setMkdirTemp(t *testing.T, f func(dir, pattern string) (string, error)) {
	t.Helper()
	old := mkdirTemp
	mkdirTemp = f
	t.Cleanup(func() { mkdirTemp = old })
}

// mkPrivate makes dir with mode perm, whatever the umask.
func mkPrivate(t *testing.T, dir string, perm os.FileMode) {
	t.Helper()
	must(t, os.Mkdir(dir, perm))
	must(t, os.Chmod(dir, perm))
}

func TestControlProxyDir(t *testing.T) {
	// sockDir is a directory name under base whose socket path, dir + "/s",
	// is exactly n bytes.
	sockDir := func(base string, n int) string {
		pad := n - 2 - len(base) - 1
		if pad < 1 {
			t.Fatalf("base %s leaves no room for a %d-byte socket path", base, n)
		}
		return filepath.Join(base, strings.Repeat("d", pad))
	}
	t.Run("100 bytes", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		want := sockDir(tmp, maxSocketPath)
		var bases []string
		setMkdirTemp(t, func(base, _ string) (string, error) {
			bases = append(bases, base)
			mkPrivate(t, want, 0o700)
			return want, nil
		})
		dir, err := controlProxyDir()
		if err != nil || dir != want || len(filepath.Join(dir, "s")) != maxSocketPath {
			t.Errorf("controlProxyDir = %q, %v, want %q (a %d-byte socket path)", dir, err, want, maxSocketPath)
		}
		if len(bases) != 1 || bases[0] != tmp {
			t.Errorf("bases tried %q, want only %q", bases, tmp)
		}
	})
	t.Run("101 bytes", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		long := sockDir(tmp, maxSocketPath+1)
		short := filepath.Join(t.TempDir(), "short")
		var bases []string
		setMkdirTemp(t, func(base, _ string) (string, error) {
			bases = append(bases, base)
			if base == "/tmp" {
				mkPrivate(t, short, 0o700)
				return short, nil
			}
			mkPrivate(t, long, 0o700)
			return long, nil
		})
		dir, err := controlProxyDir()
		if err != nil || dir != short {
			t.Errorf("controlProxyDir = %q, %v, want %q", dir, err, short)
		}
		if len(bases) != 2 || bases[0] != tmp || bases[1] != "/tmp" {
			t.Errorf("bases tried %q, want %q then /tmp", bases, tmp)
		}
		if _, err := os.Lstat(long); !os.IsNotExist(err) {
			t.Errorf("the 101-byte dir: %v, want it removed", err)
		}
	})
	t.Run("not private", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		open := filepath.Join(tmp, "open")
		setMkdirTemp(t, func(string, string) (string, error) {
			mkPrivate(t, open, 0o755)
			return open, nil
		})
		dir, err := controlProxyDir()
		if err == nil || dir != "" || !strings.Contains(err.Error(), open) {
			t.Errorf("controlProxyDir = %q, %v, want an error naming %s", dir, err, open)
		}
		if _, err := os.Lstat(open); !os.IsNotExist(err) {
			t.Errorf("the 0755 dir: %v, want it removed", err)
		}
	})
}

func TestControlProxyCheckPrivateDir(t *testing.T) {
	tmp := t.TempDir()
	private, open, link := filepath.Join(tmp, "private"), filepath.Join(tmp, "open"), filepath.Join(tmp, "link")
	mkPrivate(t, private, 0o700)
	mkPrivate(t, open, 0o755)
	must(t, os.Symlink(private, link))
	if err := checkPrivateDir(private); err != nil {
		t.Errorf("0700 dir: %v, want it accepted", err)
	}
	for name, dir := range map[string]string{"0755 dir": open, "symlink to a 0700 dir": link} {
		if err := checkPrivateDir(dir); err == nil {
			t.Errorf("%s: want it refused", name)
		}
	}
}
