package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen/client"
)

// Where loadgen's control API is: the loadgen Service in ek8s-system,
// port name control (deploy/ek8s-system/service.yaml), reached through
// the API server's service proxy (DESIGN.md sec 10).
const (
	controlProxyPath = "/api/v1/namespaces/ek8s-system/services/loadgen:control/proxy"
	// controlAcceptPaths is kubectl proxy's --accept-paths: only the
	// control Service's proxy subtree, so the socket reaches nothing else
	// the gate's kube credentials could. kubectl matches it against the
	// path before cleaning, so it alone does not stop ".." segments; the
	// client's redirect refusal does (controlHTTP).
	controlAcceptPaths = "^" + controlProxyPath + "(/.*)?$"
	controlBaseURL     = "http://localhost" + controlProxyPath
)

// proxyStartPrefix starts the line kubectl proxy prints on stdout once it
// listens: "Starting to serve on " and the socket path (checked against
// kubectl v1.37.0's proxy command, inferred).
const proxyStartPrefix = "Starting to serve on "

const (
	// maxSocketPath bounds the socket path: sun_path holds 108 bytes on
	// Linux, NUL included; 100 leaves a margin.
	maxSocketPath = 100
	// maxProxyOutput bounds what of kubectl's output is kept: the stderr
	// tail an error carries, and the stdout read before the start line.
	maxProxyOutput = 4 << 10
)

// proxyStartTimeout bounds the wait for kubectl proxy's start line
// (inferred: kubectl reads its kubeconfig and listens, without calling
// the API server, well within it).
var proxyStartTimeout = 10 * time.Second

// ControlProxy is a running `kubectl proxy` on a unix socket in a
// private directory, and the loadgen control API client that reaches
// loadgen through it. It is the gate's only path to the control API.
// Every error it returns is a gate fault: none involves the candidate.
type ControlProxy struct {
	cmd    *exec.Cmd
	dir    string
	stderr *tailWriter
	// done is closed once cmd.Wait has returned; waitErr is its error,
	// read only after done is closed.
	done    chan struct{}
	waitErr error
	tr      *http.Transport
	hc      *http.Client
	cl      *client.Client

	closeOnce sync.Once
	closeErr  error
}

// StartControlProxy starts `kubectl --context kubeCtx proxy` on a unix
// socket and waits for it to serve. kubectl is looked up on PATH. ctx
// bounds only the wait for the start line, as does proxyStartTimeout:
// kubectl runs until Close. timeout is the Timeout of the http.Client
// behind Client (0: none). An empty kubeCtx is an error and nothing is
// started, since kubectl would fall back to the kubeconfig's current
// context, which may be another cluster. A start error has stopped
// kubectl and removed the directory, and carries at most maxProxyOutput
// bytes of kubectl's stderr (none when ctx ended).
func StartControlProxy(ctx context.Context, kubeCtx string, timeout time.Duration) (*ControlProxy, error) {
	if kubeCtx == "" {
		return nil, errors.New("control proxy: empty kube context")
	}
	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		return nil, fmt.Errorf("control proxy: %w", err)
	}
	dir, err := controlProxyDir()
	if err != nil {
		return nil, fmt.Errorf("control proxy: %w", err)
	}
	sock := filepath.Join(dir, "s")
	p := &ControlProxy{dir: dir, stderr: &tailWriter{max: maxProxyOutput}, done: make(chan struct{})}
	p.tr, p.hc = controlHTTP(sock, timeout)
	p.cl = client.New(controlBaseURL, p.hc)
	start := &startWatcher{want: proxyStartPrefix + sock, result: make(chan error, 1)}
	p.cmd = exec.Command(kubectlPath, "--context", kubeCtx, "proxy",
		"--unix-socket", sock, "--accept-paths", controlAcceptPaths)
	p.cmd.Stdout = start
	p.cmd.Stderr = p.stderr
	p.cmd.WaitDelay = toolWaitDelay
	if err := p.cmd.Start(); err != nil {
		os.Remove(dir)
		return nil, fmt.Errorf("control proxy: %w", err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	if err := p.awaitStart(ctx, start); err != nil {
		return nil, err
	}
	return p, nil
}

// Client returns the loadgen control API client that goes through the
// proxy, the same one on every call, so its Expect state is shared.
func (p *ControlProxy) Client() *client.Client { return p.cl }

// awaitStart waits for kubectl's start line. On failure it stops kubectl
// and removes the directory before returning the error.
func (p *ControlProxy) awaitStart(ctx context.Context, start *startWatcher) error {
	timer := time.NewTimer(proxyStartTimeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-start.result:
		if err == nil {
			return nil
		}
	case <-p.done:
		err = fmt.Errorf("kubectl exited before serving: %v", p.waitErr)
	case <-timer.C:
		err = fmt.Errorf("no start line within %v", proxyStartTimeout)
	case <-ctx.Done():
		if serr := p.stop(); serr != nil {
			return fmt.Errorf("control proxy: start: %w; %v", ctx.Err(), serr)
		}
		return fmt.Errorf("control proxy: start: %w", ctx.Err())
	}
	serr := p.stop()
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		err = fmt.Errorf("%w: stderr: %s", err, tail)
	}
	if serr != nil {
		err = fmt.Errorf("%w; %v", err, serr)
	}
	return fmt.Errorf("control proxy: start: %w", err)
}

// Close stops kubectl, waits for it, closes idle connections and removes
// the directory. It is idempotent and safe after kubectl has exited on
// its own; later requests through Client fail.
func (p *ControlProxy) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.stop() })
	if p.closeErr != nil {
		return fmt.Errorf("control proxy: close: %w", p.closeErr)
	}
	return nil
}

// stop kills kubectl and waits for it, then closes idle connections and
// removes the directory. SIGKILL ends kubectl at once and WaitDelay then
// bounds Wait's draining of its pipes, so the timer below fires only for
// a process the kernel cannot end.
func (p *ControlProxy) stop() error {
	var errs []error
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		errs = append(errs, fmt.Errorf("kill kubectl: %w", err))
	}
	timer := time.NewTimer(2 * toolWaitDelay)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		errs = append(errs, errors.New("kubectl did not exit after SIGKILL"))
	}
	p.tr.CloseIdleConnections()
	if err := os.RemoveAll(p.dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// controlProxyDir makes the socket's directory with os.MkdirTemp under
// os.TempDir(), or under /tmp if the socket path there would be over
// maxSocketPath bytes (a long TMPDIR), and checks it is private.
func controlProxyDir() (string, error) {
	var sock string
	for _, base := range []string{os.TempDir(), "/tmp"} {
		dir, err := os.MkdirTemp(base, "ek8s-proxy-")
		if err != nil {
			return "", err
		}
		if sock = filepath.Join(dir, "s"); len(sock) > maxSocketPath {
			os.Remove(dir)
			continue
		}
		if err := checkPrivateDir(dir); err != nil {
			os.Remove(dir)
			return "", err
		}
		return dir, nil
	}
	return "", fmt.Errorf("socket path %s is over %d bytes", sock, maxSocketPath)
}

// checkPrivateDir refuses dir unless it is a directory, not a symlink,
// with permissions exactly 0700 and owned by this process's uid. MkdirTemp
// asks for 0700, but a umask or a filesystem can give another mode, and a
// socket in a directory others can enter would let another local user
// reach the API server with the gate's kube credentials, within
// --accept-paths.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: want a 0700 directory owned by uid %d", dir, os.Getuid())
	}
	return nil
}

// controlHTTP returns the transport and client for the socket at sock.
// The transport dials sock whatever the request's address, so no request
// leaves the host; it uses no proxy (HTTP_PROXY and its kin would
// otherwise apply) and HTTP/1.1 only, as kubectl proxy serves. The client
// follows no redirect: kubectl proxy answers a path with "." or ".."
// segments, which --accept-paths admits, with a redirect to the cleaned
// path, which can lie outside it. CheckRedirect stops the client following
// one and refuseRedirects makes it an error, so neither the loadgen
// client nor a caller of the http.Client sees a 3xx as an answer.
func controlHTTP(sock string, timeout time.Duration) (*http.Transport, *http.Client) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.ForceAttemptHTTP2 = false
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", sock)
	}
	hc := &http.Client{
		Transport: refuseRedirects{tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: timeout,
	}
	return tr, hc
}

// refuseRedirects turns any 3xx response into an error naming only its
// status: Location and the body are kubectl's or the API server's text,
// never echoed.
type refuseRedirects struct{ rt http.RoundTripper }

func (r refuseRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("control proxy: refused a redirect (status %d)", resp.StatusCode)
	}
	return resp, nil
}

// startWatcher is kubectl's stdout. It sends one result on result: nil
// for a line equal to want, an error for a start line naming another
// socket or for over maxProxyOutput bytes before the start line; other
// lines are skipped. After that it discards. It never blocks, so kubectl
// never blocks on a full pipe. exec.Cmd calls Write from one goroutine.
type startWatcher struct {
	want    string
	result  chan error // buffered, one value at most
	decided bool
	seen    int
	buf     []byte
}

func (w *startWatcher) Write(b []byte) (int, error) {
	if w.decided {
		return len(b), nil
	}
	w.seen += len(b)
	rest := b
	for !w.decided {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			w.buf = append(w.buf, rest...)
			break
		}
		line := string(append(w.buf, rest[:i]...))
		w.buf, rest = w.buf[:0], rest[i+1:]
		switch {
		case line == w.want:
			w.decide(nil)
		case strings.HasPrefix(line, proxyStartPrefix):
			w.decide(errors.New("start line names another socket"))
		}
	}
	if !w.decided && w.seen > maxProxyOutput {
		w.decide(fmt.Errorf("over %d bytes of stdout before the start line", maxProxyOutput))
	}
	if w.decided {
		w.buf = nil
	}
	return len(b), nil
}

func (w *startWatcher) decide(err error) {
	w.decided = true
	w.result <- err
}

// tailWriter keeps the last max bytes written to it. It never blocks.
type tailWriter struct {
	max int
	mu  sync.Mutex
	b   []byte
}

func (w *tailWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, b...)
	if over := len(w.b) - w.max; over > 0 {
		w.b = append(w.b[:0], w.b[over:]...)
	}
	return len(b), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}
