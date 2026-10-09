package gate

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

// requireToolsEnv, set to 1 (CI does), turns a missing bwrap, nsenter or
// docker daemon from a skip into a test failure.
const requireToolsEnv = "EK8S_GATE_REQUIRE_TOOLS"

var allChecks = append(slices.Clone(preBuildChecks),
	"build", "boot", "docker",
	"mutanthash-realverify-match", "realhash-mutantverify-match",
	"mutanthash-realverify-nomatch", "realhash-mutantverify-nomatch",
	"password-bounds", "contract", "cost-params")

// setupHost skips the test (or fails it, under requireToolsEnv) unless
// the host has bwrap and nsenter. Without a docker daemon it stubs the
// docker sub-check out, so Stages 2-3 still run; it reports whether the
// real sub-check runs.
func setupHost(t *testing.T) (realDocker bool) {
	t.Helper()
	required := os.Getenv(requireToolsEnv) == "1"
	for _, tool := range hostTools {
		if _, err := exec.LookPath(tool); err != nil {
			if required {
				t.Fatalf("%s=1 but %s is missing", requireToolsEnv, tool)
			}
			t.Skipf("%s not on PATH", tool)
		}
	}
	if exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run() == nil {
		return true
	}
	if required {
		t.Fatalf("%s=1 but there is no docker daemon", requireToolsEnv)
	}
	t.Logf("no docker daemon: docker sub-check stubbed")
	dockerSubCheck = func(context.Context, string, string, string) (dockerImage, string, error) {
		return dockerImage{}, "", nil
	}
	t.Cleanup(func() { dockerSubCheck = checkDocker })
	return false
}

// wrapperImages lists the host's ek8s-wrapper images.
func wrapperImages(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", "ek8s-wrapper").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func TestGen0Passes(t *testing.T) {
	realDocker := setupHost(t)
	var before []string
	if realDocker {
		before = wrapperImages(t)
	}
	rep := run(t, gen0(t))
	if !rep.Passed() || !slices.Equal(checks(rep), allChecks) {
		t.Fatalf("gen 0: %+v", rep.Results)
	}
	if !realDocker {
		return
	}
	if !slices.Contains(before, rep.Image) {
		// Only an image this test created: an identical binary's image
		// may already be in use.
		t.Cleanup(func() { removeImage(dockerImage{tag: rep.Image}) })
	}
	if !strings.HasPrefix(rep.Image, "ek8s-wrapper:") || !slices.Contains(wrapperImages(t), rep.Image) {
		t.Fatalf("passing image %q not kept", rep.Image)
	}
}

// TestStageFailures: each mutant passes the pre-build checks and must
// fail exactly at failCheck.
func TestStageFailures(t *testing.T) {
	realDocker := setupHost(t)
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("mutation target not found: " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	tests := []struct {
		name, file string
		mutate     func(string) string
		failCheck  string
		detail     string
	}{
		{"does not compile", "tunables.go",
			func(s string) string { return s + "\nfunc broken() int { return \"x\" }\n" },
			"build", "go build"},
		{"exits at startup", "server.go",
			replace("func New(h Hasher, env Env) *Server {", "func New(h Hasher, env Env) *Server {\n\tpanic(\"mutant boom\")"),
			"boot", "mutant boom"},
		{"replays one hash", "server.go",
			replace("hash, err := s.h.Hash(req.Password)", "if s.cached == \"\" {\n\t\t\ts.cached, _ = s.h.Hash(req.Password)\n\t\t}\n\t\thash, err := s.cached, error(nil)"),
			"mutanthash-realverify-match", "input 2"},
		{"drops the variant header", "server.go",
			replace("return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n\t\tvar req struct {\n\t\t\tPassword string `json:\"password\"`\n\t\t}",
				"return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n\t\tw = w.(interface{ Unwrap() http.ResponseWriter }).Unwrap()\n\t\tvar req struct {\n\t\t\tPassword string `json:\"password\"`\n\t\t}"),
			"mutanthash-realverify-match", "X-EK8s-Variant"},
		{"sheds everything", "server.go",
			replace("case s.slots <- struct{}{}:\n\t\treturn true", "case <-make(chan struct{}):\n\t\treturn true"),
			"mutanthash-realverify-match", "graceful_shed"},
		{"verify always matches", "server.go",
			replace("match, err := s.h.Verify(req.Password, req.Hash)", "_, err := s.h.Verify(req.Password, req.Hash)\n\t\tmatch := true"),
			"realhash-mutantverify-nomatch", "wrong_match"},
		{"fails open on a Verify error", "server.go",
			replace("writeError(w, http.StatusBadRequest, \"invalid hash\")", "writeJSON(w, http.StatusOK, struct {\n\t\t\t\tMatch bool `json:\"match\"`\n\t\t\t}{true})"),
			"contract", "2 verify non-exact params"},
		// /hash only (the first validPassword call): the pool sends 1-byte
		// and 1024-byte passwords to /verify.
		{"hash rejects 1024-byte passwords", "server.go",
			replace("if !validPassword(req.Password) {", "if !validPassword(req.Password) || len(req.Password) == maxPasswordBytes {"),
			"password-bounds", "1024-byte password: hash failed (bad_status, status 400"},
		{"hash rejects 1-byte passwords", "server.go",
			replace("if !validPassword(req.Password) {", "if !validPassword(req.Password) || len(req.Password) == 1 {"),
			"password-bounds", "1-byte password: hash failed (bad_status, status 400"},
		{"accepts long passwords", "server.go",
			replace("const maxPasswordBytes = 1024", "const maxPasswordBytes = 2048"),
			"contract", "4 hash password over 1024 bytes: status 200"},
		{"ignores trailing data", "server.go",
			replace("if _, err := dec.Token(); err != io.EOF {", "if false {"),
			"contract", "5 hash trailing object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gen0(t)
			edit(t, dir, tt.file, tt.mutate)
			if tt.name == "replays one hash" {
				edit(t, dir, "server.go", replace("slots chan struct{}", "slots chan struct{}\n\tcached string"))
			}
			var before []string
			if realDocker {
				before = wrapperImages(t)
			}
			rep := run(t, dir)
			if realDocker && !slices.Equal(wrapperImages(t), before) {
				t.Errorf("failed candidate's image kept: %v, before %v", wrapperImages(t), before)
			}
			if rep.Passed() || len(rep.Results) == 0 || rep.Image != "" {
				t.Fatalf("passed: %+v", rep.Results)
			}
			last := rep.Results[len(rep.Results)-1]
			if last.Pass || last.Check != tt.failCheck || !strings.Contains(last.Detail, tt.detail) {
				t.Fatalf("got %s %q, want %s containing %q", last.Check, last.Detail, tt.failCheck, tt.detail)
			}
			// Boot and Stage 2 failures carry the candidate's output (its
			// startup log line at least).
			if (last.Check == "boot" || last.Stage == 2) && !strings.Contains(last.Detail, "candidate output:\n") {
				t.Fatalf("no candidate output in %q", last.Detail)
			}
		})
	}
}

// TestContainerOnlyExit: a candidate that exits only outside the bare
// sandbox (it reads the hostname through http.Dir) fails the docker
// sub-check as a Result, not as a gate fault.
func TestContainerOnlyExit(t *testing.T) {
	if !setupHost(t) {
		t.Skip("no docker daemon")
	}
	dir := gen0(t)
	edit(t, dir, "server.go", func(s string) string {
		return strings.Replace(s, "func New(h Hasher, env Env) *Server {", `func New(h Hasher, env Env) *Server {
	f, err := http.Dir("/proc/sys/kernel").Open("hostname")
	if err != nil {
		panic(err)
	}
	if b, _ := io.ReadAll(f); len(b) < 7 || string(b[:7]) != "hasher-" {
		panic("not the gate's sandbox")
	}`, 1)
	})
	before := wrapperImages(t)
	rep := run(t, dir)
	last := rep.Results[len(rep.Results)-1]
	if rep.Passed() || last.Check != "docker" || !strings.Contains(last.Detail, "container") {
		t.Fatalf("%+v", rep.Results)
	}
	if !slices.Equal(wrapperImages(t), before) {
		t.Errorf("failed candidate's image kept")
	}
}

// TestNoMatchUsesRealVerify: mutanthash-realverify-nomatch fails if the
// real library accepts a different password for the candidate's hash.
// No real candidate can cause that, so the library is swapped for one
// that accepts everything (which mutanthash-realverify-match passes).
func TestNoMatchUsesRealVerify(t *testing.T) {
	setupHost(t)
	realVerify = func(string, string) (bool, error) { return true, nil }
	t.Cleanup(func() { realVerify = hashcore.New().Verify })
	rep := run(t, gen0(t))
	last := rep.Results[len(rep.Results)-1]
	if rep.Passed() || last.Check != "mutanthash-realverify-nomatch" || !strings.Contains(last.Detail, "real Verify of a different password") {
		t.Fatalf("%+v", rep.Results)
	}
}

// TestContractCasesCoverDesign keeps contractCases holding every case
// DESIGN.md sec 7 Stage 2 lists.
func TestContractCasesCoverDesign(t *testing.T) {
	var names []string
	for _, c := range contractCases() {
		names = append(names, c.name)
	}
	want := []string{
		"1 verify malformed hash",
		"3 verify empty hash", "3 hash empty password", "3 hash missing password",
		"3 verify empty password", "3 verify missing password",
		"4 hash password over 1024 bytes", "4 verify password over 1024 bytes",
		"5 hash invalid JSON", "5 verify invalid JSON",
		"5 hash password not a string", "5 verify hash not a string",
		"5 hash trailing object", "5 verify trailing garbage",
	}
	for _, n := range testvectors.Negatives() {
		want = append(want, "2 verify non-exact params "+n.Name)
	}
	for _, w := range want {
		if !slices.Contains(names, w) {
			t.Errorf("missing contract case %q", w)
		}
	}
	var t1, bigM bool
	for _, n := range testvectors.Negatives() {
		t1 = t1 || strings.Contains(n.Hash, ",t=1,")
		bigM = bigM || n.Constructed
	}
	if !t1 || !bigM {
		t.Errorf("negatives lack a t=1 (%v) or a constructed large-m (%v) entry", t1, bigM)
	}
}

func TestCheckVariant(t *testing.T) {
	for _, tt := range []struct {
		values []string
		ok     bool
	}{
		{[]string{"gate-1"}, true},
		{nil, false},
		{[]string{"gate-2"}, false},
		{[]string{"gate-1", "gate-1"}, false},
		{[]string{"gate-1", "x"}, false},
	} {
		h := http.Header{}
		for _, v := range tt.values {
			h.Add(variantHeader, v)
		}
		if got := checkVariant(h, "gate-1") == ""; got != tt.ok {
			t.Errorf("%q: ok=%v, want %v", tt.values, got, tt.ok)
		}
	}
}

// TestMissingHostToolIsGateFault: without bwrap or nsenter, Run errors
// instead of failing the candidate.
func TestMissingHostToolIsGateFault(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	bin := t.TempDir()
	if err := os.Symlink(gobin, filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	rep, err := Run(context.Background(), Config{RepoRoot: repoRoot(t), CandidateDir: gen0(t)})
	if err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("err %v, report %+v", err, rep.Results)
	}
	if last := rep.Results[len(rep.Results)-1]; last.Check != "build" || !last.Pass {
		t.Fatalf("results before the fault: %+v", rep.Results)
	}
}

func TestCostParams(t *testing.T) {
	good := testvectors.Pairs()[0].Hash
	if p := checkCostParams([]string{good}); p != "" {
		t.Fatalf("pool hash: %s", p)
	}
	if p := checkCostParams(nil); p == "" {
		t.Fatal("no hashes passed")
	}
	_, rest, _ := strings.Cut(strings.TrimPrefix(good, "$argon2id$v=19$m=19456,t=2,p=1"), "$")
	tail := "$" + rest
	for params, want := range map[string]string{
		"m=19456,t=2,p=1":         "",
		"m=019456,t=2,p=1":        "", // numeric: canonical form is the classifier's job
		"t=2,m=19456,p=1":         "",
		"m=19456,t=1,p=1":         "t must be exactly 2",
		"m=19455,t=2,p=1":         "m must be exactly 19456",
		"m=19456,t=2,p=2":         "p must be exactly 1",
		"m=19456,t=2":             "want exactly m, t, p",
		"m=19456,t=2,p=1,x=1":     "want exactly m, t, p",
		"m=19456,t=2,t=2":         "unparseable",
		"m=19456,t=2,p=-1":        "unparseable",
		"m=99999999999,t=2,p=1":   "unparseable",
		"m=19456;t=2,p=1":         "unparseable",
		"m=19456,t=2,p=1,m=19456": "unparseable",
	} {
		got := costParams("$argon2id$v=19$" + params + tail)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", params, got, want)
		}
	}
	for _, h := range []string{"", "$argon2i$v=19$m=19456,t=2,p=1" + tail, "argon2id$v=19$m=19456,t=2,p=1" + tail, good + "$x"} {
		if costParams(h) == "" {
			t.Errorf("%q passed", h)
		}
	}
}

// TestRelayExitsOnStdinEOF: the relay ends when its stdin closes, which
// is what happens when the gate dies however it dies (gate/internal/relay).
func TestRelayExitsOnStdinEOF(t *testing.T) {
	tc, err := resolveToolchain(context.Background(), repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay")
	if ok, out, err := goBuild(context.Background(), tc, repoRoot(t), relayPkg, relay); !ok || err != nil {
		t.Fatalf("build relay: %v %s", err, out)
	}
	cmd := exec.Command(relay, filepath.Join(dir, "r.sock"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("relay printed %q", line)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("relay exited with stdin open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	stdin.Close()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("relay exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay still running 5s after stdin closed")
	}
}

func TestBoundaryPasswords(t *testing.T) {
	pool := make(map[string]bool)
	for _, p := range testvectors.Pairs() {
		pool[p.Password] = true
	}
	for range 50 { // the 1-byte draw collides with the pool's 1-byte password about 1 time in 32
		got := boundaryPasswords()
		if len(got) != 2 || len(got[0]) != 1 || len(got[1]) != maxPasswordBytes {
			t.Fatalf("lengths %d, %d", len(got[0]), len(got[1]))
		}
		for _, p := range got {
			if pool[p] {
				t.Fatalf("pool password %q drawn", p)
			}
		}
	}
}

// TestPoolSize keeps the pool large enough for poolPairs.
func TestPoolSize(t *testing.T) {
	poolPairs() // panics if too small
	if len(testvectors.Negatives()) < 2 {
		t.Fatal("contract case 2 needs the t=1 and large-m negatives")
	}
}

func TestCappedBuffer(t *testing.T) {
	var c cappedBuffer
	c.Write([]byte(strings.Repeat("a", maxOutputBytes-1)))
	if n, err := c.Write([]byte("bcd")); n != 3 || err != nil {
		t.Fatalf("Write: %d %v", n, err)
	}
	if s := c.String(); !strings.HasSuffix(s, "ab\n[2 more bytes dropped]") {
		t.Fatalf("tail %q", s[len(s)-40:])
	}
}
