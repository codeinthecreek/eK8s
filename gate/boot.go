package gate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timeouts for bringing the candidate up (DESIGN.md sec 7, Stage 1).
const (
	healthPoll       = 100 * time.Millisecond
	bareBootTimeout  = 10 * time.Second // first /healthz 200, from the relay being up
	sandboxUpTimeout = 10 * time.Second // bwrap info, then relay "ready", each
	exitGrace        = time.Second      // to tell a candidate exit from a join fault
	maxOutputBytes   = 64 << 10         // candidate stdout+stderr kept for Detail
)

// sandboxUID is the uid and gid the candidate runs as, in the sandbox
// and in the image (USER 65532:65532).
const sandboxUID = "65532"

// newLabel returns a fresh per-run X-EK8s-Variant value, so a stale
// listener cannot answer for the candidate. It has the cluster's shape,
// ek8s-<16 hex> (deploy/variant), so the label is no gate tell
// (DESIGN.md sec 5).
func newLabel() string {
	b := make([]byte, 8)
	rand.Read(b) // never returns an error; crashes the program on failure
	return "ek8s-" + hex.EncodeToString(b)
}

// podNameAlphabet is the alphabet Kubernetes uses for generated names
// (k8s.io/apimachinery/pkg/util/rand alphanums: no vowels, no 0/1/3).
const podNameAlphabet = "bcdfghjklmnpqrstvwxz2456789"

// newPodName returns a fresh Deployment-pod-like hostname,
// hasher-<hash>-<5>, for the sandbox: a fixed gate hostname would let a
// candidate read it via http.Dir("/proc") and behave only in the gate.
// It is drawn as a Deployment pod's name is: the middle part is a
// pod-template-hash, a uint32's decimal digits each mapped through
// SafeEncodeString (so 10 characters for ~77% of names, 1-9 otherwise),
// and the suffix is 5 uniform characters (rand.String). This removes only
// that tell; others remain (docs/STATUS.md, Parked). The prefix assumes
// deploy/'s Deployment is named hasher.
func newPodName() string {
	var b [4]byte
	rand.Read(b[:])
	hash := []byte(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(b[:])), 10))
	for i, c := range hash {
		hash[i] = podNameAlphabet[int(c)%len(podNameAlphabet)] // SafeEncodeString
	}
	suffix := make([]byte, 0, 5)
	for len(suffix) < 5 {
		rand.Read(b[:1])
		// Reject the top 256 % 27 values so every character is equally likely.
		if int(b[0]) < 256-256%len(podNameAlphabet) {
			suffix = append(suffix, podNameAlphabet[int(b[0])%len(podNameAlphabet)])
		}
	}
	return "hasher-" + string(hash) + "-" + string(suffix)
}

// raceTags are Stage 4's race build tags: the pure-Go resolver and user
// lookup, so the static link pulls in no glibc NSS code (DESIGN.md sec 7
// Stage 4). raceFlags is shared with the race go list (precheck.go), so
// the listing checks exactly the configuration that is built.
const raceTags = "netgo,osusergo"

var raceFlags = []string{"-race", "-tags", raceTags}

// raceLinkFlags links the cgo race build statically, so the bare boot
// still binds only the binary: no ld.so or libc in the sandbox. It needs
// gcc and a static libc (libc6-dev, glibc-static) on the gate host.
const raceLinkFlags = "-ldflags=-linkmode=external -extldflags=-static"

// buildDetail is a build Result's Detail: "" when go build succeeded,
// else its output.
func buildDetail(ok bool, output string) string {
	if ok {
		return ""
	}
	return output
}

// goBuild builds pkg (relative to dir) into out with the scratch
// environment: statically with CGO_ENABLED=0, or, if race, as Stage 4's
// static race build with CGO_ENABLED=1. ok is false if go build itself
// failed; output then holds its output, never empty.
func goBuild(ctx context.Context, tc toolchain, dir, pkg, out string, race bool) (ok bool, output string, err error) {
	args := []string{"build", "-trimpath"}
	if race {
		args = append(append(args, raceFlags...), raceLinkFlags)
	}
	cmd := exec.CommandContext(ctx, tc.goBin, append(args, "-o", out, "./"+pkg)...)
	cmd.Dir = dir
	cmd.Env = tc.env(race)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return false, "", fmt.Errorf("go build %s: %w", pkg, ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, "go build: " + exit.String() + ": " + strings.TrimSpace(stderr.String()), nil
	}
	if err != nil {
		return false, "", fmt.Errorf("go build %s: %w", pkg, err)
	}
	return true, "", nil
}

// cappedBuffer keeps the first maxOutputBytes written to it and counts
// the rest. It is safe for concurrent use: os/exec writes to it from
// its own copying goroutines.
type cappedBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(len(p), maxOutputBytes-c.buf.Len())
	c.buf.Write(p[:n])
	c.dropped += len(p) - n
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.buf.String()
	if c.dropped > 0 {
		s += fmt.Sprintf("\n[%d more bytes dropped]", c.dropped)
	}
	return s
}

// sandbox is a candidate booted as a bare process under bubblewrap, in
// its own user, network, pid, ipc, uts and mount namespaces, plus the
// relay that reaches it.
type sandbox struct {
	bwrap, relay       *exec.Cmd
	relayStdin         io.Closer     // closing it (or the gate dying) ends the relay
	exited, relayEnded chan struct{} // closed once each Wait returns
	who                string        // "candidate", or "probe" in the preflight; names it in failures
	output             *cappedBuffer // the sandboxed binary's (and bwrap's) stdout+stderr
	race               *raceWatch    // the same output, scanned for a race report (Stage 4)
	client             *http.Client  // reaches the candidate's :8080 via the relay
	stopOnce           sync.Once
}

// bwrapArgs is the bare-boot argv (docs/decisions.md, PR2 entry). Not
// bound: /dev (it breaks the relay's nsenter join, and the binary does
// not need it), /tmp, /home, the repo, the module cache. Not set:
// --disable-userns (also breaks the join; the candidate cannot reach
// unshare or clone without syscall, os/exec or unsafe, all outside the
// import allowlist). The resource limits stay unset (0, unconstrained)
// until deploy/ fixes the LimitRange values. env holds extra NAME, value
// pairs for the sandboxed binary (Stage 4's GORACE and GOMAXPROCS).
func bwrapArgs(binary, label, hostname string, env ...string) []string {
	args := []string{
		"--unshare-all", "--unshare-user",
		"--die-with-parent", "--new-session",
		"--clearenv", "--setenv", "EK8S_VARIANT", label,
	}
	for i := 0; i+1 < len(env); i += 2 {
		args = append(args, "--setenv", env[i], env[i+1])
	}
	return append(args,
		"--uid", sandboxUID, "--gid", sandboxUID, "--hostname", hostname,
		"--ro-bind", binary, "/service",
		"--proc", "/proc",
		"--chdir", "/", "--cap-drop", "ALL",
		"--info-fd", "3",
		"/service",
	)
}

// hostTools are the gate host's programs the bare boot needs. A missing
// one is a gate fault, never a candidate failure.
var hostTools = []string{"bwrap", "nsenter"}

// bootBare starts binary under bubblewrap, joins its network namespace
// with the relay (built at relayBin) listening on a socket in work, and
// waits for /healthz. env is passed to bwrapArgs. problem is non-empty
// if the candidate failed to come up; err is a gate fault. On success the
// caller must call stop.
func bootBare(ctx context.Context, who, binary, relayBin, work, label string, env ...string) (sb *sandbox, problem string, err error) {
	for _, tool := range hostTools {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, "", fmt.Errorf("bare boot needs %s on PATH: %w", tool, err)
		}
	}
	sock := filepath.Join(work, "relay.sock")
	if len(sock) >= 108 { // sun_path, including the NUL
		return nil, "", fmt.Errorf("relay socket path too long for a Unix socket: %s", sock)
	}

	infoR, infoW, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	defer infoR.Close()
	sb = &sandbox{who: who, output: new(cappedBuffer), race: new(raceWatch), exited: make(chan struct{})}
	sb.bwrap = exec.Command("bwrap", bwrapArgs(binary, label, newPodName(), env...)...)
	sb.bwrap.Env = []string{} // bwrap's own environment; --clearenv covers the candidate's
	// One writer value for both, so os/exec gives them a single pipe and
	// never calls Write concurrently.
	out := sb.outputWriter()
	sb.bwrap.Stdout = out
	sb.bwrap.Stderr = out
	sb.bwrap.ExtraFiles = []*os.File{infoW} // fd 3
	sb.bwrap.WaitDelay = time.Second
	err = sb.bwrap.Start()
	infoW.Close()
	if err != nil {
		return nil, "", fmt.Errorf("start bwrap: %w", err)
	}
	go func() { sb.bwrap.Wait(); close(sb.exited) }()
	defer func() {
		if sb != nil && (problem != "" || err != nil) {
			sb.stop()
			sb = nil
		}
	}()

	// bwrap writes one JSON object to the info fd once the sandbox's pid 1
	// exists; it shares the candidate's user and network namespaces.
	infoR.SetReadDeadline(time.Now().Add(sandboxUpTimeout))
	var info struct {
		ChildPid int `json:"child-pid"`
	}
	if err := json.NewDecoder(infoR).Decode(&info); err != nil || info.ChildPid <= 0 {
		// Nothing the candidate does runs before bwrap reports the child,
		// so a missing report is the host's fault (e.g. no user namespaces).
		return sb, "", fmt.Errorf("bwrap reported no sandbox pid (%v): %s", err, sb.output)
	}

	if problem, err := sb.startRelay(ctx, info.ChildPid, relayBin, sock); problem != "" || err != nil {
		return sb, problem, err
	}
	sb.client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true, // one connection per request
		},
	}
	problem = waitHealthy(ctx, sb.client, "http://candidate/healthz", label, bareBootTimeout, sb.exited)
	if ctx.Err() != nil {
		return sb, "", ctx.Err()
	}
	if problem != "" {
		return sb, sb.withOutput(problem), nil
	}
	return sb, "", nil
}

// preflight boots the fixed, gate-built probe (gate/internal/probe)
// exactly as a candidate is booted: same bwrapArgs (env included), nsenter
// join, relay "ready" and /healthz, then teardown. Stage 4 runs it again
// on the probe's race build. Only gate code runs, so any
// failure is the host's (no user namespaces with capabilities, a broken
// nsenter): a gate fault carrying bwrap's and nsenter's output, never a
// candidate Result. A host failure after a passing preflight still
// reads as the candidate's boot failure (docs/decisions.md).
func preflight(ctx context.Context, probeBin, relayBin, work string, env ...string) error {
	dir := filepath.Join(work, "preflight") // its own relay.sock
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	sb, problem, err := bootBare(ctx, "probe", probeBin, relayBin, dir, newLabel(), env...)
	if err != nil {
		return fmt.Errorf("sandbox preflight (gate probe, no candidate code): %w", err)
	}
	if problem != "" {
		return fmt.Errorf("sandbox preflight (gate probe, no candidate code): %s", problem)
	}
	sb.stop()
	return nil
}

// startRelay runs the relay inside the sandbox's user and network
// namespaces and waits for it to print "ready". If the join fails because
// the candidate already exited, that is the candidate's problem;
// otherwise a failed join is a gate fault.
func (sb *sandbox) startRelay(ctx context.Context, pid int, relayBin, sock string) (problem string, err error) {
	relayOut := new(cappedBuffer)
	sb.relay = exec.Command("nsenter", "-t", fmt.Sprint(pid), "-U", "-n", "--preserve-credentials", relayBin, sock)
	sb.relay.Env = []string{}
	sb.relay.Stderr = relayOut
	sb.relay.WaitDelay = time.Second
	stdout, err := sb.relay.StdoutPipe()
	if err != nil {
		return "", err
	}
	// Never written: its write end closes when the gate exits, however it
	// exits, and the relay quits on EOF (gate/internal/relay).
	stdin, err := sb.relay.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := sb.relay.Start(); err != nil {
		sb.relay = nil
		return "", fmt.Errorf("start nsenter: %w", err)
	}
	sb.relayStdin = stdin
	sb.relayEnded = make(chan struct{})
	ready := make(chan bool, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line == "ready\n"
		io.Copy(io.Discard, stdout) // drain until exit, so Wait can return
		sb.relay.Wait()
		close(sb.relayEnded)
	}()

	timer := time.NewTimer(sandboxUpTimeout)
	defer timer.Stop()
	select {
	case ok := <-ready:
		if ok {
			return "", nil
		}
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// The relay did not come up. If the candidate exited meanwhile, its
	// namespaces went with it and the join could not have worked.
	select {
	case <-sb.exited:
		return sb.withOutput(sb.who + " exited during startup: " + sb.bwrap.ProcessState.String()), nil
	case <-time.After(exitGrace):
	}
	return "", fmt.Errorf("relay did not start in the sandbox's namespaces: %s", sb.withOutput(strings.TrimSpace(relayOut.String())))
}

// stop kills bwrap (whose sandbox pid 1 dies with it, taking the
// candidate) and the relay, and waits for both. Calls after the first do
// nothing, so Run can stop the Stage 2 sandbox before Stage 4 and still
// defer stop.
func (sb *sandbox) stop() {
	sb.stopOnce.Do(func() {
		if sb.relay != nil && sb.relay.Process != nil {
			sb.relayStdin.Close()
			sb.relay.Process.Kill()
			<-sb.relayEnded
		}
		sb.bwrap.Process.Kill()
		<-sb.exited
	})
}

// outputWriter is where the sandboxed binary's stdout and stderr go:
// the capped copy for Detail and the race scan. Both writers always
// return len(p), nil, so io.MultiWriter never stops at a short write
// and os/exec keeps draining the pipe however much the binary writes.
func (sb *sandbox) outputWriter() io.Writer { return io.MultiWriter(sb.output, sb.race) }

// withOutput appends the sandboxed binary's captured output (bwrap's
// included) to a failure detail.
func (sb *sandbox) withOutput(problem string) string {
	if out := strings.TrimSpace(sb.output.String()); out != "" {
		return problem + "\n" + sb.who + " output:\n" + out
	}
	return problem
}

// waitHealthy polls url until it answers 200 with body "ok" and exactly
// one X-EK8s-Variant equal to label, failing after timeout or at once
// when exited is closed. It returns the failure, or "".
func waitHealthy(ctx context.Context, client *http.Client, url, label string, timeout time.Duration, exited <-chan struct{}) string {
	deadline := time.Now().Add(timeout)
	last := "no response"
	for {
		if p := checkHealth(ctx, client, url, label); p == "" {
			return ""
		} else {
			last = p
		}
		select {
		case <-exited:
			return "exited before /healthz answered (last: " + last + ")"
		case <-ctx.Done():
			return "cancelled" // the caller reports ctx.Err() as a gate fault
		case <-time.After(healthPoll):
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("/healthz not healthy within %v (last: %s)", timeout, last)
		}
	}
}

// checkHealth does one GET of url and returns why it is not healthy, or "".
func checkHealth(ctx context.Context, client *http.Client, url, label string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return err.Error()
	}
	if p := checkVariant(resp.Header, label); p != "" {
		return p
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		return fmt.Sprintf("status %d, body %q", resp.StatusCode, body)
	}
	return ""
}

// checkVariant requires exactly one X-EK8s-Variant header equal to label
// (DESIGN.md sec 5, sec 7).
func checkVariant(h http.Header, label string) string {
	if v := h.Values(variantHeader); len(v) != 1 || v[0] != label {
		return fmt.Sprintf("%s %q, want [%q]", variantHeader, v, label)
	}
	return ""
}

// variantHeader is cmd/service's per-response variant header.
const variantHeader = "X-EK8s-Variant"
