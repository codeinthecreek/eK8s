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
// fakeKubectlDir/reqs.
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
		fmt.Fprint(os.Stderr, strings.Repeat("x", 8<<10)+"\nerror: fake start failure\n")
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

// fakeProxy is the fake's server: the filter, the redirect of unclean
// paths, then a fake loadgen behind the control Service's proxy path.
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
	if !fakeAccepts(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if unclean(r.URL.Path) {
		w.Header().Set("Location", path.Clean(r.URL.Path))
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
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

// kubectl proxy redirects an accepted path with ".." segments to the
// cleaned path, which can lie outside --accept-paths; the client must
// never follow it.
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
	req, err := http.NewRequest(http.MethodGet, controlBaseURL+"/../../../../../namespaces", nil)
	must(t, err)
	var resp *http.Response
	bounded(t, func() { resp, err = p.hc.Do(req) })
	if err == nil {
		resp.Body.Close()
		t.Errorf("raw request: status %d, want an error", resp.StatusCode)
	}
	want := map[string]int{
		controlProxyPath + "/targets/..":                1,
		controlProxyPath + "/../../../../../namespaces": 1,
	}
	if got := fakeRequests(t, fdir); !maps.Equal(got, want) {
		t.Errorf("requests %v, want %v (no request to a cleaned path)", got, want)
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
		{name: "silent", mode: "silent", want: "no start line within 300ms"},
		{name: "wrong line", mode: "wrong-line", want: "start line names another socket"},
		{name: "cancelled ctx", mode: "silent", cancel: true, want: "context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fdir, tmp := installFakeKubectl(t, tc.mode)
			if !tc.cancel {
				setProxyStartTimeout(t, 300*time.Millisecond)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
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
			if tc.mode == "fail-at-start" && err != nil && (!strings.HasSuffix(err.Error(), "error: fake start failure") || len(err.Error()) > maxProxyOutput+200) {
				t.Errorf("err is %d bytes, want the stderr tail within %d bytes", len(err.Error()), maxProxyOutput)
			}
			if tc.name == "silent" && elapsed < 300*time.Millisecond {
				t.Errorf("failed after %v, before the 300ms bound", elapsed)
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
