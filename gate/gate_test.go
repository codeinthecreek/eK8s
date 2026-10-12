package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// repoRoot is the repo checkout the tests run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// gen0 copies the repo's generation-0 wrapper (its non-test .go files)
// into a fresh candidate directory.
func gen0(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := copyGoFiles(filepath.Join(repoRoot(t), wrapperPkg), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// edit rewrites one candidate file.
func edit(t *testing.T, dir, name string, f func(string) string) {
	t.Helper()
	p := filepath.Join(dir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(f(string(b))), 0o644); err != nil {
		t.Fatal(err)
	}
}

// must fails the test on a setup error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// addImport adds a blank import to doc.go, which has no declarations.
func addImport(path string) func(string) string {
	return func(s string) string { return s + "\nimport _ \"" + path + "\"\n" }
}

func run(t *testing.T, dir string) Report {
	t.Helper()
	rep, err := Run(context.Background(), Config{RepoRoot: repoRoot(t), CandidateDir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func checks(rep Report) []string {
	var out []string
	for _, r := range rep.Results {
		out = append(out, r.Check)
	}
	return out
}

var preBuildChecks = []string{"candidate-files", "hasher-pin", "build-constraints", "go-list", "imports", "package-files"}

// preBuildReport runs only Stage 1's pre-build checks on dir, so it needs
// no bwrap, nsenter or docker.
func preBuildReport(t *testing.T, dir string) Report {
	t.Helper()
	var rep Report
	_, tree, _, err := preBuild(context.Background(), Config{RepoRoot: repoRoot(t), CandidateDir: dir}, &rep)
	if tree != "" {
		os.RemoveAll(tree)
	}
	if err != nil {
		t.Fatalf("preBuild: %v", err)
	}
	return rep
}

func TestGen0PassesPreBuild(t *testing.T) {
	rep := preBuildReport(t, gen0(t))
	if !rep.Passed() || !slices.Equal(checks(rep), preBuildChecks) {
		t.Fatalf("gen 0: %+v", rep.Results)
	}
}

// TestRepoHasherMatchesPin keeps hasherSHA256 equal to the repo's
// hasher.go.
func TestRepoHasherMatchesPin(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), wrapperPkg, "hasher.go"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != hasherSHA256 {
		t.Fatalf("internal/wrapper/hasher.go sha256 %s, pin %s", got, hasherSHA256)
	}
}

// TestRepoWrapperFileSet keeps candidateFiles equal to gen 0's non-test
// .go files.
func TestRepoWrapperFileSet(t *testing.T) {
	entries, err := os.ReadDir(gen0(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, candidateFiles) {
		t.Fatalf("gen 0 files %v, candidateFiles %v", got, candidateFiles)
	}
}

func TestRejects(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, dir string)
		failCheck string
		detail    string // substring of the failing Result's Detail
	}{
		{"extra test file", func(t *testing.T, dir string) {
			must(t, os.WriteFile(filepath.Join(dir, "server_test.go"), []byte("package wrapper\n"), 0o644))
		}, "candidate-files", `unexpected entry "server_test.go"`},
		{"extra non-go file", func(t *testing.T, dir string) {
			must(t, os.WriteFile(filepath.Join(dir, "vectors.json"), nil, 0o644))
		}, "candidate-files", `unexpected entry "vectors.json"`},
		{"missing file", func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, "tunables.go")))
		}, "candidate-files", "missing tunables.go"},
		{"subdirectory", func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
		}, "candidate-files", `unexpected entry "sub"`},
		{"symlink", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "doc.go")
			must(t, os.Rename(p, p+".real"))
			must(t, os.Symlink(p+".real", p))
			must(t, os.Remove(p+".real")) // dangling: must not be followed either way
		}, "candidate-files", "doc.go: not a regular file"},
		{"oversize", func(t *testing.T, dir string) {
			edit(t, dir, "doc.go", func(s string) string {
				return s + "//" + strings.Repeat("x", maxCandidateFileBytes) + "\n"
			})
		}, "candidate-files", "doc.go: larger than"},
		{"hasher.go edited", func(t *testing.T, dir string) {
			edit(t, dir, "hasher.go", func(s string) string { return s + "\n" })
		}, "hasher-pin", "hasher.go sha256"},
		{"go:build line", func(t *testing.T, dir string) {
			edit(t, dir, "tunables.go", func(s string) string { return "//go:build !race\n\n" + s })
		}, "build-constraints", "tunables.go:1"},
		{"+build line", func(t *testing.T, dir string) {
			edit(t, dir, "doc.go", func(s string) string { return s + "\n// +build linux\n" })
		}, "build-constraints", "doc.go:"},
		{"wrong package name", func(t *testing.T, dir string) {
			edit(t, dir, "doc.go", func(s string) string { return strings.Replace(s, "package wrapper", "package other", 1) })
		}, "package-files", "InvalidGoFiles"},
	}
	forbidden := []string{
		modulePath + "/internal/hashcore",
		modulePath + "/internal/classifier/testvectors",
		"golang.org/x/crypto/argon2",
		"github.com/alexedwards/argon2id",
		"crypto/subtle",
		"crypto/sha256",
		"hash/crc32",
		"unsafe",
		"os",
		"os/exec",
		"syscall",
		"plugin",
		"embed",
		"reflect",
		"net",
	}
	for _, imp := range forbidden {
		tests = append(tests, struct {
			name      string
			mutate    func(t *testing.T, dir string)
			failCheck string
			detail    string
		}{"import " + imp, func(t *testing.T, dir string) {
			edit(t, dir, "doc.go", addImport(imp))
		}, "imports", `import "` + imp + `"`})
	}
	// "C" is not listed under CGO_ENABLED=0 (the file is ignored), but is
	// under the race listing.
	tests = append(tests, struct {
		name      string
		mutate    func(t *testing.T, dir string)
		failCheck string
		detail    string
	}{"import C", func(t *testing.T, dir string) {
		edit(t, dir, "doc.go", func(s string) string { return s + "\nimport \"C\"\n" })
	}, "imports", `CGO_ENABLED=1 -race -tags netgo,osusergo: import "C"`})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gen0(t)
			tt.mutate(t, dir)
			rep := run(t, dir)
			if rep.Passed() || len(rep.Results) == 0 {
				t.Fatalf("passed: %+v", rep.Results)
			}
			last := rep.Results[len(rep.Results)-1]
			if last.Pass || last.Check != tt.failCheck || !strings.Contains(last.Detail, tt.detail) {
				t.Fatalf("got %s %q, want %s containing %q", last.Check, last.Detail, tt.failCheck, tt.detail)
			}
			for _, r := range rep.Results[:len(rep.Results)-1] {
				if !r.Pass {
					t.Fatalf("failure before the last Result: %+v", rep.Results)
				}
			}
		})
	}
}

func TestAllowedImports(t *testing.T) {
	for _, imp := range []string{"runtime/debug", "sync/atomic", "math/rand/v2", "container/heap"} {
		t.Run(imp, func(t *testing.T) {
			dir := gen0(t)
			edit(t, dir, "doc.go", addImport(imp))
			if rep := preBuildReport(t, dir); !rep.Passed() {
				t.Fatalf("%+v", rep.Results)
			}
		})
	}
}

func TestBuildConstraintLines(t *testing.T) {
	for line, want := range map[string]bool{
		"//go:build linux":        true,
		"  //go:build race":       true,
		"// +build linux":         true,
		"//+build linux":          true,
		"// a +build mention":     false,
		"// go:build is spaced":   false,
		"x := 1 //go:build":       false,
		"\uFEFF//go:build ignore": true,
	} {
		files := map[string][]byte{"doc.go": []byte("package wrapper\n" + line + "\n")}
		if got := checkBuildConstraints(files) != ""; got != want {
			t.Errorf("%q: rejected=%v, want %v", line, got, want)
		}
	}
}

// listGen0 assembles gen 0 and lists it under the first listing with tc.
func listGen0(t *testing.T, ctx context.Context, tc toolchain) (string, error) {
	t.Helper()
	files, detail, err := readCandidate(gen0(t))
	if err != nil || detail != "" {
		t.Fatalf("readCandidate: %v %q", err, detail)
	}
	tree, err := assemble(repoRoot(t), files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tree) })
	_, problem, err := goList(ctx, tc, tree, listings[0])
	return problem, err
}

// TestEnvNotInherited: an unknown GOEXPERIMENT fails every go command, so
// go list passing means neither the process environment nor a go env file
// at the process's XDG_CONFIG_HOME reached it.
func TestEnvNotInherited(t *testing.T) {
	tc, err := resolveToolchain(context.Background(), repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("environment", func(t *testing.T) {
		t.Setenv("GOEXPERIMENT", "bogus")
		t.Setenv("GOFLAGS", "-mod=vendor")
		if problem, err := listGen0(t, context.Background(), tc); err != nil || problem != "" {
			t.Fatalf("%v %q", err, problem)
		}
	})
	t.Run("go env file", func(t *testing.T) {
		xdg := t.TempDir()
		cfg := filepath.Join(xdg, "go")
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "env"), []byte("GOEXPERIMENT=bogus\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if problem, err := listGen0(t, context.Background(), tc); err != nil || problem != "" {
			t.Fatalf("%v %q", err, problem)
		}
	})
}

func TestEnvKeys(t *testing.T) {
	var keys []string
	for _, kv := range (toolchain{}).env(false) {
		keys = append(keys, strings.SplitN(kv, "=", 2)[0])
	}
	want := []string{"PATH", "GOENV", "GOFLAGS", "GOPROXY", "GOWORK", "GOTOOLCHAIN",
		"GOOS", "GOARCH", "CGO_ENABLED", "GOMODCACHE", "GOCACHE"}
	if !slices.Equal(keys, want) {
		t.Fatalf("env keys %v, want %v", keys, want)
	}
}

// TestCancelledIsError: a go list killed by its context exits with an
// ExitError, but is a gate fault, not a candidate failure.
func TestCancelledIsError(t *testing.T) {
	slow := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := toolchain{goBin: slow, path: os.Getenv("PATH")}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if problem, err := listGen0(t, ctx, tc); err == nil {
		t.Fatalf("no error; problem %q", problem)
	}
}

func TestCheckPackageFiles(t *testing.T) {
	good := goPackage{GoFiles: []string{"tunables.go", "doc.go", "server.go", "hasher.go"}}
	if d := checkPackageFiles([]goPackage{good, good}); d != "" {
		t.Fatalf("good package: %s", d)
	}
	for name, tt := range map[string]struct {
		mut  func(*goPackage)
		want string
	}{
		"missing GoFile": {func(p *goPackage) { p.GoFiles = p.GoFiles[1:] }, "GoFiles"},
		"cgo file":       {func(p *goPackage) { p.CgoFiles = []string{"doc.go"} }, "CgoFiles"},
		"ignored file":   {func(p *goPackage) { p.IgnoredGoFiles = []string{"x.go"} }, "IgnoredGoFiles"},
		"invalid file":   {func(p *goPackage) { p.InvalidGoFiles = []string{"x.go"} }, "InvalidGoFiles"},
		"test file":      {func(p *goPackage) { p.TestGoFiles = []string{"x_test.go"} }, "TestGoFiles"},
		"xtest file":     {func(p *goPackage) { p.XTestGoFiles = []string{"x_test.go"} }, "XTestGoFiles"},
		"load error":     {func(p *goPackage) { p.Error = &struct{ Err string }{"boom"} }, "boom"},
		"deps error":     {func(p *goPackage) { p.DepsErrors = []*struct{ Err string }{{"dep boom"}} }, "dependency: dep boom"},
	} {
		t.Run(name, func(t *testing.T) {
			// Only the race listing is bad: each listing is checked.
			bad := good
			bad.GoFiles = slices.Clone(good.GoFiles)
			tt.mut(&bad)
			d := checkPackageFiles([]goPackage{good, bad})
			if !strings.Contains(d, listings[1].name+": ") || !strings.Contains(d, tt.want) {
				t.Fatalf("detail %q, want %q from %s", d, tt.want, listings[1].name)
			}
		})
	}
}

// TestBuildConstraintScanError: a line longer than the scanner's buffer
// stops the scan; the rest is unchecked, so the check must fail. Real
// candidates cannot reach this (readCandidate caps files below the
// buffer), so it is called directly.
func TestBuildConstraintScanError(t *testing.T) {
	files := map[string][]byte{"doc.go": []byte(strings.Repeat("x", maxCandidateFileBytes+2) + "\n//go:build ignore\n")}
	if d := checkBuildConstraints(files); !strings.Contains(d, "doc.go: scan:") {
		t.Fatalf("detail %q", d)
	}
}

// TestBOMConstraintRejected: go/build strips a leading BOM, so a BOM must
// not hide a constraint from the lexical check.
func TestBOMConstraintRejected(t *testing.T) {
	dir := gen0(t)
	edit(t, dir, "tunables.go", func(s string) string { return "\uFEFF//go:build ignore\n\n" + s })
	rep := run(t, dir)
	last := rep.Results[len(rep.Results)-1]
	if rep.Passed() || last.Check != "build-constraints" {
		t.Fatalf("%+v", rep.Results)
	}
}

func TestReportPassed(t *testing.T) {
	if (Report{}).Passed() {
		t.Error("empty Report passed")
	}
}

func TestParseGoVersion(t *testing.T) {
	for s, want := range map[string]goVersion{
		"go1.26.9":    {1, 26, 9},
		"go1.26":      {1, 26, -2},
		"go1.27rc1":   {1, 27, -1},
		"go1.27beta2": {1, 27, -1},
		"go1.100.0":   {1, 100, 0},
	} {
		if got, ok := parseGoVersion(s); !ok || got != want {
			t.Errorf("%s: %v %v, want %v", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "1.26.9", "go1", "go1.26.9 X:foo", "devel go1.27", "go1.26.9.1"} {
		if _, ok := parseGoVersion(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
	if compareGoVersions(goVersion{1, 27, -1}, goVersion{1, 26, 9}) <= 0 ||
		compareGoVersions(goVersion{1, 27, -1}, goVersion{1, 27, 0}) >= 0 ||
		compareGoVersions(goVersion{1, 27, -2}, goVersion{1, 27, -1}) >= 0 {
		t.Error("compareGoVersions order")
	}
}

func TestMinGoVersion(t *testing.T) {
	got, err := minGoVersion(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil || compareGoVersions(got, goVersion{1, 26, 0}) <= 0 {
		t.Fatalf("repo go.mod: %v %v (want its toolchain line, above the go directive)", got, err)
	}
	dir := t.TempDir()
	mod := filepath.Join(dir, "go.mod")
	for content, want := range map[string]goVersion{
		"module m\n\ngo 1.26.0\n":                       {1, 26, 0},
		"module m\n\ngo 1.26.0\n\ntoolchain go1.26.9\n": {1, 26, 9},
	} {
		must(t, os.WriteFile(mod, []byte(content), 0o644))
		if got, err := minGoVersion(mod); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", content, got, err, want)
		}
	}
	must(t, os.WriteFile(mod, []byte("module m\n"), 0o644))
	if _, err := minGoVersion(mod); err == nil {
		t.Error("go.mod without go or toolchain line accepted")
	}
}

// TestToolchainTooOld: a resolved go older than go.mod's toolchain line
// is a gate fault. GOTOOLCHAIN=local keeps go from switching to it.
func TestToolchainTooOld(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.26.0\n\ntoolchain go1.999.0\n"), 0o644))
	t.Setenv("GOTOOLCHAIN", "local")
	if _, err := resolveToolchain(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "older than go.mod's minimum go1.999.0") {
		t.Fatalf("err %v", err)
	}
}
