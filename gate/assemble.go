package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// candidateFiles is the exact file set a candidate wrapper must consist
// of: gen 0's non-test .go files. The driver copies only these into the
// sandbox, and a mutant cannot create files (DESIGN.md sec 12).
var candidateFiles = []string{"doc.go", "hasher.go", "server.go", "tunables.go"}

// maxCandidateFileBytes caps each candidate file. Gen 0's largest is
// under 6 KiB; the cap only stops a runaway file, not a real mutation.
const maxCandidateFileBytes = 256 << 10

// fixedDirs are the repo packages copied into every build tree, non-test
// .go files only, top level only: internal/classifier/testvectors is left
// out, so the tree a candidate is built and booted from never holds the
// test-vector pool.
var fixedDirs = []string{"cmd/service", "internal/hashcore", "internal/classifier"}

// wrapperPkg is the candidate's package path inside the build tree.
const wrapperPkg = "internal/wrapper"

// readCandidate reads the candidate's files, failing (non-empty detail)
// unless dir holds exactly candidateFiles, each a regular file within
// maxCandidateFileBytes. Symlinks and anything else are rejected, never
// followed.
func readCandidate(dir string) (files map[string][]byte, detail string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("read candidate dir: %w", err)
	}
	var problems []string
	seen := make(map[string]bool)
	for _, e := range entries {
		name := e.Name()
		seen[name] = true
		switch {
		case !slices.Contains(candidateFiles, name):
			problems = append(problems, fmt.Sprintf("unexpected entry %q", name))
		case !e.Type().IsRegular(): // ReadDir types come from lstat
			problems = append(problems, fmt.Sprintf("%s: not a regular file (%v)", name, e.Type()))
		}
	}
	for _, name := range candidateFiles {
		if !seen[name] {
			problems = append(problems, "missing "+name)
		}
	}
	if len(problems) > 0 {
		return nil, strings.Join(problems, "; "), nil
	}

	files = make(map[string][]byte, len(candidateFiles))
	for _, name := range candidateFiles {
		b, problem, err := readRegular(filepath.Join(dir, name))
		if err != nil {
			return nil, "", err
		}
		if problem != "" {
			return nil, name + ": " + problem, nil
		}
		files[name] = b
	}
	return files, "", nil
}

// readRegular reads path, which must still be a regular file within the
// size cap when opened: O_NOFOLLOW refuses a symlink swapped in after the
// directory listing, O_NONBLOCK keeps a swapped-in FIFO from blocking the
// open, and the type and size are checked on the open descriptor.
func readRegular(path string) (b []byte, problem string, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, "not a regular file (symlink)", nil
		}
		return nil, "", fmt.Errorf("open candidate file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat candidate file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Sprintf("not a regular file (%v)", fi.Mode().Type()), nil
	}
	b, err = io.ReadAll(io.LimitReader(f, maxCandidateFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read candidate file: %w", err)
	}
	if len(b) > maxCandidateFileBytes {
		return nil, fmt.Sprintf("larger than %d bytes", maxCandidateFileBytes), nil
	}
	return b, "", nil
}

// assemble builds a fresh tree under a new temp directory: the repo's
// go.mod, go.sum and fixedDirs, plus the candidate's files as
// internal/wrapper (DESIGN.md sec 4: fixed core + evolved wrapper). The
// caller removes the returned directory.
func assemble(repoRoot string, candidate map[string][]byte) (tree string, err error) {
	tree, err = os.MkdirTemp("", "ek8s-gate-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tree)
		}
	}()
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := copyFile(filepath.Join(repoRoot, name), filepath.Join(tree, name)); err != nil {
			return "", err
		}
	}
	for _, dir := range fixedDirs {
		if err := copyGoFiles(filepath.Join(repoRoot, dir), filepath.Join(tree, dir)); err != nil {
			return "", err
		}
	}
	wdir := filepath.Join(tree, wrapperPkg)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		return "", err
	}
	for name, b := range candidate {
		if err := os.WriteFile(filepath.Join(wdir, name), b, 0o644); err != nil {
			return "", err
		}
	}
	return tree, nil
}

// copyGoFiles copies src's top-level non-test .go files into dst.
func copyGoFiles(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// toolchain is the go command the gate runs, with the facts it needs
// from the operator's environment resolved once.
type toolchain struct {
	goBin, modCache, buildCache, path string
}

// resolveToolchain asks the operator's go, run in repoRoot so any
// GOTOOLCHAIN switch to go.mod's toolchain or go line happens, for its
// GOROOT, version and caches. Every later go command runs that GOROOT's
// go directly, under toolchain.env. On a host whose go is older than
// go.mod asks for, this go env may first download that toolchain
// (checksum-verified) under the operator's GOTOOLCHAIN and GOPROXY. A
// resolved go older than go.mod's minimum (minGoVersion) is a gate
// fault: under GOTOOLCHAIN=local the toolchain line is only a
// preference, and the deployed binary must not get an older stdlib.
func resolveToolchain(ctx context.Context, repoRoot string) (toolchain, error) {
	min, err := minGoVersion(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return toolchain{}, err
	}
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOVERSION", "GOMODCACHE", "GOCACHE")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return toolchain{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var v struct{ GOROOT, GOVERSION, GOMODCACHE, GOCACHE string }
	if err := json.Unmarshal(out, &v); err != nil {
		return toolchain{}, fmt.Errorf("go env: %w", err)
	}
	if v.GOROOT == "" || v.GOMODCACHE == "" || v.GOCACHE == "" {
		return toolchain{}, fmt.Errorf("go env: empty GOROOT, GOMODCACHE or GOCACHE: %s", out)
	}
	// GOVERSION may carry experiments after a space: "go1.26.9 X:foo".
	release, _, _ := strings.Cut(v.GOVERSION, " ")
	got, ok := parseGoVersion(release)
	if !ok {
		return toolchain{}, fmt.Errorf("go env: unparseable GOVERSION %q", v.GOVERSION)
	}
	if compareGoVersions(got, min) < 0 {
		return toolchain{}, fmt.Errorf("resolved go %s (%s) is older than go.mod's minimum go%d.%d.%d", v.GOVERSION, v.GOROOT, min[0], min[1], min[2])
	}
	return toolchain{
		goBin:      filepath.Join(v.GOROOT, "bin", "go"),
		modCache:   v.GOMODCACHE,
		buildCache: v.GOCACHE,
		path:       os.Getenv("PATH"),
	}, nil
}

// env is the complete environment for a go command on a build tree,
// built from scratch, never inherited: inherited GOFLAGS could carry
// -toolexec, and the go env file could set CC, CGO_*, GOEXPERIMENT or
// GOCACHEPROG. With neither HOME nor XDG_CONFIG_HOME set, go cannot
// locate that file at all; GOENV=off ignores it regardless. GOPROXY=off
// means the module cache must already hold every module (run
// `go mod download` in the repo).
func (t toolchain) env(cgo bool) []string {
	cgoVal := "0"
	if cgo {
		cgoVal = "1"
	}
	return []string{
		"PATH=" + t.path,
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOOS=linux",
		"GOARCH=" + runtime.GOARCH,
		"CGO_ENABLED=" + cgoVal,
		"GOMODCACHE=" + t.modCache,
		"GOCACHE=" + t.buildCache,
	}
}

// goVersion is a release's major, minor and patch numbers. As in Go's own
// ordering, a language version (go1.27, patch -2) sorts below its
// pre-releases (go1.27rc1, patch -1), which sort below go1.27.0.
type goVersion [3]int

var goVersionRE = regexp.MustCompile(`^go(\d+)\.(\d+)(?:\.(\d+)|(rc|beta)\d+)?$`)

// parseGoVersion parses a go version string such as go1.26.9, go1.26
// (a language version, as in a go directive) or go1.27rc1.
func parseGoVersion(s string) (goVersion, bool) {
	m := goVersionRE.FindStringSubmatch(s)
	if m == nil {
		return goVersion{}, false
	}
	var v goVersion
	for i, part := range m[1:4] {
		if part == "" {
			v[i] = -2 // only the patch can be absent
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return goVersion{}, false
		}
		v[i] = n
	}
	if m[4] != "" {
		v[2] = -1
	}
	return v, true
}

func compareGoVersions(a, b goVersion) int { return slices.Compare(a[:], b[:]) }

// minGoVersion reads the go.mod at path and returns its toolchain line's
// version, or the go directive's when there is no toolchain line (go
// itself refuses a go older than the directive).
func minGoVersion(path string) (goVersion, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return goVersion{}, err
	}
	var directive, toolchainLine string
	for _, line := range strings.Split(string(b), "\n") {
		switch f := strings.Fields(line); {
		case len(f) == 2 && f[0] == "go":
			directive = "go" + f[1]
		case len(f) == 2 && f[0] == "toolchain":
			toolchainLine = f[1]
		}
	}
	want := directive
	if toolchainLine != "" {
		want = toolchainLine
	}
	v, ok := parseGoVersion(want)
	if !ok {
		return goVersion{}, fmt.Errorf("%s: no parseable toolchain or go line (%q)", path, want)
	}
	return v, nil
}
