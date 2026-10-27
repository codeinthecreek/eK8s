package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/lineage"
)

// requireToolsEnv, set to 1 (CI does), turns a missing bwrap from a skip
// into a test failure, as in the gate's tests.
const requireToolsEnv = "EK8S_GATE_REQUIRE_TOOLS"

const fakeVersion = "0.0.0-fake"

// fakeClaude is the built driver/internal/fakeclaude, set by TestMain.
var fakeClaude string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "driver-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeClaude = filepath.Join(dir, "claude")
	// Static, so it needs nothing the sandbox might lack.
	cmd := exec.Command("go", "build", "-o", fakeClaude, "./internal/fakeclaude")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakeclaude: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// needBwrap skips the test, or fails it under requireToolsEnv, without
// bwrap on PATH.
func needBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		if os.Getenv(requireToolsEnv) == "1" {
			t.Fatalf("%s=1 but bwrap is missing", requireToolsEnv)
		}
		t.Skip("bwrap not on PATH")
	}
}

// newDriver returns a Driver over the fake claude.
func newDriver(t *testing.T) *Driver {
	t.Helper()
	needBwrap(t)
	d, err := New(context.Background(), Config{Claude: fakeClaude, Version: fakeVersion, Token: "tok-test", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// gen0 returns the repo's generation-0 wrapper files.
func gen0(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob("../internal/wrapper/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(p)] = b
	}
	if len(files) != 4 {
		t.Fatalf("gen 0 has %d files, want 4", len(files))
	}
	return files
}

func nonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "nonce-" + hex.EncodeToString(b)
}

// TestMutateEdit: an edit comes back as the changed file, with the
// rationale trimmed, the usage, the session id and the whole transcript.
func TestMutateEdit(t *testing.T) {
	d := newDriver(t)
	parent := gen0(t)
	m, err := d.Mutate(context.Background(), parent, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Changed, []string{"tunables.go"}) {
		t.Errorf("Changed %q, want [tunables.go]", m.Changed)
	}
	if want := string(parent["tunables.go"]) + "\n// fake edit\n"; string(m.Files["tunables.go"]) != want {
		t.Errorf("tunables.go after the edit:\n%s", m.Files["tunables.go"])
	}
	if !bytes.Equal(m.Files["hasher.go"], parent["hasher.go"]) || len(m.Files) != len(parent) || m.Extra != 0 {
		t.Errorf("files %d, extra %d, hasher.go changed %v", len(m.Files), m.Extra, !bytes.Equal(m.Files["hasher.go"], parent["hasher.go"]))
	}
	want := lineage.Usage{NumTurns: 3, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 7, DurationMS: 1234, CostUSD: 0.01}
	if m.Usage != want || m.Rationale != "Shortened acceptWait." || m.SessionID != "fake-session" {
		t.Errorf("usage %+v, rationale %q, session %q", m.Usage, m.Rationale, m.SessionID)
	}
	if !bytes.HasPrefix(m.Transcript, []byte(`{"claude_code_version"`)) || !bytes.Contains(m.Transcript, []byte(`"type":"result"`)) {
		t.Errorf("transcript:\n%s", m.Transcript)
	}
	if ents, _ := os.ReadDir(d.scratch); len(ents) != 0 {
		t.Errorf("scratch dir keeps %d entries after the call", len(ents))
	}
}

// TestMutateNoop: a session that edits nothing is no error, and Changed
// is empty.
func TestMutateNoop(t *testing.T) {
	d := newDriver(t)
	m, err := d.Mutate(context.Background(), gen0(t), "noop")
	if err != nil || len(m.Changed) != 0 {
		t.Fatalf("err %v, changed %q", err, m.Changed)
	}
}

// TestFakeDryRun: the fake's dry-run mode, which a real BuildPrompt prompt
// starts (deploy/orchestrator-smoke.sh): a lineage with no earlier attempt
// gets a comment, one earlier attempt a broken function, two a comment.
func TestFakeDryRun(t *testing.T) {
	d := newDriver(t)
	parent := gen0(t)
	attempt := "\n- Generation 1 (from 0): passed the gate and scored 0.500. Note: \"x\"\n"
	for _, c := range []struct {
		earlier int
		broken  bool
	}{{0, false}, {1, true}, {2, false}} {
		prompt := "You are the mutation operator.\nRecent attempts in this lineage:" + strings.Repeat(attempt, c.earlier)
		m, err := d.Mutate(context.Background(), parent, prompt)
		if err != nil {
			t.Fatal(err)
		}
		got := string(m.Files["tunables.go"])
		if !slices.Equal(m.Changed, []string{"tunables.go"}) || strings.Contains(got, "dryRunBreak") != c.broken ||
			!strings.HasSuffix(got, fmt.Sprintf("// after %d earlier attempts\n", c.earlier)) {
			t.Errorf("%d earlier: changed %q, tunables.go ends %q", c.earlier, m.Changed, got[len(got)-80:])
		}
	}
}

// TestSandboxEnvAndArgv: the session sees exactly the allowlisted
// environment, exactly claudeArgs and the sandbox's work directory.
func TestSandboxEnvAndArgv(t *testing.T) {
	d := newDriver(t)
	m, err := d.Mutate(context.Background(), gen0(t), "report")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Argv, Env []string
		Cwd       string
	}
	if err := json.Unmarshal([]byte(m.Rationale), &got); err != nil {
		t.Fatal(err)
	}
	var want []string
	env := sandboxEnv("tok-test")
	for i := 0; i < len(env); i += 2 {
		want = append(want, env[i]+"="+env[i+1])
	}
	want = append(want, "PWD="+sandboxWork) // bwrap's own, from --chdir
	slices.Sort(want)
	if !slices.Equal(got.Env, want) {
		t.Errorf("env\n%q\nwant\n%q", got.Env, want)
	}
	if !slices.Equal(got.Argv, claudeArgs("report")) {
		t.Errorf("argv %q", got.Argv)
	}
	if got.Cwd != sandboxWork {
		t.Errorf("cwd %q", got.Cwd)
	}
}

// TestSandboxTree: nothing of the fixed core or the repo is in the tree
// the session sees, and its top level holds only the sandbox's own
// mounts: no /home, /run, /var, /root or /opt (non-negotiable 1).
func TestSandboxTree(t *testing.T) {
	d := newDriver(t)
	m, err := d.Mutate(context.Background(), gen0(t), "probe")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Top, Found []string
		Walked     int
	}
	if err := json.Unmarshal([]byte(m.Rationale), &got); err != nil {
		t.Fatalf("%v: %s", err, m.Rationale)
	}
	if len(got.Found) != 0 {
		t.Errorf("the sandbox shows %q", got.Found)
	}
	allowed := []string{"sandbox", "usr", "lib", "lib64", "etc", "proc", "dev", "tmp"}
	for _, e := range got.Top {
		if !slices.Contains(allowed, e) {
			t.Errorf("the sandbox's / holds %q", e)
		}
	}
	if got.Walked < 10 {
		t.Errorf("walked %d paths", got.Walked)
	}
}

// TestInitRefused: a session whose init event shows another tool set is
// killed at once, long before the call's timeout.
func TestInitRefused(t *testing.T) {
	d := newDriver(t)
	d.timeout = time.Minute
	start := time.Now()
	m, err := d.Mutate(context.Background(), gen0(t), "badinit")
	if err == nil || !strings.Contains(err.Error(), "session tools") {
		t.Fatalf("err %v", err)
	}
	// Well under waitDelay: os/exec's own fallback kill must not be what
	// ends it.
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("refused after %v", el)
	}
	if m == nil || m.Files != nil {
		t.Errorf("mutation %+v", m)
	}
}

// TestNoInit: output that does not start with the init event is refused.
func TestNoInit(t *testing.T) {
	d := newDriver(t)
	if _, err := d.Mutate(context.Background(), gen0(t), "noinit"); err == nil || !strings.Contains(err.Error(), "session refused") {
		t.Fatalf("err %v", err)
	}
}

// waitGone fails unless no process's command line holds tok within 5 s.
func waitGone(t *testing.T, tok string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive []string
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err == nil && bytes.Contains(b, []byte(tok)) {
				alive = append(alive, e.Name())
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes %v still run with %s", alive, tok)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestTimeoutKillsAll: a session past its wall-clock timeout is killed
// with every process it started, and Mutate returns promptly.
func TestTimeoutKillsAll(t *testing.T) {
	d := newDriver(t)
	d.timeout = 2 * time.Second
	tok := nonce()
	start := time.Now()
	_, err := d.Mutate(context.Background(), gen0(t), "hang "+tok)
	if err == nil || !strings.Contains(err.Error(), "killed after") {
		t.Fatalf("err %v", err)
	}
	// Well under timeout+waitDelay: the group kill, not os/exec's
	// fallback after waitDelay, must end it.
	if el := time.Since(start); el > d.timeout+2*time.Second {
		t.Errorf("returned after %v", el)
	}
	waitGone(t, tok)
}

// TestCancelKillsAll: cancelling the caller's context kills the session
// the same way, and the error wraps context.Canceled.
func TestCancelKillsAll(t *testing.T) {
	d := newDriver(t)
	tok := nonce()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	_, err := d.Mutate(ctx, gen0(t), "hang "+tok)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	waitGone(t, tok)

	// Cancelled before the init event: still a cancel, not a refusal.
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	if _, err := d.Mutate(ctx, gen0(t), "slowinit"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel before init: %v", err)
	}
}

// TestKilledAfterResult: a session that prints its result and then
// hangs is killed, and its usage is still returned.
func TestKilledAfterResult(t *testing.T) {
	d := newDriver(t)
	d.timeout = 2 * time.Second
	m, err := d.Mutate(context.Background(), gen0(t), "resulthang")
	if err == nil || !strings.Contains(err.Error(), "killed after") {
		t.Fatalf("err %v", err)
	}
	if m == nil || m.Usage.NumTurns != 3 || m.Files != nil {
		t.Errorf("mutation %+v", m)
	}
}

// TestTokenNotInCmdline: while a session runs, no process's command line
// holds the token (it reaches bwrap through --args).
func TestTokenNotInCmdline(t *testing.T) {
	needBwrap(t)
	token := nonce()
	d, err := New(context.Background(), Config{Claude: fakeClaude, Version: fakeVersion, Token: token, ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d.timeout = 2 * time.Second
	tok := nonce()
	parent := gen0(t)
	done := make(chan struct{})
	go func() {
		d.Mutate(context.Background(), parent, "hang "+tok)
		close(done)
	}()
	sawSession := false
	for {
		select {
		case <-done:
			if !sawSession {
				t.Error("never saw the session's processes")
			}
			return
		default:
		}
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err != nil {
				continue
			}
			if bytes.Contains(b, []byte(token)) {
				t.Errorf("process %s has the token in its command line", e.Name())
			}
			sawSession = sawSession || bytes.Contains(b, []byte(tok))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSessionFailures: each way a session can fail is an error, with the
// usage kept when the session reached its result.
func TestSessionFailures(t *testing.T) {
	d := newDriver(t)
	d.maxOutput = 100 << 10
	for _, tc := range []struct {
		mode, want string
		usage      bool
	}{
		{"symlink", "copy-back of tunables.go", true},
		{"grow", "copy-back of tunables.go", true},
		{"maxturns", "error_max_turns", true},
		{"wrongmodel", "did not run on", true},
		{"exit1", "reported success but", true},
		{"noresult", "does not end in its one result event", false},
		{"flood", "output over", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			m, err := d.Mutate(context.Background(), gen0(t), tc.mode)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if m == nil || m.Files != nil || (m.Usage.NumTurns == 3) != tc.usage {
				t.Errorf("mutation files %v, usage %+v", m.Files != nil, m.Usage)
			}
		})
	}
}

// TestExtraIgnored: a file the session adds is never read back.
func TestExtraIgnored(t *testing.T) {
	d := newDriver(t)
	parent := gen0(t)
	m, err := d.Mutate(context.Background(), parent, "extra")
	if err != nil {
		t.Fatal(err)
	}
	if m.Extra != 1 || len(m.Changed) != 0 || len(m.Files) != len(parent) {
		t.Errorf("extra %d, changed %q, files %d", m.Extra, m.Changed, len(m.Files))
	}
}

// TestMutateRefusals: a bad parent or prompt, or a claude binary that
// changed since New, is refused before any sandbox starts.
func TestMutateRefusals(t *testing.T) {
	needBwrap(t)
	bin := filepath.Join(t.TempDir(), "claude")
	b, err := os.ReadFile(fakeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := New(context.Background(), Config{Claude: bin, Version: fakeVersion, Token: "t", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	with := func(name string, b []byte) map[string][]byte {
		m := gen0(t)
		m[name] = b
		return m
	}
	noHasher := gen0(t)
	delete(noHasher, "hasher.go")
	for name, tc := range map[string]struct {
		parent map[string][]byte
		prompt string
	}{
		"empty parent":   {map[string][]byte{}, "edit"},
		"no hasher.go":   {noHasher, "edit"},
		"test file":      {with("server_test.go", nil), "edit"},
		"path":           {with("../x.go", nil), "edit"},
		"dot file":       {with(".x.go", nil), "edit"},
		"not go":         {with("x.txt", nil), "edit"},
		"big file":       {with("big.go", make([]byte, lineage.MaxGenomeFile+1)), "edit"},
		"too many files": {manyFiles(lineage.MaxGenomeFiles + 1), "edit"},
		"empty prompt":   {gen0(t), ""},
		"long prompt":    {gen0(t), strings.Repeat("x", maxPromptBytes+1)},
		"NUL in prompt":  {gen0(t), "edit\x00"},
		"invalid UTF-8":  {gen0(t), "edit \xff"},
	} {
		if _, err := d.Mutate(context.Background(), tc.parent, tc.prompt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mutate(context.Background(), gen0(t), "edit"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("changed binary: %v", err)
	}
}

// manyFiles returns a genome of n files, hasher.go among them.
func manyFiles(n int) map[string][]byte {
	m := map[string][]byte{"hasher.go": []byte("package wrapper\n")}
	for i := 1; len(m) < n; i++ {
		m[fmt.Sprintf("f%d.go", i)] = []byte("package wrapper\n")
	}
	return m
}

// TestMutateLimits: a prompt, a file and a file count each exactly at
// their limit are accepted.
func TestMutateLimits(t *testing.T) {
	d := newDriver(t)
	big := gen0(t)
	big["big.go"] = make([]byte, lineage.MaxGenomeFile)
	for name, tc := range map[string]struct {
		parent map[string][]byte
		prompt string
	}{
		"prompt at limit": {gen0(t), "noop " + strings.Repeat("x", maxPromptBytes-len("noop "))},
		"file at limit":   {big, "noop"},
		"files at limit":  {manyFiles(lineage.MaxGenomeFiles), "noop"},
	} {
		m, err := d.Mutate(context.Background(), tc.parent, tc.prompt)
		if err != nil || len(m.Files) != len(tc.parent) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestNewRefusals: New refuses a bad config and a binary of another
// version.
func TestNewRefusals(t *testing.T) {
	needBwrap(t)
	dir := t.TempDir()
	ok := Config{Claude: fakeClaude, Version: fakeVersion, Token: "t", ScratchDir: dir}
	for name, mut := range map[string]func(*Config){
		"other version":  func(c *Config) { c.Version = "9.9.9" },
		"spaced version": func(c *Config) { c.Version = fakeVersion + " x" },
		"no token":       func(c *Config) { c.Token = "" },
		"token newline":  func(c *Config) { c.Token = "a\nb" },
		"relative dir":   func(c *Config) { c.ScratchDir = "scratch" },
		"missing dir":    func(c *Config) { c.ScratchDir = filepath.Join(dir, "none") },
		"missing binary": func(c *Config) { c.Claude = filepath.Join(dir, "none") },
		"not executable": func(c *Config) { c.Claude = "driver_test.go" },
	} {
		c := ok
		mut(&c)
		if _, err := New(context.Background(), c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestNewResolvesSymlink: New runs the file a symlink points at.
func TestNewResolvesSymlink(t *testing.T) {
	needBwrap(t)
	link := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(fakeClaude, link); err != nil {
		t.Fatal(err)
	}
	d, err := New(context.Background(), Config{Claude: link, Version: fakeVersion, Token: "t", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(fakeClaude); d.Claude() != want {
		t.Errorf("Claude() %q, want %q", d.Claude(), want)
	}
}

// TestRealClaudeSandboxed: the installed claude, if any, starts in the
// sandbox and reports its version there (no token is used).
func TestRealClaudeSandboxed(t *testing.T) {
	needBwrap(t)
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude on PATH")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	ver, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if _, err := New(context.Background(), Config{Claude: path, Version: ver, Token: "unused", ScratchDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxOptions: the bwrap options unshare and drop what DESIGN.md
// sec 12 names, clear the environment before setting any of it, and
// bind only the call's directories, the binary and the system paths.
func TestSandboxOptions(t *testing.T) {
	opts, err := sandboxOptions("/s/work", "/s/home", "/s/claude", sandboxEnv("tok"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"--unshare-all", "--unshare-user", "--share-net", "--disable-userns", "--die-with-parent", "--new-session", "--clearenv"} {
		if !slices.Contains(opts, f) {
			t.Errorf("no %s", f)
		}
	}
	if i := slices.Index(opts, "--cap-drop"); i < 0 || opts[i+1] != "ALL" {
		t.Error("no --cap-drop ALL")
	}
	if slices.Index(opts, "--clearenv") > slices.Index(opts, "--setenv") {
		t.Error("--setenv before --clearenv")
	}
	allowed := append(append([]string{"/s/work", "/s/home", "/s/claude"}, libPaths...), etcPaths...)
	for i, o := range opts {
		switch o {
		case "--bind", "--ro-bind", "--ro-bind-try", "--bind-try", "--dev-bind", "--dev-bind-try":
			if !slices.Contains(allowed, opts[i+1]) {
				t.Errorf("%s %s", o, opts[i+1])
			}
		}
	}
	for _, p := range etcPaths {
		if p == "/etc/ssl" || p == "/etc/pki" || strings.Contains(p, "private") {
			t.Errorf("binds %s, which may hold private keys", p)
		}
	}
	for _, p := range allowed {
		for _, never := range []string{"/home", "/run", "/var", "/root", "/opt"} {
			if p == never || strings.HasPrefix(p, never+"/") {
				t.Errorf("system path %s is under %s", p, never)
			}
		}
	}
}

// TestSandboxEnv: the environment is exactly the allowlist (DESIGN.md
// sec 12), and nothing that must never be present is.
func TestSandboxEnv(t *testing.T) {
	env := sandboxEnv("tok")
	var names []string
	for i := 0; i < len(env); i += 2 {
		names = append(names, env[i])
	}
	slices.Sort(names)
	want := []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_RESTRICTED", "DISABLE_AUTOUPDATER", "HOME", "LANG", "PATH", "TMPDIR"}
	if !slices.Equal(names, want) {
		t.Errorf("env names %q", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "ANTHROPIC_") || n == "KUBECONFIG" || n == "DOCKER_HOST" || n == "SSH_AUTH_SOCK" || strings.HasSuffix(n, "_TOKEN") && n != "CLAUDE_CODE_OAUTH_TOKEN" {
			t.Errorf("env holds %s", n)
		}
	}
}

// TestClaudeArgs: --tools "Read,Edit" with --restricted (non-negotiable
// 3), never the permission-only tool flags, the pinned model, and the
// prompt last, after "--", even when it looks like a flag.
func TestClaudeArgs(t *testing.T) {
	args := claudeArgs("--dangerously-skip-permissions")
	if i := slices.Index(args, "--tools"); i < 0 || args[i+1] != "Read,Edit" {
		t.Error(`no --tools "Read,Edit"`)
	}
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != PinnedModel {
		t.Error("no pinned --model")
	}
	for flag, val := range map[string]string{
		"--permission-mode": "acceptEdits", "--permission-prompts": "none",
		"--max-turns": fmt.Sprint(maxTurns), "--output-format": "stream-json",
	} {
		if i := slices.Index(args, flag); i < 0 || args[i+1] != val {
			t.Errorf("no %s %s", flag, val)
		}
	}
	for _, f := range []string{"-p", "--restricted", "--strict-mcp-config", "--no-session-persistence", "--verbose"} {
		if !slices.Contains(args, f) {
			t.Errorf("no %s", f)
		}
	}
	for _, f := range args[:len(args)-1] {
		if strings.Contains(strings.ToLower(f), "allowed") || strings.Contains(f, "dangerously") || f == "--bare" {
			t.Errorf("argv holds %s", f)
		}
	}
	if args[len(args)-2] != "--" || slices.Index(args, "--") != len(args)-2 {
		t.Error(`the prompt does not follow a single "--"`)
	}
}

// TestCheckInit: every field the init check reads is enforced.
func TestCheckInit(t *testing.T) {
	good := map[string]any{
		"type": "system", "subtype": "init", "cwd": sandboxWork, "tools": []string{"Read", "Edit"},
		"mcp_servers": []any{}, "model": PinnedModel, "permissionMode": "acceptEdits", "claude_code_version": "1.2.3",
	}
	line := func(k string, v any) []byte {
		m := make(map[string]any)
		for kk, vv := range good {
			m[kk] = vv
		}
		if k != "" {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	if err := checkInit(line("", nil), "1.2.3"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		k string
		v any
	}{
		{"type", "assistant"}, {"subtype", "x"}, {"cwd", "/home/u/repo"},
		{"tools", []string{"Read"}}, {"tools", []string{"Read", "Edit", "Bash"}}, {"tools", []string{"Read", "Write"}},
		{"mcp_servers", []any{map[string]any{"name": "x"}}}, {"model", "claude-haiku-5-5"},
		{"permissionMode", "bypassPermissions"}, {"claude_code_version", "1.2.4"},
	} {
		if err := checkInit(line(tc.k, tc.v), "1.2.3"); err == nil {
			t.Errorf("%s=%v passed", tc.k, tc.v)
		}
	}
	if err := checkInit([]byte("not json"), "1.2.3"); err == nil {
		t.Error("non-JSON passed")
	}
}

// TestParseStream: the result must be the last line and the only one.
func TestParseStream(t *testing.T) {
	init := `{"type":"system","subtype":"init"}`
	res := `{"type":"result","subtype":"success","num_turns":2,"usage":{"input_tokens":3}}`
	for name, tc := range map[string]struct {
		out string
		ok  bool
	}{
		"good":            {init + "\n" + res + "\n", true},
		"no newline":      {init + "\n" + res, true},
		"empty":           {"", false},
		"no result":       {init + "\n", false},
		"result not last": {res + "\n" + init + "\n", false},
		"two results":     {init + "\n" + res + "\n" + res + "\n", false},
		"junk line":       {init + "\nxx\n" + res + "\n", false},
		"untyped line":    {init + "\n{}\n" + res + "\n", false},
	} {
		r, err := parseStream([]byte(tc.out))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v", name, err)
		}
		if tc.ok && (r.NumTurns != 2 || r.Usage.InputTokens != 3) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	var r resultEvent
	r.Usage.OutputTokens = -1
	if _, err := r.usage(); err == nil {
		t.Error("negative usage passed")
	}
}

// TestCut: cuts land on rune boundaries and fix invalid UTF-8.
func TestCut(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"}, {"abcdef", 3, "abc"}, {"aé", 2, "a"}, {"a\xffb", 10, "a�b"},
	} {
		if got := cut(tc.in, tc.n); got != tc.want {
			t.Errorf("cut(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestWrapperImports: the prompt's import list is gate Stage 1's.
func TestWrapperImports(t *testing.T) {
	if got, want := slices.Sorted(slices.Values(wrapperImports)), slices.Sorted(slices.Values(gate.AllowedImports())); !slices.Equal(got, want) {
		t.Errorf("driver lists %q, gate allows %q", got, want)
	}
}

// TestDeps: the package's build depends on the standard library and
// lineage, fitness, internal/classifier and loadgen/wire only: never on
// gate, internal/hashcore, the test-vector pool or an Argon2 package.
func TestDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const mod = "github.com/codeinthecreek/eK8s"
	ok := map[string]bool{
		mod + "/driver": true, mod + "/lineage": true, mod + "/fitness": true,
		mod + "/internal/classifier": true, mod + "/loadgen/wire": true,
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(l, " ")
		if std != "true" && !ok[path] {
			t.Errorf("driver's build depends on %s", path)
		}
	}
}

// TestLiveMutation runs one real session (a Sonnet call against the
// owner's quota) on generation 0: only with EK8S_DRIVER_LIVE=1, a
// CLAUDE_CODE_OAUTH_TOKEN and claude on PATH. It is the workstation check
// that the real CLI passes checkInit, edits through acceptEdits and
// reports usage (docs/STATUS.md).
func TestLiveMutation(t *testing.T) {
	if os.Getenv("EK8S_DRIVER_LIVE") != "1" {
		t.Skip("EK8S_DRIVER_LIVE is not 1")
	}
	needBwrap(t)
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	path, err := exec.LookPath("claude")
	if token == "" || err != nil {
		t.Fatal("needs CLAUDE_CODE_OAUTH_TOKEN and claude on PATH")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	ver, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	d, err := New(context.Background(), Config{Claude: path, Version: ver, Token: token, ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := BuildPrompt([]lineage.Attempt{gen0Attempt()}, "0", testParams())
	if err != nil {
		t.Fatal(err)
	}
	parent := gen0(t)
	m, err := d.Mutate(context.Background(), parent, prompt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("changed %q, extra %d, usage %+v\nrationale: %s", m.Changed, m.Extra, m.Usage, m.Rationale)
	if !bytes.Equal(m.Files["hasher.go"], parent["hasher.go"]) {
		t.Error("the session edited hasher.go")
	}
	if len(m.Changed) == 0 || m.Usage.NumTurns == 0 || m.Rationale == "" {
		t.Error("no edit, turns or rationale")
	}
}
